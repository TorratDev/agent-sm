//go:build formal

// Bounded exhaustive check of tracker creation in service-management mode. It runs the real
// manager.apply against the fake trackers under every schedule of up to N faults and checks
// three rules after every apply. Run and read it with the verify-tracker-create skill
// (.claude/skills/verify-tracker-create/SKILL.md). Not part of the normal test run.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

const (
	ghCreate   = "github POST /repos/owner/repo/issues"
	jiraCreate = "jira POST /issue"
	ghSearch   = "github GET /repos/owner/repo/issues"
	jiraSearch = "jira GET /search/jql"
)

// formalFault is one thing that goes wrong at one tracker call, counted from the first apply.
type formalFault struct {
	at   int
	kind string // fail | lost | crash | crash-after | stale
	call string // the call expected at that index; a mismatch means the run is not deterministic
}

var formalKinds = map[string]string{
	"fail":        "returns an error; the tracker did nothing",
	"lost":        "takes effect on the tracker, but the response is lost",
	"crash":       "the service dies just before sending it",
	"crash-after": "takes effect on the tracker, then the service dies before recording it",
	"stale":       "answers from a stale index that does not show newly created issues yet",
}

type formalCrash struct{}

// formalAPI wraps the fake trackers, injects the scheduled faults and counts create requests.
type formalAPI struct {
	inner  *fakeTrackers
	faults []formalFault
	apply  int
	trace  []string       // "apply N: <call>" for every call
	calls  []string       // the bare call at each index
	posts  map[string]int // "<tracker> <item>" -> create requests sent
	items  []string
	done   bool // the last apply reported the batch complete
	bad    string
}

func (a *formalAPI) call(ctx context.Context, system, method, path string, input, output any) error {
	key := system + " " + method + " " + strings.Split(path, "?")[0]
	at := len(a.calls)
	a.calls = append(a.calls, key)
	a.trace = append(a.trace, fmt.Sprintf("apply %d: %s", a.apply, key))
	kind := ""
	for _, f := range a.faults {
		if f.at == at {
			if f.call != key {
				a.bad = fmt.Sprintf("explorer: call %d was %q, expected %q; the run is not deterministic", at, key, f.call)
			}
			kind = f.kind
		}
	}
	if kind == "crash" { // the service died before sending anything
		panic(formalCrash{})
	}
	if key == ghCreate || key == jiraCreate { // a create request left the service, whatever happens to it
		b, _ := json.Marshal(input)
		for _, id := range a.items {
			if strings.Contains(string(b), "agent-sm:batch:"+id) {
				a.posts[system+" "+id]++
			}
		}
	}
	if kind == "fail" { // the service cannot tell this from a lost response
		return trackerError{500}
	}
	if kind == "stale" {
		if key == ghSearch {
			return json.Unmarshal([]byte(`[]`), output)
		}
		return json.Unmarshal([]byte(`{"issues":[],"isLast":true}`), output)
	}
	err := a.inner.call(ctx, system, method, path, input, output)
	switch kind {
	case "lost":
		return fmt.Errorf("lost response")
	case "crash-after":
		panic(formalCrash{})
	}
	return err
}

func formalKindsFor(call string) []string {
	if strings.Contains(call, " GET ") || call == "github POST /graphql" {
		if call == ghSearch || call == jiraSearch {
			return []string{"fail", "crash", "stale"}
		}
		return []string{"fail", "crash"}
	}
	return []string{"fail", "crash", "lost", "crash-after"}
}

// formalRules returns the first broken rule, or "".
func formalRules(a *formalAPI, complete bool) string {
	for _, id := range a.items {
		marker := "agent-sm:batch:" + id
		gh, jira := 0, 0
		for _, s := range a.inner.gh {
			if strings.Contains(s.Body, marker+" -->") {
				gh++
			}
		}
		for _, s := range a.inner.jira {
			if strings.Contains(string(s.Fields.Description), marker+`"`) {
				jira++
			}
		}
		only := os.Getenv("FORMAL_ONLY") // check one rule alone, to see it fail by itself
		if (gh > 1 || jira > 1) && (only == "" || only == "NoDuplicate") {
			return fmt.Sprintf("NoDuplicate: item %q exists %d times on GitHub and %d times on Jira", id, gh, jira)
		}
		for _, tracker := range []string{"github", "jira"} {
			if n := a.posts[tracker+" "+id]; n > 1 && (only == "" || only == "NoSecondCreate") {
				return fmt.Sprintf("NoSecondCreate: %d create requests were sent to %s for item %q", n, tracker, id)
			}
		}
		if complete && (gh != 1 || jira != 1) && (only == "" || only == "CompleteMeansPaired") {
			return fmt.Sprintf("CompleteMeansPaired: the batch is complete but item %q exists %d times on GitHub and %d times on Jira", id, gh, jira)
		}
	}
	return ""
}

// formalRun replays one fault schedule from an empty journal and empty trackers: preview, then
// apply the approved digest again after every failure or crash, as an operator or a restart does.
func formalRun(t *testing.T, plan WorkPlan, faults []formalFault) *formalAPI {
	m, f := testManager(t)
	ctx := context.Background()
	p, err := m.preview(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	a := &formalAPI{inner: f, faults: faults, posts: map[string]int{}}
	for _, i := range plan.Items {
		a.items = append(a.items, i.ID)
	}
	complete := false
	for a.apply = 1; a.apply <= len(faults)+2 && !complete && a.bad == ""; a.apply++ {
		restarted := &manager{dir: m.dir, repo: m.repo, scope: m.scope, profile: m.profile, api: a}
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, ok := r.(formalCrash); !ok {
						panic(r)
					}
				}
			}()
			complete = restarted.apply(ctx, p.Digest) == nil
		}()
		if a.bad == "" {
			a.bad = formalRules(a, complete)
		}
	}
	a.done = complete
	if complete && a.bad == "" { // a completed batch must stay a no-op
		before := len(a.inner.calls)
		again := &manager{dir: m.dir, repo: m.repo, scope: m.scope, profile: m.profile, api: a}
		if err := again.apply(ctx, p.Digest); err != nil || len(a.inner.calls) != before {
			a.bad = fmt.Sprintf("CompleteMeansPaired: applying the completed batch again was not a no-op (%v)", err)
		}
	}
	return a
}

// formalExplore tries every schedule of at most maxFaults faults, fewest faults first so that
// the first schedule reported is a shortest one. It returns how many schedules it ran and how
// many ended with the batch complete, and stops at the first broken rule.
func formalExplore(t *testing.T, plan WorkPlan, maxFaults int) (runs, completed int) {
	var dfs func(faults []formalFault, limit int) bool
	dfs = func(faults []formalFault, limit int) bool {
		a := formalRun(t, plan, faults)
		runs++
		if a.bad != "" {
			var b strings.Builder
			fmt.Fprintf(&b, "RULE BROKEN: %s\nShortest schedule found (%d faults):\n", a.bad, len(faults))
			for _, f := range faults {
				fmt.Fprintf(&b, "  %s (call %d) %s\n", a.trace[f.at], f.at, formalKinds[f.kind])
			}
			fmt.Fprintf(&b, "Create requests sent: %v\nAll %d calls:\n  %s", a.posts, len(a.trace), strings.Join(a.trace, "\n  "))
			t.Error(b.String())
			return false
		}
		if a.done {
			completed++
		}
		if len(faults) == limit {
			return true
		}
		from := 0
		if len(faults) > 0 {
			from = faults[len(faults)-1].at + 1
		}
		for at := from; at < len(a.calls); at++ {
			for _, kind := range formalKindsFor(a.calls[at]) {
				if !dfs(append(append([]formalFault{}, faults...), formalFault{at, kind, a.calls[at]}), limit) {
					return false
				}
			}
		}
		return true
	}
	for limit := 0; limit <= maxFaults; limit++ {
		runs, completed = 0, 0
		if !dfs(nil, limit) {
			return
		}
	}
	return
}

func formalBound(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n >= 0 {
		return n
	}
	return fallback
}

func TestFormalTrackerCreate(t *testing.T) {
	one := WorkPlan{ID: "batch", Items: []WorkItem{{ID: "story", Title: "Story", Body: "Requirements", Kind: "story"}}}
	for _, c := range []struct {
		name   string
		plan   WorkPlan
		faults int
	}{
		{"one item", one, formalBound("FORMAL_FAULTS_ONE", 2)},
		{"story with two tasks", simplePlan(), formalBound("FORMAL_FAULTS_THREE", 1)},
	} {
		runs, completed := formalExplore(t, c.plan, c.faults)
		if t.Failed() {
			return
		}
		if completed == 0 {
			t.Fatalf("%s: no schedule completed the batch; the check is vacuous", c.name)
		}
		t.Logf("%s: all rules hold in %d schedules of at most %d faults (%d completed the batch)", c.name, runs, c.faults, completed)
	}
}
