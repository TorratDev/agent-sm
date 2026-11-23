package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type trackerFunc func(context.Context, string, string, string, any, any) error

func (f trackerFunc) call(ctx context.Context, system, method, path string, input, output any) error {
	return f(ctx, system, method, path, input, output)
}

func appliedManager(t *testing.T) (*manager, *fakeTrackers) {
	t.Helper()
	m, f := testManager(t)
	p, err := m.preview(context.Background(), simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(context.Background(), p.Digest); err != nil {
		t.Fatal(err)
	}
	return m, f
}

func existingPlan() WorkPlan {
	plan := simplePlan()
	plan.ID = "existing"
	for index := range plan.Items {
		plan.Items[index].Number = index + 10
		plan.Items[index].JiraKey = fmt.Sprintf("DEMO-%d", index+10)
		plan.Items[index].Body = ""
	}
	return plan
}

func TestManagementPreservesHumanFieldsAndRemovesApprovedEdges(t *testing.T) {
	m, f := appliedManager(t)
	ctx := context.Background()
	f.gh[10].Labels = []struct{ Name string }{{"human"}, {"old"}}
	annotatedBase := "- **Base branch**: `main` (parent **Story** #12)."
	f.gh[10].Body = strings.Replace(f.gh[10].Body, "- **Base branch**: `main`", annotatedBase, 1)
	if !strings.Contains(f.gh[10].Body, annotatedBase) {
		t.Fatal("base annotation fixture not inserted")
	}
	f.gh[10].Body += "\n\n## Notes\nHuman notes"
	desc, _ := appendADF(f.jira["DEMO-10"].Fields.Description, "Human ADF content")
	f.jira["DEMO-10"].Fields.Description, _ = json.Marshal(desc)
	beforeADF := append([]byte{}, f.jira["DEMO-10"].Fields.Description...)
	edit := WorkPlan{ID: "detach", Items: []WorkItem{{
		ID: "root", Number: 10, JiraKey: "DEMO-10", Kind: "story", Title: "Story",
		RemoveParent: true, Labels: []string{"managed"}, RemoveLabels: []string{"old"},
	}}}
	p, err := m.preview(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeADF, []byte(f.jira["DEMO-10"].Fields.Description)) {
		t.Fatal("human ADF was replaced")
	}
	if !strings.Contains(f.gh[10].Body, "Human notes") {
		t.Fatal("human issue content lost")
	}
	if !strings.Contains(f.gh[10].Body, annotatedBase) {
		t.Fatal("base branch annotation lost")
	}
	if f.jira["DEMO-10"].Fields.Parent != nil {
		t.Fatal("Jira parent not detached")
	}
	names := map[string]bool{}
	for _, l := range f.gh[10].Labels {
		names[l.Name] = true
	}
	if !names["human"] || !names["managed"] || names["old"] {
		t.Fatal("labels not preserved/updated", names)
	}
	remove := existingPlan()
	remove.Items[2].DependsOn, remove.Items[2].RemoveDependencies = nil, []string{"one"}
	p, err = m.preview(ctx, remove)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	if len(f.deps[12]) != 0 || len(f.jira["DEMO-12"].Fields.Links) != 0 {
		t.Fatal("dependency removal not mirrored")
	}
}

func TestGitHubBodyPreservesBaseAnnotation(t *testing.T) {
	for _, tc := range []struct{ body, line string }{
		{"- **Base branch**: `main` (parent **Story** #12).\n\n## Jira\nOther notes", "- **Base branch**: `main` (parent **Story** #12)."},
		{"## Jira\n- **Base branch:** **`main`** (parent **Story** #12).", "- **Base branch:** **`main`** (parent **Story** #12)."},
		{"## Jira\n- **Base branch :** `main` (parent **Story** #12).", "- **Base branch :** `main` (parent **Story** #12)."},
		{"- **Base branch**: `main` (parent **Story** #12).", "- **Base branch**: `main` (parent **Story** #12)."},
		{"## Jira\n**Base branch: `main` (parent Story #12).**", "**Base branch: `main` (parent Story #12).**"},
	} {
		for _, base := range []string{"main", "feature"} {
			got := githubBody(tc.body, "https://jira.example/browse/DEMO-1", base, "agent-sm:test:item")
			line := strings.Replace(tc.line, "`main`", "`"+base+"`", 1)
			if strings.Count(got, line) != 1 {
				t.Errorf("base annotation changed from %q:\n%s", tc.body, got)
			}
			meta, err := issueMetadata(got)
			if err != nil || meta.Base != "origin/"+base {
				t.Errorf("rewritten metadata: %+v %v", meta, err)
			}
			if again := githubBody(got, "https://jira.example/browse/DEMO-1", base, "agent-sm:test:item"); again != got {
				t.Errorf("rewrite is not idempotent:\n%s\n%s", got, again)
			}
		}
	}
}

func TestManagementRefusesTransitiveRemoteCyclesBeforeWrites(t *testing.T) {
	m, f := appliedManager(t)
	f.deps[11] = []int{12}
	before := writes(f.calls)
	if _, err := m.preview(context.Background(), existingPlan()); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatal(err)
	}
	if writes(f.calls) != before {
		t.Fatal("cycle validation mutated a tracker")
	}
}

func TestManagementPartialApplyProtectsHumanEdits(t *testing.T) {
	m, f := testManager(t)
	ctx := context.Background()
	plan := simplePlan()
	plan.Items = plan.Items[:1]
	p, err := m.preview(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	f.failAfter = "github PATCH /repos/owner/repo/issues/10"
	if err := m.apply(ctx, p.Digest); err == nil {
		t.Fatal("lost update response not reported")
	}
	f.gh[10].Title = "Human changed this title"
	before := writes(f.calls)
	if err := m.apply(ctx, p.Digest); err == nil || !strings.Contains(err.Error(), "title changed") {
		t.Fatal(err)
	}
	if writes(f.calls) != before || f.gh[10].Title != "Human changed this title" {
		t.Fatal("human edit overwritten during recovery")
	}
}

func TestManagementStartupResumesApprovedButNotUnapprovedPlans(t *testing.T) {
	m, f := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.resume(ctx); err != nil {
		t.Fatal(err)
	}
	if writes(f.calls) != 0 {
		t.Fatal("unapproved plan resumed")
	}
	f.failAfter = "jira POST /issue"
	if err := m.apply(ctx, p.Digest); err == nil {
		t.Fatal("missing failure")
	}
	restarted := *m
	restarted.state = managementState{}
	if err := restarted.resume(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.gh) != 3 || len(f.jira) != 4 {
		t.Fatal("recovery duplicated or omitted issues")
	}
}

func TestManagementIntentBeforePOSTIsDurable(t *testing.T) {
	m, _ := testManager(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.locked(context.Background(), func() error {
		return m.op(canceled, "create", true, func(bool) (any, error) { t.Fatal("POST ran after cancellation"); return nil, nil })
	}); err == nil {
		t.Fatal("missing cancellation")
	}
	if err := m.locked(context.Background(), func() error {
		return m.op(context.Background(), "create", true, func(uncertain bool) (any, error) {
			if !uncertain {
				t.Fatal("lost persisted intent")
			}
			return nil, fmt.Errorf("manual inspection required")
		})
	}); err == nil {
		t.Fatal("ambiguous create treated as successful")
	}
}

func TestManagementMissingDateAndLaterStates(t *testing.T) {
	m, f := appliedManager(t)
	ctx := context.Background()
	if err := m.register(ctx, 10, true, 0, "working"); err != nil {
		t.Fatal(err)
	}
	f.dates["DEMO-10"] = "2024-12-01"
	if err := m.reconcile(ctx, 10, true); err != nil {
		t.Fatal(err)
	}
	if f.dates["DEMO-10"] != "2024-12-01" {
		t.Fatal("existing Start date overwritten")
	}
	f.jira["DEMO-10"].Fields.Status.Name = "Ready for Deploy"
	if err := m.reconcile(ctx, 10, true); err != nil {
		t.Fatal(err)
	}
	if f.jira["DEMO-10"].Fields.Status.Name != "Ready for Deploy" {
		t.Fatal("later Jira state moved backwards")
	}
	// Losing the event must be visible, not replaced with the current time.
	delete(f.dates, "DEMO-10")
	f.timeline[10] = nil
	if err := m.reconcile(ctx, 10, true); err == nil || !strings.Contains(err.Error(), "assignment event unavailable") {
		t.Fatal(err)
	}
	if f.dates["DEMO-10"] != "" {
		t.Fatal("invented Start date")
	}
	if err := m.deactivate(ctx, 10, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcile(ctx, 10, true); err != nil {
		t.Fatal("stopped run kept assigning/date-writing:", err)
	}
}

func TestManagementMetadataRejectsBeforeCreatingAnything(t *testing.T) {
	m, f := testManager(t)
	m.api = trackerFunc(func(ctx context.Context, system, method, path string, input, out any) error {
		if strings.HasPrefix(path, "/issue/createmeta/DEMO/issuetypes/2?") {
			b := []byte(`{"total":1,"fields":[{"fieldId":"customfield_99","required":true}]}`)
			return json.Unmarshal(b, out)
		}
		return f.call(ctx, system, method, path, input, out)
	})
	if _, err := m.preview(context.Background(), simplePlan()); err == nil || !strings.Contains(err.Error(), "customfield_99") {
		t.Fatal(err)
	}
	for _, value := range []string{"null", `""`, "[]", "{}", `"configured value"`} {
		m.profile.Jira.CreateFields = map[string]map[string]json.RawMessage{"Story": {"customfield_99": json.RawMessage(value)}}
		_, err := m.preview(context.Background(), simplePlan())
		if (err == nil) != (value == `"configured value"`) {
			t.Fatalf("required value %s: %v", value, err)
		}
	}
	if writes(f.calls) != 0 {
		t.Fatal("missing required field caused partial creation")
	}
}

func TestManagementEditsParentsBeforeCreatingChildren(t *testing.T) {
	m, f := appliedManager(t)
	f.jira["DEMO-10"].Fields.IssueType.ID = "1"
	f.jira["DEMO-10"].Fields.IssueType.Name = "Epic"
	plan := WorkPlan{ID: "repair-parent", Items: []WorkItem{
		{ID: "parent", Number: 10, JiraKey: "DEMO-10", Kind: "story", Title: "Story"},
		{ID: "child", Kind: "task", Parent: "parent", Title: "New task"},
	}}
	m.api = trackerFunc(func(ctx context.Context, system, method, path string, input, out any) error {
		if system == "jira" && method == "POST" && path == "/issue" && f.jira["DEMO-10"].Fields.IssueType.Name != "Story" {
			return fmt.Errorf("child created before parent migration")
		}
		return f.call(ctx, system, method, path, input, out)
	})
	p, err := m.preview(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(context.Background(), p.Digest); err != nil {
		t.Fatal(err)
	}
	if f.jira["DEMO-13"].Fields.Parent.Key != "DEMO-10" {
		t.Fatal("wrong new parent")
	}
}

func TestManagementNativeTypeWithoutAssignee(t *testing.T) {
	m, f := testManager(t)
	plan := simplePlan()
	plan.Items[0].GitHubType = "Unavailable"
	if _, err := m.preview(context.Background(), plan); err == nil {
		t.Fatal("type skipped when no assignee")
	}
	if writes(f.calls) != 0 {
		t.Fatal("invalid type caused writes")
	}
	plan.Items[0].GitHubType = "Task"
	p, err := m.preview(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(context.Background(), p.Digest); err != nil {
		t.Fatal(err)
	}
	if f.gh[10].Type == nil || f.gh[10].Type.Name != "Task" {
		t.Fatal("native GitHub type lost")
	}
}

func TestManagementIssueContextKeepsHumanComments(t *testing.T) {
	m, f := appliedManager(t)
	ctx := context.Background()
	if err := m.register(ctx, 10, true, 0, "working"); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcile(ctx, 10, true); err != nil {
		t.Fatal(err)
	}
	human := githubComment{ID: 2, Body: "Please fix this: <!-- agent-sm:progress:10 -->"}
	human.User.Login = "human"
	f.comments[human.ID] = human
	c := &Ctx{Issue: 10, Body: "Requirements"}
	if err := m.issueContext(ctx, c); err != nil {
		t.Fatal(err)
	}
	if c.Comments != "\n\n"+human.Body {
		t.Fatal("human comment lost or service noise included", c.Comments)
	}
	human.Body = strings.Repeat("x", 1<<20)
	f.comments[human.ID] = human
	if err := m.issueContext(ctx, c); err == nil {
		t.Fatal("unbounded issue context accepted")
	}
}

func TestManagementServiceEnvironmentPinsScope(t *testing.T) {
	t.Setenv("GH_HOST", "unexpected.example")
	t.Setenv("GH_REPO", "other/repo")
	t.Setenv("GH_TOKEN", "service-test-token")
	env := strings.Join(serviceEnv(), "\n")
	if strings.Contains(env, "GH_HOST=") || strings.Contains(env, "GH_REPO=") || !strings.Contains(env, "GH_TOKEN=service-test-token") {
		t.Fatal("service scope overrides retained or authentication removed")
	}
}

func TestRemoteTrackerConditionalReadsAndRateLimits(t *testing.T) {
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		user, token, ok := r.BasicAuth()
		if !ok || user != "user" || token != "token" {
			t.Error("missing Jira credentials")
		}
		if r.URL.Path == "/rest/api/3/limited" {
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(429)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"key":"DEMO-1"}`))
	}))
	defer srv.Close()
	old := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = old })
	t.Setenv("JIRA_USER_EMAIL", "user")
	t.Setenv("JIRA_API_TOKEN", "token")
	client := &remoteTrackers{profile: Profile{Jira: JiraConfig{APIBase: srv.URL}}}
	var result struct{ Key string }
	for i := 0; i < 2; i++ {
		result.Key = ""
		if err := client.call(context.Background(), "jira", "GET", "/issue/DEMO-1", nil, &result); err != nil || result.Key != "DEMO-1" {
			t.Fatal(err)
		}
	}
	if requests != 2 {
		t.Fatal("unexpected request count")
	}
	err := client.call(context.Background(), "jira", "GET", "/limited", nil, &result)
	if limit, ok := err.(retryError); !ok || limit.after != 10*time.Minute {
		t.Fatal(err)
	}
}

func TestRemoteGitHubUsesTypedInputAndChecksResponses(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
 *"/missing"*) printf 'HTTP/1.1 404 Not Found\r\nContent-Length: 2\r\n\r\n{}'; exit 1;;
 *"--input -"*) read -r payload; [ "$payload" = '{"title":"$(do-not-run)"}' ] || exit 3;;
esac
printf 'HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\n{"id":1}'
`
	writeFile(t, filepath.Join(dir, "gh"), script, 0700)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := &remoteTrackers{repo: dir, profile: Profile{Management: ManagementConfig{Host: "github.com"}}}
	var actual struct{ ID int }
	if err := client.call(context.Background(), "github", "POST", "/test", map[string]string{"title": "$(do-not-run)"}, &actual); err != nil || actual.ID != 1 {
		t.Fatal(err)
	}

	err := client.call(context.Background(), "github", "GET", "/missing", nil, nil)
	if status, ok := err.(trackerError); !ok || status.Status != 404 {
		t.Fatal(err)
	}
}

func TestManagementHTTPPreviewApplyRoundTrip(t *testing.T) {
	m, f := testManager(t)
	mux := http.NewServeMux()
	managementRoutes(mux, m, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("X-SM-Token") != "secret" || r.Method != "POST" {
			http.Error(w, "forbidden", 403)
			return false
		}
		return true
	})
	post := func(path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", path, bytes.NewReader(b))
		req.Header.Set("X-SM-Token", "secret")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	w := post("/api/management/preview", simplePlan())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var p Preview
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Scope != m.scope || len(p.Operations) == 0 || !digestPattern.MatchString(p.Digest) || writes(f.calls) != 0 {
		t.Fatal("invalid preview response")
	}
	w = post("/api/management/apply", map[string]string{"digest": p.Digest})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var state struct {
		Enabled bool
		State   managementState
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || len(state.State.Items) != 3 || !state.State.Previews[p.Digest].Complete {
		t.Fatal("invalid apply state", err)
	}
	before := writes(f.calls)
	w = post("/api/management/apply", map[string]string{"digest": p.Digest})
	if w.Code != 200 || writes(f.calls) != before {
		t.Fatal("API retry duplicated operations")
	}
}

func TestManagementCancelPreservesPartialResources(t *testing.T) {
	m, f := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	f.failAfter = "github POST /repos/owner/repo/issues"
	if err := m.apply(ctx, p.Digest); err == nil {
		t.Fatal("missing failure")
	}
	retry, err := m.preview(ctx, simplePlan())
	if err != nil || retry.Digest != p.Digest || !retry.Started {
		t.Fatal("in-flight artifact not preserved", err)
	}
	before := writes(f.calls)
	if err := m.cancel(ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	if err := m.resume(ctx); err != nil {
		t.Fatal(err)
	}
	if writes(f.calls) != before || len(f.gh) != 1 {
		t.Fatal("cancellation mutated trackers or resumed the batch")
	}
	if err := m.apply(ctx, p.Digest); err == nil {
		t.Fatal("cancelled approval reused")
	}
	replacement := simplePlan()
	replacement.ID = "adopt-cancelled"
	replacement.Items[0].Number = 10
	adopted, err := m.preview(ctx, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(ctx, adopted.Digest); err != nil {
		t.Fatal(err)
	}
	if len(f.gh) != 3 || len(f.jira) != 4 {
		t.Fatal("replacement duplicated partial resources")
	}
}

func TestManagementCollectionsPageAndFailClosed(t *testing.T) {
	m, _ := testManager(t)
	pages := 0
	m.api = trackerFunc(func(ctx context.Context, system, method, path string, input, out any) error {
		pages++
		count := 100
		if strings.Contains(path, "page=2") {
			count = 1
		}
		values := make([]issueSnapshot, count)
		b, _ := json.Marshal(values)
		return json.Unmarshal(b, out)
	})
	items, err := ghPages[issueSnapshot](context.Background(), m, "/issues")
	if err != nil || len(items) != 101 || pages != 2 {
		t.Fatal(len(items), pages, err)
	}
	m.api = trackerFunc(func(ctx context.Context, system, method, path string, input, out any) error {
		return json.Unmarshal([]byte("null"), out)
	})
	if _, err := ghPages[issueSnapshot](context.Background(), m, "/dependencies/blocked_by"); err == nil {
		t.Fatal("unknown dependencies treated as empty")
	}
}

func TestManagementApprovalTamperingAndBackoff(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.locked(ctx, func() error {
		m.state.Previews[p.Digest].Plan.Items[0].Title = "Changed after approval"
		return m.save()
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.apply(ctx, p.Digest); err == nil || !strings.Contains(err.Error(), "altered") {
		t.Fatal("tampered approval accepted", err)
	}
	m, f := appliedManager(t)
	if err := m.register(ctx, 10, true, 0, "active"); err != nil {
		t.Fatal(err)
	}
	m.api = trackerFunc(func(ctx context.Context, system, method, path string, input, out any) error {
		if system == "jira" && path == "/issue/DEMO-10" {
			return retryError{429, 30 * time.Minute}
		}
		return f.call(ctx, system, method, path, input, out)
	})
	if err := m.reconcile(ctx, 10, true); err == nil {
		t.Fatal("rate limit hidden")
	}
	if err := m.locked(ctx, func() error {
		if time.Until(m.state.Items["10"].Next) < 29*time.Minute {
			t.Fatal("Retry-After ignored")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
