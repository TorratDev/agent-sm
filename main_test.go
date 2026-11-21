package main

import (
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

func TestResume(t *testing.T) {
	old := steps
	defer func() { steps = old }()
	steps[Review].Run = func(c *Ctx) Out { c.Verdict = "fine"; return Pass }
	steps[Publish].Run = func(*Ctx) Out { return Ok }
	dir := t.TempDir()
	saveCtx(&Ctx{Issue: 7, Scratch: dir, Repo: "old", Evidence: "kept"})

	c := &Ctx{Issue: 7, Scratch: dir, Repo: "new"}
	s, err := resume(c, "review")
	if err != nil || s != Review || c.Repo != "new" || c.Evidence != "kept" {
		t.Fatalf("resume = %v, %v, ctx %+v", s, err, c)
	}
	if e := run(c, s); e != Ready {
		t.Errorf("resumed run = %v, want ready", e)
	}
	again := &Ctx{Issue: 7, Scratch: dir}
	if _, err := resume(again, "publish"); err != nil || again.Verdict != "fine" {
		t.Errorf("run did not save its steps: %v, %+v", err, again)
	}
	if _, err := resume(&Ctx{Issue: 7, Scratch: dir}, "prepare"); err == nil {
		t.Error("--from prepare accepted")
	}
	if _, err := resume(&Ctx{Issue: 8, Scratch: dir}, "review"); err == nil {
		t.Error("another issue's ctx.json accepted")
	}
}

func TestReadRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "412")
	os.MkdirAll(dir, 0o755)
	ev := `{"state":"prepare","out":"ok","cost_usd":0,"t":"2026-11-20T10:00:00Z"}
{"state":"plan","out":"direct","cost_usd":0.1}
{"state":"execute","out":"ok","cost_usd":0.4}
{"state":"check","out":"fail","cost_usd":0.4}
`
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(ev), 0o644)
	os.WriteFile(filepath.Join(dir, "ctx.json"), []byte(`{"Title":"Retry queue jobs","Feedback":"check failed: go vet\nlots of output","Verdict":"review (REVISE):\n- add a test\n- more"}`), 0o644)

	rs, ok := readRun(dir, time.Now())
	if !ok || rs.Live != "execute" || rs.Exit != "" || rs.Stalled || len(rs.Steps) != 4 || rs.Cost != 0.4 ||
		rs.Title != "Retry queue jobs" || rs.Feedback != "check failed: go vet" || rs.Verdict != "review (REVISE): add a test" || rs.Started != "2026-11-20T10:00:00Z" {
		t.Fatalf("running run = %+v", rs)
	}
	if rs, _ := readRun(dir, time.Now().Add(stallAfter+time.Minute)); !rs.Stalled {
		t.Error("a run silent for longer than stallAfter is not stalled")
	}
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(ev+`{"event":"pr-opened","url":"https://x/pull/9"}
{"exit":"ready","cost_usd":0.9,"t":"2026-11-20T10:20:00Z"}
`), 0o644)
	if rs, _ := readRun(dir, time.Now().Add(time.Hour)); rs.Exit != "ready" || rs.Live != "" || rs.Stalled || rs.PR == "" || rs.Cost != 0.9 {
		t.Errorf("finished run = %+v", rs)
	}
	if _, ok := readRun(t.TempDir(), time.Now()); ok {
		t.Error("a directory without events.jsonl is not a run")
	}
	if live := liveState([]runStep{{"plan", "ok"}, {"investigate", "more"}}); live != "investigate" {
		t.Errorf("liveState after more = %q", live)
	}
}

func TestRunDirsFindsPageRuns(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, age time.Duration) string {
		dir := filepath.Join(root, rel)
		os.MkdirAll(dir, 0o755)
		f := filepath.Join(dir, "events.jsonl")
		os.WriteFile(f, []byte("{}\n"), 0o644)
		at := time.Now().Add(-age)
		os.Chtimes(f, at, at)
		return dir
	}
	cli := write("424", 0)
	write("repos/abc/570", time.Hour)
	retry := write("repos/abc/570/attempt-2", 0)
	write("repos/abc/notanissue", 0)
	got := runDirs(root)
	if len(got) != 2 || got["424"] != cli || got["570"] != retry {
		t.Fatalf("runDirs = %v", got)
	}
}

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

func TestWriteStatsRecordsTimeRepoAndSharedPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SM_STATS", "")
	t.Setenv("SM_SCRATCH_ROOT", root)
	writeStats(&Stats{Repo: "/src/app", Issue: 7})
	b, err := os.ReadFile(filepath.Join(root, "stats.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var st Stats
	if err := json.Unmarshal(b, &st); err != nil || st.Repo != "/src/app" || st.Issue != 7 {
		t.Fatalf("got %+v, %v", st, err)
	}
	if _, err := time.Parse(time.RFC3339, st.T); err != nil {
		t.Fatalf("t = %q: %v", st.T, err)
	}
}
