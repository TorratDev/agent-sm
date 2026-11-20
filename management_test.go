package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTrackers struct {
	gh               map[int]*issueSnapshot
	jira             map[string]*jiraSnapshot
	parents          map[int]int
	deps             map[int][]int
	comments         map[int64]githubComment
	jiraComments     map[string]jiraComment
	dates            map[string]string
	timeline         map[int][]timelineEvent
	prs              map[int]linkedPR
	calls            []string
	nextGH, nextJira int
	failAfter        string
	activeWorkflow   bool
}

func testManager(t *testing.T) (*manager, *fakeTrackers) {
	t.Helper()
	p := defaultProfile("copilot")
	p.Management = ManagementConfig{Mode: "service", Host: "github.com", Repo: "owner/repo", WritersDisabled: true,
		Identities: map[string]string{"dev": "account"}, GitHubAssignee: "dev"}
	p.Jira = JiraConfig{Mode: "service", Site: "https://jira.example", Project: "DEMO", Epic: "DEMO-1",
		EpicType: "Epic", StoryType: "Story", TaskType: "Sub-task", InitialStatuses: []string{"To Do"},
		InProgress: "In Progress", InReview: "In Review", Delivered: "Ready for Deploy", StartDateField: "customfield_10"}
	f := &fakeTrackers{gh: map[int]*issueSnapshot{}, jira: map[string]*jiraSnapshot{}, parents: map[int]int{}, deps: map[int][]int{},
		comments: map[int64]githubComment{}, jiraComments: map[string]jiraComment{}, dates: map[string]string{},
		timeline: map[int][]timelineEvent{}, prs: map[int]linkedPR{}, nextGH: 10, nextJira: 10}
	epic := &jiraSnapshot{Key: "DEMO-1"}
	epic.Fields.Project.Key, epic.Fields.IssueType.ID, epic.Fields.IssueType.Name = "DEMO", "1", "Epic"
	f.jira[epic.Key] = epic
	m := &manager{dir: t.TempDir(), repo: t.TempDir(), scope: "https://github.com/owner/repo|https://jira.example|DEMO", profile: p, api: f}
	return m, f
}

func (f *fakeTrackers) call(ctx context.Context, system, method, path string, input, output any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := system + " " + method + " " + strings.Split(path, "?")[0]
	f.calls = append(f.calls, key)
	var result any
	var data map[string]any
	if input != nil {
		b, _ := json.Marshal(input)
		json.Unmarshal(b, &data)
	}
	u, _ := url.Parse(path)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if system == "github" {
		switch {
		case path == "/user":
			result = map[string]string{"login": "service"}
		case path == "/graphql":
			variables := data["variables"].(map[string]any)
			pr := f.prs[int(variables["n"].(float64))]
			result = map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"title": pr.Title, "body": pr.Body, "state": pr.State, "isDraft": pr.IsDraft, "baseRefName": pr.BaseRefName,
				"url": pr.URL, "mergedAt": pr.MergedAt, "closingIssuesReferences": map[string]any{"nodes": pr.ClosingIssuesReferences,
					"pageInfo": map[string]any{"hasNextPage": false}}}}}}
		case path == "/repos/owner/repo":
			result = map[string]any{"has_issues": true, "permissions": map[string]bool{"push": true}}
		case strings.Contains(path, "/actions/workflows/"):
			state := "disabled_manually"
			if f.activeWorkflow {
				state = "active"
			}
			result = map[string]string{"state": state}
		case strings.Contains(path, "/assignees/dev"):
			result = nil
		case strings.Contains(path, "/labels/"):
			result = map[string]string{"name": parts[len(parts)-1]}
		case path == "/repos/owner/repo/issue-types":
			result = []map[string]string{{"name": "Task"}}
		case len(parts) == 4 && parts[3] == "issues":
			if method == "POST" {
				n := f.nextGH
				f.nextGH++
				s := &issueSnapshot{ID: int64(n + 1000), Number: n, URL: fmt.Sprintf("https://github.com/owner/repo/issues/%d", n),
					Title: data["title"].(string), Body: data["body"].(string), State: "open"}
				if name, ok := data["type"].(string); ok {
					s.Type = &struct{ Name string }{name}
				}
				if labels, ok := data["labels"].([]any); ok {
					for _, label := range labels {
						s.Labels = append(s.Labels, struct{ Name string }{label.(string)})
					}
				}
				f.gh[n], result = s, s
			} else {
				issues := []issueSnapshot{}
				for _, s := range f.gh {
					issues = append(issues, *s)
				}
				result = issues
			}
		case len(parts) == 6 && parts[4] == "comments":
			id, _ := strconv.ParseInt(parts[5], 10, 64)
			c, ok := f.comments[id]
			if !ok {
				return trackerError{404}
			}
			if method == "PATCH" {
				c.Body = data["body"].(string)
				f.comments[id] = c
			}
			result = c
		case len(parts) >= 5 && parts[3] == "issues":
			n, _ := strconv.Atoi(parts[4])
			s := f.gh[n]
			if s == nil {
				return trackerError{404}
			}
			suffix := strings.Join(parts[5:], "/")
			switch suffix {
			case "":
				if method == "PATCH" {
					if v, ok := data["title"].(string); ok {
						s.Title = v
					}
					if v, ok := data["body"].(string); ok {
						s.Body = v
					}
					if v, ok := data["state"].(string); ok {
						s.State = v
					}
					if name, ok := data["type"].(string); ok {
						s.Type = &struct{ Name string }{name}
					}
					if labels, ok := data["labels"].([]any); ok {
						s.Labels = nil
						for _, label := range labels {
							s.Labels = append(s.Labels, struct{ Name string }{label.(string)})
						}
					}
				}
				result = s
			case "parent":
				if f.parents[n] == 0 {
					return trackerError{404}
				}
				result = f.gh[f.parents[n]]
			case "sub_issues":
				childID := int64(data["sub_issue_id"].(float64))
				for num, issue := range f.gh {
					if issue.ID == childID {
						f.parents[num] = n
					}
				}
				result = map[string]bool{"ok": true}
			case "sub_issue":
				childID := int64(data["sub_issue_id"].(float64))
				for num, issue := range f.gh {
					if issue.ID == childID {
						delete(f.parents, num)
					}
				}
			case "dependencies/blocked_by":
				if method == "POST" {
					blockID := int64(data["issue_id"].(float64))
					for num, issue := range f.gh {
						if issue.ID == blockID {
							f.deps[n] = append(f.deps[n], num)
						}
					}
				}
				list := []issueSnapshot{}
				for _, num := range f.deps[n] {
					list = append(list, *f.gh[num])
				}
				result = list
			case "timeline":
				result = f.timeline[n]
				if f.timeline[n] == nil {
					result = []timelineEvent{}
				}
			case "assignees":
				login := data["assignees"].([]any)[0].(string)
				s.Assignees = append(s.Assignees, struct{ Login string }{login})
				e := timelineEvent{Event: "assigned", Created: "2025-01-02T03:04:05Z"}
				e.Assignee.Login = login
				f.timeline[n] = append(f.timeline[n], e)
			case "comments":
				if method == "POST" {
					id := int64(len(f.comments) + 1)
					c := githubComment{ID: id, Body: data["body"].(string)}
					c.User.Login = "service"
					f.comments[id], result = c, c
				} else {
					comments := []githubComment{}
					for _, c := range f.comments {
						comments = append(comments, c)
					}
					result = comments
				}
			default:
				if strings.HasPrefix(suffix, "dependencies/blocked_by/") && method == "DELETE" {
					id, _ := strconv.ParseInt(strings.TrimPrefix(suffix, "dependencies/blocked_by/"), 10, 64)
					var keep []int
					for _, num := range f.deps[n] {
						if f.gh[num].ID != id {
							keep = append(keep, num)
						}
					}
					f.deps[n] = keep
				} else {
					return fmt.Errorf("unhandled fake GitHub %s", key)
				}
			}
		default:
			return fmt.Errorf("unhandled fake GitHub %s", key)
		}
	} else {
		switch {
		case parts[0] == "mypermissions":
			perms := map[string]any{}
			for _, name := range strings.Split(u.Query().Get("permissions"), ",") {
				perms[name] = map[string]bool{"havePermission": true}
			}
			result = map[string]any{"permissions": perms}
		case path == "/myself":
			result = map[string]string{"accountId": "jira-service"}
		case parts[0] == "user":
			result = []any{map[string]any{"accountId": "account", "active": true}}
		case strings.HasPrefix(path, "/issue/createmeta/DEMO/issuetypes"):
			if len(parts) == 4 {
				result = map[string]any{"total": 3, "issueTypes": []any{
					map[string]any{"id": "1", "name": "Epic"}, map[string]any{"id": "2", "name": "Story"},
					map[string]any{"id": "3", "name": "Sub-task", "subtask": true}}}
			} else {
				result = map[string]any{"total": 2, "fields": []any{map[string]any{"fieldId": "summary", "required": true}, map[string]any{"fieldId": "description"}}}
			}
		case path == "/issue" && method == "POST":
			fields := data["fields"].(map[string]any)
			id := f.nextJira
			f.nextJira++
			jiraKey := fmt.Sprintf("DEMO-%d", id)
			s := &jiraSnapshot{Key: jiraKey, ID: strconv.Itoa(id)}
			s.Fields.Project.Key = "DEMO"
			s.Fields.Summary = fields["summary"].(string)
			s.Fields.Description, _ = json.Marshal(fields["description"])
			s.Fields.IssueType.ID = fields["issuetype"].(map[string]any)["id"].(string)
			s.Fields.IssueType.Name = map[string]string{"1": "Epic", "2": "Story", "3": "Sub-task"}[s.Fields.IssueType.ID]
			s.Fields.Status.Name = "To Do"
			if p, ok := fields["parent"].(map[string]any); ok {
				s.Fields.Parent = &struct{ Key string }{p["key"].(string)}
			}
			f.jira[jiraKey], result = s, map[string]string{"key": jiraKey}
		case path == "/issueLinkType":
			result = map[string]any{"issueLinkTypes": []any{map[string]string{"id": "100", "name": "Blocks", "inward": "is blocked by", "outward": "blocks"}}}
		case path == "/issueLink" && method == "POST":
			blocker := data["inwardIssue"].(map[string]any)["key"].(string)
			dependent := data["outwardIssue"].(map[string]any)["key"].(string)
			link := jiraLink{ID: "500"}
			link.Type.ID, link.Type.Outward, link.Type.Inward = "100", "blocks", "is blocked by"
			link.InwardIssue = &struct{ Key string }{blocker}
			f.jira[dependent].Fields.Links = append(f.jira[dependent].Fields.Links, link)
		case strings.HasPrefix(path, "/issueLink/") && method == "DELETE":
			for _, s := range f.jira {
				s.Fields.Links = nil
			}
		case pathHasPrefix(parts, "search", "jql"):
			matches := []*jiraSnapshot{}
			for _, s := range f.jira {
				if hasBacklink(s.Fields.Description, f.searchURL(u.Query().Get("jql"))) {
					matches = append(matches, s)
				}
			}
			result = map[string]any{"issues": matches, "isLast": true}
		case len(parts) >= 2 && parts[0] == "issue":
			s := f.jira[parts[1]]
			if s == nil {
				return trackerError{404}
			}
			switch {
			case len(parts) == 2:
				if method == "PUT" {
					fields := data["fields"].(map[string]any)
					if v, ok := fields["summary"].(string); ok {
						s.Fields.Summary = v
					}
					if v, ok := fields["issuetype"].(map[string]any); ok {
						name := v["name"].(string)
						s.Fields.IssueType.Name = name
						s.Fields.IssueType.ID = map[string]string{"Epic": "1", "Story": "2", "Sub-task": "3"}[name]
					}
					if v, ok := fields["description"]; ok {
						s.Fields.Description, _ = json.Marshal(v)
					}
					if v, ok := fields["parent"].(map[string]any); ok {
						s.Fields.Parent = &struct{ Key string }{v["key"].(string)}
					}
					if v, ok := fields["parent"]; ok && v == nil {
						s.Fields.Parent = nil
					}
					if v, ok := fields["assignee"].(map[string]any); ok {
						s.Fields.Assignee = &struct {
							AccountID string `json:"accountId"`
						}{v["accountId"].(string)}
					}
					if v, ok := fields["customfield_10"].(string); ok {
						f.dates[s.Key] = v
					}
				}
				b, _ := json.Marshal(s)
				var raw map[string]any
				json.Unmarshal(b, &raw)
				raw["fields"].(map[string]any)["customfield_10"] = nil
				if date := f.dates[s.Key]; date != "" {
					raw["fields"].(map[string]any)["customfield_10"] = date
				}
				result = raw
			case parts[2] == "editmeta":
				fields := map[string]any{}
				for _, name := range []string{"summary", "description", "parent", "assignee", "issuetype", "customfield_10"} {
					fields[name] = map[string]any{"operations": []string{"set"}, "schema": map[string]string{"type": "date"}}
				}
				result = map[string]any{"fields": fields}
			case parts[2] == "transitions":
				if method == "POST" {
					s.Fields.Status.Name = data["transition"].(map[string]any)["id"].(string)
				}
				var transitions []any
				for _, status := range []string{"In Progress", "In Review", "Ready for Deploy"} {
					transitions = append(transitions, map[string]any{"id": status, "to": map[string]string{"name": status}})
				}
				result = map[string]any{"transitions": transitions}
			case parts[2] == "comment":
				if len(parts) == 3 && method == "POST" {
					id := strconv.Itoa(len(f.jiraComments) + 1)
					c := jiraComment{ID: id}
					c.Author.AccountID = "jira-service"
					c.Body, _ = json.Marshal(data["body"])
					f.jiraComments[id], result = c, c
				} else if len(parts) == 3 {
					comments := []jiraComment{}
					for _, c := range f.jiraComments {
						comments = append(comments, c)
					}
					result = map[string]any{"total": len(comments), "comments": comments}
				} else {
					c := f.jiraComments[parts[3]]
					if method == "PUT" {
						c.Body, _ = json.Marshal(data["body"])
						f.jiraComments[c.ID] = c
					}
					result = c
				}
			default:
				return fmt.Errorf("unhandled fake Jira %s", key)
			}
		default:
			return fmt.Errorf("unhandled fake Jira %s", key)
		}
	}
	if f.failAfter == key {
		f.failAfter = ""
		return fmt.Errorf("simulated lost response after remote commit")
	}
	if output != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, output)
	}
	return nil
}

func (f *fakeTrackers) searchURL(jql string) string {
	for _, s := range f.gh {
		if strings.Contains(jql, s.URL) {
			return s.URL
		}
	}
	return ""
}

func pathHasPrefix(parts []string, first, second string) bool {
	return len(parts) > 1 && parts[0] == first && parts[1] == second
}

func simplePlan() WorkPlan {
	return WorkPlan{ID: "batch", Items: []WorkItem{
		{ID: "story", Title: "Story", Body: "Requirements", Kind: "story"},
		{ID: "one", Title: "First task", Body: "First acceptance", Kind: "task", Parent: "story"},
		{ID: "two", Title: "Second task", Body: "Second acceptance", Kind: "task", Parent: "story", DependsOn: []string{"one"}},
	}}
}

func writes(calls []string) int {
	n := 0
	for _, call := range calls {
		if !strings.Contains(call, " GET ") && call != "github POST /graphql" {
			n++
		}
	}
	return n
}

func TestManagementApplyAndCrashRecovery(t *testing.T) {
	for _, fail := range []string{"", "github POST /repos/owner/repo/issues", "jira POST /issue"} {
		t.Run(fail, func(t *testing.T) {
			m, f := testManager(t)
			ctx := context.Background()
			p, err := m.preview(ctx, simplePlan())
			if err != nil {
				t.Fatal(err)
			}
			if writes(f.calls) != 0 {
				t.Fatal("preview mutated trackers")
			}
			f.failAfter = fail
			if err := m.apply(ctx, p.Digest); err != nil {
				if fail == "" {
					t.Fatal(err)
				}
				restarted := *m
				restarted.state = managementState{}
				m = &restarted
				if err := m.apply(ctx, p.Digest); err != nil {
					t.Fatal("recovery:", err)
				}
			}
			if len(f.gh) != 3 || len(f.jira) != 4 {
				t.Fatalf("duplicate/missing resources: %d %d", len(f.gh), len(f.jira))
			}
			if f.parents[11] != 10 || f.parents[12] != 10 {
				t.Fatal("missing native hierarchy")
			}
			if f.jira["DEMO-11"].Fields.Parent.Key != "DEMO-10" || f.jira["DEMO-10"].Fields.Parent.Key != "DEMO-1" {
				t.Fatal("incorrect Jira hierarchy")
			}
			if len(f.deps[12]) != 1 || f.deps[12][0] != 11 || f.jira["DEMO-12"].Fields.Links[0].InwardIssue.Key != "DEMO-11" {
				t.Fatal("dependency direction mismatch")
			}
			before := writes(f.calls)
			if err := m.apply(ctx, p.Digest); err != nil {
				t.Fatal(err)
			}
			if writes(f.calls) != before {
				t.Fatal("completed apply wrote again")
			}
			for _, s := range f.gh {
				meta, err := issueMetadata(s.Body)
				if err != nil || meta.Base != "origin/main" || !hasBacklink(f.jira[meta.JiraKey].Fields.Description, s.URL) {
					t.Fatal("counterpart readback failed", err)
				}
			}
		})
	}
}

func TestManagementApprovalAndRemoteDrift(t *testing.T) {
	m, f := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(ctx, strings.Repeat("0", 64)); err == nil {
		t.Fatal("unknown approval accepted")
	}
	if writes(f.calls) != 0 {
		t.Fatal("unknown approval wrote")
	}
	if err := m.apply(ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	edit := WorkPlan{ID: "edit", Items: []WorkItem{{ID: "existing", Number: 10, JiraKey: "DEMO-10", Kind: "story", Title: "Changed", Body: "Explicit replacement"}}}
	preview, err := m.preview(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	f.gh[10].Body += "\nHuman edit"
	before := writes(f.calls)
	if err := m.apply(ctx, preview.Digest); err == nil || !strings.Contains(err.Error(), "remote state changed") {
		t.Fatal(err)
	}
	if writes(f.calls) != before {
		t.Fatal("stale preview wrote")
	}
	m.profile.Management.Workflows = []string{"sync.yml"}
	f.activeWorkflow = true
	if _, err := m.preview(ctx, WorkPlan{ID: "another", Items: simplePlan().Items}); err == nil {
		t.Fatal("competing writer accepted")
	}
}

func TestManagementUnknownCreateDoesNotRetry(t *testing.T) {
	m, f := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	f.failAfter = "github POST /repos/owner/repo/issues"
	if err := m.apply(ctx, p.Digest); err == nil {
		t.Fatal("lost response not reported")
	}
	delete(f.gh, 10)
	before := writes(f.calls)
	if err := m.apply(ctx, p.Digest); err == nil || !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatal(err)
	}
	if writes(f.calls) != before {
		t.Fatal("ambiguous create retried")
	}
}

func TestWorkPlanValidation(t *testing.T) {
	for _, mutate := range []func(*WorkPlan){
		func(p *WorkPlan) { p.Items[1].ID = "story" },
		func(p *WorkPlan) { p.Items[1].DependsOn = []string{"missing"} },
		func(p *WorkPlan) { p.Items[0].DependsOn = []string{"two"}; p.Items[1].DependsOn = []string{"story"} },
		func(p *WorkPlan) { p.Items[1].Parent = "two" },
		func(p *WorkPlan) { p.Items[0].Base = "../main" },
		func(p *WorkPlan) { p.Items[0].JiraKey = "DEMO-99" },
	} {
		p := simplePlan()
		mutate(&p)
		if err := p.validate(); err == nil {
			t.Fatal("invalid plan accepted", p)
		}
	}
	for _, raw := range []string{`{"id":"a","id":"b","items":[]}`, `{"id":"a","items":[],"url":"https://evil"}`, `{"id":"a","items":[{"id":"a","title":"t","body":"","kind":"story","number":1.5}]}`} {
		if _, err := decodeWorkPlan([]byte(raw)); err == nil {
			t.Fatal("invalid JSON contract accepted")
		}
	}
}

func TestManagementLifecycleAfterRunAndRestart(t *testing.T) {
	m, f := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, simplePlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.apply(ctx, p.Digest); err != nil {
		t.Fatal(err)
	}
	// Any accidental agent launch fails: tracker management uses only the fake APIs.
	t.Setenv("PATH", t.TempDir())
	if err := m.register(ctx, 11, true, 20, "Implementation started"); err != nil {
		t.Fatal(err)
	}
	pr := linkedPR{Title: "DEMO-11 feature", URL: "https://github.com/owner/repo/pull/20", State: "OPEN", IsDraft: true, BaseRefName: "main"}
	pr.ClosingIssuesReferences = append(pr.ClosingIssuesReferences, struct {
		URL    string
		Number int
	}{f.gh[11].URL, 11})
	f.prs[20] = pr
	if err := m.reconcile(ctx, 11, true); err != nil {
		t.Fatal(err)
	}
	if f.jira["DEMO-11"].Fields.Status.Name != "In Progress" || f.dates["DEMO-11"] != "2025-01-02" {
		t.Fatal("implementation/date not synchronized")
	}
	if len(f.comments) != 1 || len(f.jiraComments) != 1 {
		t.Fatal("progress comment missing")
	}
	pr.IsDraft = false
	f.prs[20] = pr
	if err := m.reconcile(ctx, 11, true); err != nil {
		t.Fatal(err)
	}
	if f.jira["DEMO-11"].Fields.Status.Name != "In Review" {
		t.Fatal("human ready event not synchronized")
	}
	restarted := *m
	restarted.state = managementState{}
	pr.State, pr.MergedAt = "MERGED", "2025-01-03T00:00:00Z"
	f.prs[20] = pr
	if err := restarted.reconcile(ctx, 11, true); err != nil {
		t.Fatal(err)
	}
	if f.gh[11].State != "closed" || f.jira["DEMO-11"].Fields.Status.Name != "Ready for Deploy" {
		t.Fatal("merge not synchronized after restart")
	}
	if f.gh[10].State != "open" || f.jira["DEMO-10"].Fields.Status.Name != "To Do" {
		t.Fatal("parent incorrectly rolled up")
	}
	if len(f.comments) != 1 || len(f.jiraComments) != 1 {
		t.Fatal("duplicate comments")
	}
}

func TestManagementJournalLockAndScope(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	second := *m
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	executed := make(chan struct{}, 2)
	for _, instance := range []*manager{m, &second} {
		wg.Add(1)
		go func(m *manager) {
			defer wg.Done()
			errs <- m.locked(ctx, func() error {
				return m.op(ctx, "one", true, func(bool) (any, error) {
					executed <- struct{}{}
					time.Sleep(10 * time.Millisecond)
					return "only one", nil
				})
			})
		}(instance)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if len(executed) != 1 {
			t.Fatal("concurrent processes executed the same operation twice")
		}
	}
	second.scope = "other-project"
	if err := second.locked(ctx, func() error { t.Fatal("wrong-scope callback ran"); return nil }); err == nil {
		t.Fatal("scope collision accepted")
	}
}

func TestManagementHTTPApprovalBoundary(t *testing.T) {
	m, f := testManager(t)
	mux := http.NewServeMux()
	managementRoutes(mux, m, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "POST" || r.Header.Get("X-SM-Token") != "secret" {
			http.Error(w, "forbidden", 403)
			return false
		}
		return true
	})
	for _, token := range []string{"", "secret"} {
		req := httptest.NewRequest("POST", "/api/management/apply", strings.NewReader(`{"digest":"bad"}`))
		req.Header.Set("X-SM-Token", token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code < 400 {
			t.Fatal("invalid approval accepted")
		}
	}
	if writes(f.calls) > 0 {
		t.Fatal("invalid request wrote trackers")
	}
}

func TestManagementProfileAndCredentialIsolation(t *testing.T) {
	m, _ := testManager(t)
	p := m.profile
	if err := p.validateFor(true); err != nil {
		t.Fatal(err)
	}
	if err := p.validate(); err == nil {
		t.Fatal("coding allowed without checks")
	}
	p.Jira.Mode = "actions"
	if err := p.validateFor(true); err == nil {
		t.Fatal("two writer modes allowed")
	}
	old := settings
	settings = m.profile
	defer func() { settings = old }()
	t.Setenv("GH_TOKEN", "service-write-token")
	t.Setenv("JIRA_API_TOKEN", "jira-write-token")
	if strings.Contains(strings.Join(cleanAgentEnv(), "\n"), "write-token") {
		t.Fatal("credentials leaked")
	}
	if strings.Contains(configuredAllow(Plan), "gh issue") {
		t.Fatal("worker tracker access retained")
	}
	if !strings.Contains(strings.Join(copilotArgs(Plan, "", "usage", 1), " "), "--deny-tool=shell(rtk proxy gh:*)") {
		t.Fatal("RTK tracker bypass")
	}
}
