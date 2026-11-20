package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every (state, out) a step may return must have a rule: next() must not panic.
func TestTableHasNoHoles(t *testing.T) {
	for i, st := range steps {
		s := State(i)
		if st.Name == "" || st.Run == nil || len(st.Outs) == 0 {
			t.Errorf("state %d is not fully declared: %+v", i, st)
		}
		if agentStep := st.Role != ""; agentStep != (st.Schema != "" && st.Allow != "") {
			t.Errorf("%v: an agent step needs Role, Schema and Allow together", s)
		}
		for _, o := range st.Outs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("hole: state=%v out=%v: %v", s, o, r)
					}
				}()
				next(s, o, [numStates]int{})
			}()
		}
	}
}

func TestLimits(t *testing.T) {
	var v [numStates]int
	v[Execute] = maxExecute
	if n := next(Check, Fail, v); n.Exit != GaveUp {
		t.Errorf("check fail at limit = %+v, want gave-up", n)
	}
	v[Execute] = 1
	if n := next(Review, Fail, v); n.State != Execute {
		t.Errorf("review fail = %+v, want execute", n)
	}
	v[Review] = maxInvalid
	if n := next(Review, Invalid, v); n.Exit != Errored {
		t.Errorf("repeated invalid = %+v, want errored", n)
	}
	v[Review] = 1
	if n := next(Review, Invalid, v); n.State != Review {
		t.Errorf("first invalid = %+v, want retry", n)
	}
	v[Plan] = 1
	if n := next(Execute, Blocked, v); n.State != Plan {
		t.Errorf("first blocked execute = %+v, want re-plan", n)
	}
	v[Plan] = maxPlan
	if n := next(Execute, Blocked, v); n.Exit != GaveUp {
		t.Errorf("blocked execute after re-plan = %+v, want gave-up", n)
	}
	if n := next(Execute, Fail, v); n.Exit != Errored {
		t.Errorf("execute management failure = %+v, want errored", n)
	}
}

func TestReportFindsUnused(t *testing.T) {
	in := `{"state":"review","tools_listed":["Read","Bash","Edit","StructuredOutput"],"mcp_servers":["claude.ai Notion","claude.ai Canva"],"tool_calls":{"Read":3,"mcp__claude_ai_Notion__notion-search":1,"StructuredOutput":1},"denied_tools":{"Bash":2},"denied_sample":["Bash {\"command\":\"ls\"}"],"cost_usd":0.1,"turns":2}`
	got := report([]byte(in))
	for _, want := range []string{"TRIM tools (listed, never called) 2: Bash, Edit", "TRIM (no calls): claude.ai Canva", "Read x3", "DENIED (wanted, not allowed; add to allow list or fix the prompt): Bash x2", "e.g. Bash"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q in:\n%s", want, got)
		}
	}
}

func TestRefsGuard(t *testing.T) {
	for _, bad := range []string{"does not close #5", "Fixes #12 partly", "#7 resolved here"} {
		if !closeWord.MatchString(bad) {
			t.Errorf("guard missed %q", bad)
		}
	}
	for _, ok := range []string{"Refs #5", "Part of #5: docs", "rename the fix-it helper"} {
		if closeWord.MatchString(ok) {
			t.Errorf("guard flagged %q", ok)
		}
	}
}

func TestCmdWords(t *testing.T) {
	got := strings.Join(cmdWords("cd /x; rtk grep -rn foo . | head -3 && git log --oneline; for a in 1; do ls $a; done"), ",")
	if want := "cd,rtk grep,head,git log,ls"; got != want {
		t.Errorf("cmdWords = %q, want %q", got, want)
	}
}

func TestDependencies(t *testing.T) {
	if d := dependencies("## Depends on / Related\n- **Parent:** #73\n- Related: #9\n"); len(d) != 0 {
		t.Errorf("parent/related are not dependencies: %v", d)
	}
	d := dependencies("## Dependencies\n- #5\n- Parent: #7\n- #8 first\n\n## Rules\n#99")
	if len(d) != 2 || d[0] != 5 || d[1] != 8 {
		t.Errorf("dependencies = %v, want [5 8]", d)
	}
	if d := dependencies("## Depends on\n- **Depends on:** #12\n- note #3"); len(d) != 1 || d[0] != 12 {
		t.Errorf("depends-on line = %v, want [12]", d)
	}
	body := "## Dependencies\nSibling `depends_on`: `[]`.\n\n" +
		"Story gates remain #54 -> #21/#22 -> #23 -> #24.\n" +
		"See #20's checkpoint; this does not depend on #99.\n\n" +
		"## Branching\n- Base branch: `feature` (parent Story #21)."
	if d := dependencies(body); len(d) != 0 {
		t.Errorf("contextual references became blockers: %v", d)
	}
	if d := dependencies("## Dependencies\n**Depends on:** #12\nBlocked by #13\n1. #14\n+ #15"); !slices.Equal(d, []int{12, 13, 14, 15}) {
		t.Errorf("explicit entries = %v, want [12 13 14 15]", d)
	}
}

func TestScopeAndAllow(t *testing.T) {
	files := []string{"docs/re/status.md", "crates/sim/"}
	if !inScope("docs/re/status.md", files) || !inScope("crates/sim/src/a.rs", files) || inScope("crates/data/a.rs", files) {
		t.Error("inScope wrong")
	}
	a := suggestAllow("investigate", map[string]int{"Bash": 2, "Edit": 1, "StructuredOutput": 1},
		map[string]int{"ghidra status": 2, "git push": 1, "rtk git commit": 1, "rm": 1})
	if !strings.Contains(a, "Bash(ghidra status:*)") || strings.Contains(a, "push") || strings.Contains(a, "commit") ||
		strings.Contains(a, "rm") || strings.Contains(a, "Edit") {
		t.Errorf("suggestAllow = %q", a)
	}
}

func TestCmdWordsIgnoresHeredocAndQuotes(t *testing.T) {
	cmd := "python3 - <<'EOF'\nimport re\nassert x; the = 1\nEOF\ngrep -n \"a; b | c\" f && 021408f8 Bite"
	if got, want := strings.Join(cmdWords(cmd), ","), "python3,grep"; got != want {
		t.Errorf("cmdWords = %q, want %q", got, want)
	}
}

func TestUnitLoop(t *testing.T) {
	var v [numStates]int
	if n := next(Investigate, More, v); n.State != Investigate {
		t.Errorf("more units = %+v, want investigate again", n)
	}
	v[Investigate] = maxInvalid
	if n := next(Investigate, Invalid, v); n.Exit != Errored {
		t.Errorf("repeated invalid in one unit = %+v, want errored", n)
	}
	if n := next(Plan, Direct, v); n.State != Execute {
		t.Errorf("direct = %+v, want execute", n)
	}
}

// fake replaces steps' Run functions for one test; states not named pass straight through.
func fake(t *testing.T, runs map[State]func(*Ctx) Out) {
	old := steps
	t.Cleanup(func() { steps = old })
	for i := range steps {
		o := steps[i].Outs[0] // Ok or Pass: the forward edge
		steps[i].Run = func(*Ctx) Out { return o }
	}
	for s, f := range runs {
		steps[s].Run = f
	}
}

func TestRunLoop(t *testing.T) {
	// Review never passes: execute runs maxExecute times, then the run gives up.
	executes := 0
	fake(t, map[State]func(*Ctx) Out{
		Execute: func(*Ctx) Out { executes++; return Ok },
		Review:  func(*Ctx) Out { return Fail },
	})
	if exit := run(&Ctx{}); exit != GaveUp || executes != maxExecute {
		t.Errorf("execute limit: exit=%v after %d executes, want gave-up after %d", exit, executes, maxExecute)
	}

	// Invalid output: one retry of the same state, then on to ready.
	plans := 0
	fake(t, map[State]func(*Ctx) Out{Plan: func(*Ctx) Out {
		if plans++; plans == 1 {
			return Invalid
		}
		return Direct
	}})
	if exit := run(&Ctx{}); exit != Ready || plans != 2 {
		t.Errorf("invalid retry: exit=%v after %d plans, want ready after 2", exit, plans)
	}
	// Invalid every time: errored after maxInvalid runs.
	plans = 0
	fake(t, map[State]func(*Ctx) Out{Plan: func(*Ctx) Out { plans++; return Invalid }})
	if exit := run(&Ctx{}); exit != Errored || plans != maxInvalid {
		t.Errorf("invalid limit: exit=%v after %d plans, want errored after %d", exit, plans, maxInvalid)
	}

	// A blocked execute re-plans once, and the re-plan hears why.
	plans, executes = 0, 0
	var replanSaw string
	fake(t, map[State]func(*Ctx) Out{
		Plan: func(c *Ctx) Out { plans++; replanSaw = c.Summary; return Direct },
		Execute: func(c *Ctx) Out {
			if executes++; executes == 1 {
				c.Summary = "needs apps/x.rs"
				return Blocked
			}
			return Ok
		},
	})
	if exit := run(&Ctx{}); exit != Ready || plans != 2 || replanSaw != "needs apps/x.rs" {
		t.Errorf("re-plan: exit=%v after %d plans (saw %q), want ready after 2", exit, plans, replanSaw)
	}
	// Blocked again after the re-plan: gave-up.
	plans = 0
	fake(t, map[State]func(*Ctx) Out{
		Plan:    func(*Ctx) Out { plans++; return Direct },
		Execute: func(*Ctx) Out { return Blocked },
	})
	if exit := run(&Ctx{}); exit != GaveUp || plans != maxPlan {
		t.Errorf("blocked twice: exit=%v after %d plans, want gave-up after %d", exit, plans, maxPlan)
	}

	// A timed-out or over-budget session is not rerun.
	for _, o := range []Out{Timeout, OverBudget} {
		n := 0
		fake(t, map[State]func(*Ctx) Out{Investigate: func(*Ctx) Out { n++; return o }})
		if exit := run(&Ctx{}); exit != GaveUp || n != 1 {
			t.Errorf("%v: exit=%v after %d runs, want gave-up after 1", o, exit, n)
		}
	}
}

func TestRunBudget(t *testing.T) {
	var ran []State
	fake(t, map[State]func(*Ctx) Out{
		Plan:        func(c *Ctx) Out { ran = append(ran, Plan); c.Cost += 3; return Ok },
		Investigate: func(c *Ctx) Out { ran = append(ran, Investigate); c.Cost += 3; return Ok },
		Execute:     func(c *Ctx) Out { ran = append(ran, Execute); return Ok },
	})
	if exit := run(&Ctx{RunBudget: 5}); exit != GaveUp || len(ran) != 2 {
		t.Errorf("exit=%v after %v, want gave-up before execute starts", exit, ran)
	}
}

func TestStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runCtx = ctx
	t.Cleanup(func() { runCtx = context.Background() })
	fake(t, map[State]func(*Ctx) Out{Plan: func(*Ctx) Out { cancel(); return Ok }})
	if exit := run(&Ctx{}); exit != Stopped {
		t.Errorf("exit=%v, want stopped", exit)
	}
}

func TestReviewDiffIsNeverCut(t *testing.T) {
	if got := reviewDiff("small", "stat"); got != "small" {
		t.Errorf("a small diff must pass unchanged, got %q", got)
	}
	big := strings.Repeat("x", maxDiff+1)
	got := reviewDiff(big, " a.rs | 9 +")
	if strings.Contains(got, "xxx") || !strings.Contains(got, "NOT INCLUDED") || !strings.Contains(got, "a.rs") || !strings.Contains(got, "git diff --cached") {
		t.Errorf("an oversized diff must be replaced by a note with the file list, got %q", tail(got, 300))
	}
}

// Every command a role prompt tells its agent to run must be on that state's default allow list.
// shortcut: the commands are listed here by hand (prompts are prose); the test fails if one
// leaves the prompt, so the list cannot rot, but a new command in a prompt must be added here.
func TestPromptCommandsAreAllowed(t *testing.T) {
	p := defaultProfile("claude")
	p.Legacy = true
	useProfile(t, p)
	named := map[State][]string{
		Plan:        {"gh issue view"},
		Investigate: {"ghidra", "grep", "~/venvs/desmume/bin/python", "Export JAVA_HOME", "source scripts/agent.sh", "re_fn", "re_tree"},
		Execute:     {"cargo fmt", "scripts/verify.sh", "~/venvs/desmume/bin/python"},
		Review:      {"git diff --cached", "grep", "ghidra", "~/venvs/desmume/bin/python", "Export JAVA_HOME", "source scripts/agent.sh", "re_src"},
	}
	for s, cmds := range named {
		prompt := rolePrompt(s)
		for _, cmd := range cmds {
			if !strings.Contains(prompt, cmd) {
				t.Errorf("%v: %q is no longer in the prompt; update this test", s, cmd)
			}
			allowed := false
			for _, a := range strings.Split(configuredAllow(s), ",") {
				if p, ok := strings.CutPrefix(a, "Bash("); ok && strings.HasPrefix(strings.ToLower(cmd), strings.TrimSuffix(p, ":*)")) {
					allowed = true
				}
			}
			if !allowed {
				t.Errorf("%v: the prompt names %q but the default allow list does not allow it", s, cmd)
			}
		}
	}
	if got := withRTK("Read,Bash(git log:*)"); got != "Read,Bash(git log:*),Bash(rtk git log:*),Bash(rtk proxy git log:*)" {
		t.Errorf("withRTK = %q", got)
	}
}

func TestAgentStepsHaveSchemaAndPrompt(t *testing.T) {
	for _, st := range steps {
		if st.Role != "" && !json.Valid([]byte(st.Schema)) {
			t.Errorf("%s: schema is not valid JSON", st.Name)
		}
		if st.Role != "" {
			if _, err := promptFS.ReadFile("prompts/" + st.Role + ".md"); err != nil {
				t.Errorf("%s: %v", st.Name, err)
			}
		}
	}
}

func TestPickSubIssues(t *testing.T) {
	open := func(n int) bool { return n == 7 }
	if run, notes := pickSubIssues(5, nil, open); len(run) != 1 || run[0] != 5 || notes != nil {
		t.Errorf("an issue without sub-issues runs itself: %v %v", run, notes)
	}
	subs := []subIssue{
		{Number: 10, State: "open"},
		{Number: 11, State: "closed"},
		{Number: 12, State: "open", Blocked: 1},
		{Number: 13, State: "open", Body: "## Dependencies\n- #7"},
		{Number: 14, State: "open", Body: "## Dependencies\n- #8"}, // #8 is closed: free to run
		{Number: 15, State: "open", Subs: 2},
	}
	run, notes := pickSubIssues(5, subs, open)
	if len(run) != 2 || run[0] != 10 || run[1] != 14 {
		t.Errorf("run = %v, want [10 14] (the parent and dependent sub-issues never run)", run)
	}
	if got := strings.Join(notes, "|"); !strings.Contains(got, "#12 skipped: blocked") || !strings.Contains(got, "#13 skipped: depends on #7") || !strings.Contains(got, "#15 skipped: has its own") {
		t.Errorf("notes = %q", got)
	}
	if run, notes := pickSubIssues(5, subs[2:3], open); len(run) != 0 || !strings.Contains(strings.Join(notes, "|"), "no sub-issue can run now") {
		t.Errorf("all blocked: %v %v", run, notes)
	}
}

func TestExpandSubIssues(t *testing.T) {
	useProfile(t, defaultProfile("copilot"))
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
case "$1" in
api)
  for arg in "$@"; do
    case "$arg" in
      --paginate) paginate=1;;
      --slurp) slurp=1;;
      --jq) jq=1;;
    esac
  done
  if [ "$slurp" = 1 ] && [ "$jq" = 1 ]; then
    printf '%s\n\n' 'the --slurp option is not supported with --jq' >&2
    printf '%0220d\n' 0 >&2
    exit 1
  fi
  case "$SM_TEST_GH" in
    empty) printf '%s\n' '[]';;
    pages)
      printf '%s\n' '[{"number":11,"state":"open"},{"number":12,"state":"open","blocked":1},{"number":13,"state":"closed"}]'
      if [ "$paginate" = 1 ]; then
        printf '%s\n' '[{"number":14,"state":"open","body":"## Dependencies\n- #7"},{"number":15,"state":"open","subs":2},{"number":16,"state":"open","body":"## Dependencies\n- #8"}]'
      fi;;
    error)
      printf '%s\n\n' 'gh: API rate limit exceeded' >&2
      printf '%0220d\n' 0 >&2
      exit 1;;
    partial-error)
      printf '%s\n' '[{"number":11,"state":"open"}]'
      printf '%s\n' 'gh: permission denied' >&2
      exit 1;;
    malformed) printf '[]\n{';;
    empty-output) :;;
    null) printf 'null\n';;
    all-closed|close-fails) printf '%s\n' '[{"number":11,"state":"closed"},{"number":13,"state":"closed"}]';;
    *) exit 2;;
  esac;;
issue)
  if [ "$2" = close ]; then
    if [ "$SM_TEST_GH" = close-fails ]; then printf 'gh: no permission\n' >&2; exit 1; fi
    printf '%s\n' "$*" > "$SM_TEST_CLOSED"; exit 0
  fi
  case "$3" in
    7) printf 'OPEN\n';;
    8) printf 'CLOSED\n';;
    *) exit 2;;
  esac;;
*) exit 2;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	closedLog := filepath.Join(dir, "closed")
	t.Setenv("SM_TEST_CLOSED", closedLog)
	for _, tc := range []struct {
		name  string
		run   []int
		notes []string
	}{
		{"empty", []int{10}, nil},
		{"pages", []int{11, 16}, []string{"#12 skipped: blocked", "#14 skipped: depends on #7", "#15 skipped: has its own"}},
		{"error", nil, []string{"#10 not queued: cannot read its sub-issues: gh: API rate limit exceeded"}},
		{"partial-error", nil, []string{"gh: permission denied"}},
		{"malformed", nil, []string{"#10 not queued: cannot read its sub-issues:"}},
		{"empty-output", nil, []string{"#10 not queued: cannot read its sub-issues:"}},
		{"null", nil, []string{"#10 not queued: cannot read its sub-issues:"}},
		{"all-closed", nil, []string{"#10 closed: all its sub-issues are closed"}},
		{"close-fails", nil, []string{"closing it failed: gh: no permission"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SM_TEST_GH", tc.name)
			run, notes := expand(dir, 10)
			if !slices.Equal(run, tc.run) {
				t.Errorf("run = %v, want %v", run, tc.run)
			}
			if len(notes) != len(tc.notes) {
				t.Errorf("notes = %v, want %v", notes, tc.notes)
			}
			for _, want := range tc.notes {
				if !strings.Contains(strings.Join(notes, "\n"), want) {
					t.Errorf("notes = %v, want %q", notes, want)
				}
			}
		})
	}
	if closed, _ := os.ReadFile(closedLog); !strings.Contains(string(closed), "issue close 10 --reason completed") {
		t.Errorf("close command = %q", closed)
	}
	t.Run("missing gh", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		run, notes := expand(dir, 10)
		if len(run) != 0 || len(notes) != 1 || !strings.Contains(notes[0], `exec: "gh"`) {
			t.Fatalf("run = %v, notes = %v", run, notes)
		}
	})
}

func TestGitHubDependencies(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
printf '%s\n' "$SM_TEST_BLOCKERS"
exit "$SM_TEST_BLOCKERS_EXIT"
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, response, exit, want string
	}{
		{"none", "[]", "0", ""},
		{"closed", `[{"number":7,"state":"closed"}]`, "0", ""},
		{"later page blocked", "[{\"number\":7,\"state\":\"closed\"}]\n[{\"number\":8,\"state\":\"open\"}]", "0", "dependency #8"},
		{"unavailable", "permission denied", "1", "cannot read GitHub blockers"},
		{"incomplete", "[]\n{", "0", "invalid GitHub blockers"},
		{"null", "null", "0", "invalid GitHub blockers"},
		{"invalid identity", "[null]", "0", "invalid GitHub blocker identity"},
		{"unknown state", `[{"number":7}]`, "0", "invalid GitHub blocker identity or state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SM_TEST_BLOCKERS", tc.response)
			t.Setenv("SM_TEST_BLOCKERS_EXIT", tc.exit)
			err := githubDependencies(dir, 10)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func testGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := sh(repo, "git", args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func retryRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	testGit(t, dir, "init", "-q", "-b", "main", repo)
	testGit(t, repo, "config", "user.name", "test")
	testGit(t, repo, "config", "user.email", "test@example.com")
	testGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	remote := filepath.Join(dir, "remote.git")
	testGit(t, dir, "init", "-q", "--bare", remote)
	testGit(t, repo, "remote", "add", "origin", remote)
	testGit(t, repo, "push", "-q", "origin", "main")
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
if [ -n "$SM_TEST_GH_ARGS" ]; then printf '%s\n' "$@" >> "$SM_TEST_GH_ARGS"; fi
case "$1 $2" in
  "pr list")
    if [ "$SM_TEST_PR_ERROR" = 1 ]; then echo 'PR lookup failed' >&2; exit 1; fi
    if [ -n "$SM_TEST_PR_RESPONSE" ]; then printf '%s\n' "$SM_TEST_PR_RESPONSE"; exit; fi
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--head" ]; then shift; head="$1"; fi
      shift
    done
    if [ "$head" = "$SM_TEST_PR_BRANCH" ]; then echo '[{"number":42}]'; else echo '[]'; fi;;
  "pr create") echo 'https://github.com/example/repo/pull/42';;
  "pr comment") :;;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return repo
}

func TestWorktreeRetry(t *testing.T) {
	repo := retryRepo(t)
	r := &Runner{repo: repo}
	dir, branch, err := r.worktreeAt(5, "main")
	if err != nil || branch != "sm/issue-5" || filepath.Base(dir) != "5-sm" {
		t.Fatalf("first attempt: %s %s %v", dir, branch, err)
	}
	writeFile(t, filepath.Join(dir, "committed.txt"), "old commit", 0o644)
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-qm", "old attempt")
	oldHead := testGit(t, dir, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "staged.txt"), "old staged work", 0o644)
	testGit(t, dir, "add", "staged.txt")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "old untracked work", 0o644)
	testGit(t, repo, "branch", "sm/issue-5-10", "main")

	fresh, branch, err := r.worktreeAt(5, "main")
	if err != nil || branch != "sm/issue-5-1" || filepath.Base(fresh) != "5-sm-1" {
		t.Fatalf("retry: %s %s %v", fresh, branch, err)
	}
	if testGit(t, fresh, "rev-parse", "HEAD") != testGit(t, repo, "rev-parse", "main") {
		t.Fatal("retry inherited the old attempt instead of the configured base")
	}
	for _, file := range []string{"committed.txt", "staged.txt", "untracked.txt"} {
		if _, err := os.Stat(filepath.Join(fresh, file)); !os.IsNotExist(err) {
			t.Errorf("retry inherited %s", file)
		}
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Errorf("old work lost %s: %v", file, err)
		}
	}
	if testGit(t, dir, "rev-parse", "HEAD") != oldHead || testGit(t, dir, "diff", "--cached", "--name-only") != "staged.txt" {
		t.Fatal("old branch or index changed")
	}
	testGit(t, repo, "branch", "sm/issue-5-2", "main")
	occupied := filepath.Join(r.worktreeRoot(), "5-sm-3")
	if err := os.Mkdir(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(occupied, "keep.txt"), "keep", 0o644)
	testGit(t, repo, "push", "-q", "origin", "main:refs/heads/sm/issue-5-4")
	t.Setenv("SM_TEST_PR_BRANCH", "sm/issue-5-5")
	next, branch, err := r.worktreeAt(5, "main")
	if err != nil || branch != "sm/issue-5-6" || filepath.Base(next) != "5-sm-6" {
		t.Fatalf("mixed collisions: %s %s %v", next, branch, err)
	}
	if b, err := os.ReadFile(filepath.Join(occupied, "keep.txt")); err != nil || string(b) != "keep" {
		t.Fatal("occupied directory was changed")
	}
	if got := r.leftovers(); !slices.Equal(got, []int{5}) {
		t.Errorf("leftovers = %v", got)
	}
}

func TestRegisteredWorktreeRetry(t *testing.T) {
	repo := retryRepo(t)
	r := &Runner{repo: repo}
	old := filepath.Join(r.worktreeRoot(), "5-sm")
	testGit(t, repo, "worktree", "add", "--detach", old, "main")
	if err := os.Rename(old, old+"-kept"); err != nil {
		t.Fatal(err)
	}
	dir, branch, err := r.worktreeAt(5, "main")
	if err != nil || branch != "sm/issue-5-1" || filepath.Base(dir) != "5-sm-1" {
		t.Fatalf("registered collision: %s %s %v", dir, branch, err)
	}
}

func TestRetryLookupFailsClosed(t *testing.T) {
	for _, mode := range []string{"github unavailable", "invalid github response", "invalid PR identity", "remote unavailable"} {
		t.Run(mode, func(t *testing.T) {
			repo := retryRepo(t)
			switch mode {
			case "github unavailable":
				t.Setenv("SM_TEST_PR_ERROR", "1")
			case "invalid github response":
				t.Setenv("SM_TEST_PR_RESPONSE", "null")
			case "invalid PR identity":
				t.Setenv("SM_TEST_PR_RESPONSE", "[null]")
			case "remote unavailable":
				testGit(t, repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
			}
			r := &Runner{repo: repo}
			if _, _, err := r.worktreeAt(5, "main"); err == nil {
				t.Fatal("lookup failure was treated as a free name")
			}
			if got := testGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/sm"); got != "" {
				t.Fatalf("lookup failure created a branch: %s", got)
			}
		})
	}
}

func TestPublishRetryBranch(t *testing.T) {
	p := defaultProfile("copilot")
	p.Base, p.Checks = "main", []string{"true"}
	useProfile(t, p)
	repo := retryRepo(t)
	r := &Runner{repo: repo}
	old, _, err := r.worktreeAt(5, "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(old, "old.txt"), "keep staged", 0o644)
	testGit(t, old, "add", "-A")
	dir, branch, err := r.worktreeAt(5, "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "new.txt"), "new attempt", 0o644)
	testGit(t, dir, "add", "-A")
	args := filepath.Join(t.TempDir(), "gh-args")
	t.Setenv("SM_TEST_GH_ARGS", args)
	c := &Ctx{Repo: dir, Issue: 5, Branch: branch, Base: "main", Level: levelPR, Title: "task", ClosesIssue: true}
	if got := publish(c); got != Ok {
		t.Fatalf("publish = %v", got)
	}
	if got := testGit(t, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch); !strings.Contains(got, "refs/heads/sm/issue-5-1") {
		t.Fatalf("retry branch was not pushed: %s", got)
	}
	b, err := os.ReadFile(args)
	if err != nil || !strings.Contains(string(b), "--head\nsm/issue-5-1\n") {
		t.Fatalf("PR did not use selected branch: %s %v", b, err)
	}
	c.Merged = true
	cleanupWorktree(c)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("merged retry worktree was not cleaned up")
	}
	if got := testGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch); got != "" {
		t.Fatalf("merged retry branch remains: %s", got)
	}
	if testGit(t, old, "diff", "--cached", "--name-only") != "old.txt" {
		t.Fatal("cleanup changed the earlier attempt")
	}
}

func TestPublishRetryRefusesCollisions(t *testing.T) {
	p := defaultProfile("copilot")
	p.Base, p.Checks = "main", []string{"true"}
	useProfile(t, p)
	for _, mode := range []string{"remote collision", "changed branch"} {
		t.Run(mode, func(t *testing.T) {
			repo := retryRepo(t)
			r := &Runner{repo: repo}
			dir, branch, err := r.worktreeAt(5, "main")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(dir, "new.txt"), "candidate", 0o644)
			testGit(t, dir, "add", "-A")
			if mode == "remote collision" {
				testGit(t, repo, "push", "-q", "origin", "main:refs/heads/"+branch)
			} else {
				testGit(t, dir, "checkout", "-qb", "another-branch")
			}
			head := testGit(t, dir, "rev-parse", "HEAD")
			remote := testGit(t, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
			c := &Ctx{Repo: dir, Issue: 5, Branch: branch, Base: "main", Level: levelPR, Title: "task"}
			if publish(c) != Fail {
				t.Fatal("publication accepted a conflicting branch")
			}
			if got := testGit(t, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch); got != remote {
				t.Fatal("publication overwrote a remote branch")
			}
			if mode == "changed branch" && testGit(t, dir, "rev-parse", "HEAD") != head {
				t.Fatal("publication committed on a different branch")
			}
		})
	}
}

// No agent step may fall back to the CLI's default model, and the page's choice reaches the run.
func TestModelDefaultsAndChoice(t *testing.T) {
	p := defaultProfile("claude")
	p.Checks = []string{"true"}
	useProfile(t, p)
	for _, st := range steps {
		if st.Role != "" && st.Model == "" {
			t.Errorf("%s has no default model", st.Name)
		}
	}
	if m := agentModels(); m["review"] != opus || m["execute"] != sonnet || len(m) != 4 {
		t.Errorf("agentModels = %v", m)
	}
	t.Setenv("SM_MODEL_PLAN", "claude-haiku-5-5")
	t.Setenv("SM_CHECKS", "true")
	var err error
	settings, err = loadProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if m := agentModels()["plan"]; m != "claude-haiku-5-5" {
		t.Errorf("SM_MODEL_PLAN not honoured: %q", m)
	}
	env := childEnv(&Job{Issue: 1, Models: map[string]string{"plan": "claude-opus-5-5"}}, "s")
	last := ""
	for _, e := range env {
		if strings.HasPrefix(e, "SM_MODEL_PLAN=") {
			last = e
		}
	}
	if last != "SM_MODEL_PLAN=claude-opus-5-5" {
		t.Errorf("the page's model must win over the server's environment, got %q", last)
	}
}

// A REVISE list must reach the next review, which confirms each item; a first review gets none.
func TestReReviewGetsLastRevise(t *testing.T) {
	dir := t.TempDir()
	p := defaultProfile("claude")
	p.Transcripts = false
	useProfile(t, p)
	oldEvents := eventsPath
	eventsPath = filepath.Join(dir, "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })
	args, count := filepath.Join(dir, "args"), filepath.Join(dir, "count")
	writeFile(t, filepath.Join(dir, "claude"), `#!/bin/sh
n=$(cat '`+count+`' 2>/dev/null || echo 0); echo $((n+1)) > '`+count+`'
printf '%s\n' "$@" > '`+args+`'.$n
v='{"verdict":"PASS","reasons":[]}'; [ "$n" = 0 ] && v='{"verdict":"REVISE","reasons":["rename foo"]}'
printf '{"type":"result","is_error":false,"total_cost_usd":0.1,"structured_output":%s}\n' "$v"
`, 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	c := &Ctx{Repo: dir, Scratch: dir}
	if got := review(c); got != Fail || c.LastRevise != "- rename foo" {
		t.Fatalf("first review = %v, LastRevise %q", got, c.LastRevise)
	}
	if got := review(c); got != Pass {
		t.Fatalf("second review = %v", got)
	}
	first, _ := os.ReadFile(args + ".0")
	second, _ := os.ReadFile(args + ".1")
	if strings.Contains(string(first), "Re-review") || !strings.Contains(string(second), "Confirm each one was made:\n- rename foo") {
		t.Fatalf("re-review dispatch missing the last REVISE list:\nfirst: %s\nsecond: %s", tail(string(first), 300), tail(string(second), 300))
	}
}

// Queued jobs run at once, each in its own worktree, and one issue never runs twice at once.
func TestQueueRunsJobsConcurrently(t *testing.T) {
	repo := retryRepo(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\ncase \"$1\" in pr) echo '[]';; *) echo '{\"body\":\"\"}';; esac\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SM_SCRATCH_ROOT", filepath.Join(dir, "scratch"))
	log := filepath.Join(dir, "starts")
	t.Setenv("SM_TEST_STARTS", log)
	// Stand-in for agent-sm: waits until two jobs have started (so it fails unless they overlap),
	// then checks its worktree holds only its own work.
	self := filepath.Join(dir, "agent-sm")
	writeFile(t, self, `#!/bin/sh
wt="$1"; n="$2"; exit=ready
mkdir "$SM_TEST_STARTS.$n" 2>/dev/null || exit=duplicate
echo "$n" >> "$SM_TEST_STARTS"
echo "issue $n" > "$wt/mine-$n.txt"
i=0; while [ "$(wc -l < "$SM_TEST_STARTS")" -lt 2 ]; do i=$((i+1)); [ $i -gt 100 ] && exit=sequential && break; sleep 0.05; done
[ "$(ls "$wt" | grep -c '^mine-')" = 1 ] || exit=shared
[ "$(git -C "$wt" branch --show-current)" = "sm/issue-$n" ] || [ "$(git -C "$wt" branch --show-current)" = "sm/issue-$n-1" ] || exit=branch
sleep 0.2
rmdir "$SM_TEST_STARTS.$n"
printf '{"exit":"%s"}\n' "$exit" >> "$SM_EVENTS"
`, 0o755)
	p := defaultProfile("copilot")
	p.Base = "main"
	useProfile(t, p)
	r := &Runner{repo: repo, self: self, stats: filepath.Join(dir, "stats.jsonl"), slots: newSlots(3)}
	r.Enqueue([]Job{{Issue: 1, Profile: &p}, {Issue: 2, Profile: &p}, {Issue: 1, Profile: &p}})
	deadline := time.Now().Add(30 * time.Second)
	for {
		snap := r.Snapshot()
		if len(snap["running"].([]Job)) == 0 && len(snap["queued"].([]Job)) == 0 && len(snap["history"].([]Job)) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not finish: %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
	worktrees := map[string]bool{}
	for _, j := range r.Snapshot()["history"].([]Job) {
		if j.Exit != "ready" || j.Err != "" {
			t.Errorf("issue %d: exit %q err %q", j.Issue, j.Exit, j.Err)
		}
		worktrees[j.Worktree] = true
	}
	if len(worktrees) != 3 {
		t.Errorf("jobs shared a worktree: %v", worktrees)
	}
	if got := testGit(t, repo, "branch", "--show-current"); got != "main" {
		t.Errorf("main checkout switched to %s", got)
	}
	if m, _ := filepath.Glob(filepath.Join(repo, "mine-*")); len(m) != 0 {
		t.Errorf("a job wrote into the main checkout: %v", m)
	}
}

func TestSlotsResize(t *testing.T) {
	s := newSlots(1)
	s.acquire()
	got := make(chan struct{})
	go func() { s.acquire(); close(got) }()
	select {
	case <-got:
		t.Fatal("second job started with one slot")
	case <-time.After(50 * time.Millisecond):
	}
	s.resize(2)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("raising the limit did not start the waiting job")
	}
	s.resize(1) // lowering holds new jobs until enough finish
	s.release()
	if s.used != 1 || s.size() != 1 {
		t.Fatalf("used %d, size %d", s.used, s.size())
	}
}
