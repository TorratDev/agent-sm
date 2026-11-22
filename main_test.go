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
	for i, st := range steps[:numStates] { // Summarize sits outside the graph
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

func TestDismissRunHidesEveryCopy(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"359", "repos/abc/359/attempt-2", "repos/abc/570"} {
		os.MkdirAll(filepath.Join(root, rel), 0o755)
		os.WriteFile(filepath.Join(root, rel, "events.jsonl"), []byte("{}\n"), 0o644)
	}
	if err := dismissRun(root, 359); err != nil {
		t.Fatal(err)
	}
	if got := runDirs(root); len(got) != 1 || got["570"] == "" {
		t.Fatalf("runDirs after dismiss = %v", got)
	}
	if moved, _ := filepath.Glob(filepath.Join(root, "archive", "*", "repos", "abc", "359", "attempt-2", "events.jsonl")); len(moved) != 1 {
		t.Fatal("dismissed run was not kept under archive/")
	}
	if dismissRun(root, 359) == nil {
		t.Fatal("dismissing a gone run should fail")
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

func TestFollowUpReason(t *testing.T) {
	for _, tc := range []struct {
		c    Ctx
		exit Exit
		want bool
	}{
		{Ctx{}, SplitExit, true},
		{Ctx{Partial: true}, Ready, true},
		{Ctx{}, Ready, false},
		{Ctx{Rounds: maxExecute}, GaveUp, true},
		{Ctx{}, GaveUp, false},
		{Ctx{Partial: true}, Stopped, false},
	} {
		if got := followUpReason(&tc.c, tc.exit) != ""; got != tc.want {
			t.Errorf("followUpReason(%+v, %s) set = %v, want %v", tc.c, tc.exit, got, tc.want)
		}
	}
}

func TestFollowUpBodyFallsBackToRawNotes(t *testing.T) {
	useProfile(t, defaultProfile("copilot"))
	c := &Ctx{Scratch: t.TempDir(), Summary: "did a", Evidence: "open b", RunBudget: 1, CostUnknown: 1} // over budget: no session runs
	if got := followUpBody(c, "split"); !strings.Contains(got, "did a") || !strings.Contains(got, "open b") {
		t.Fatalf("followUpBody = %q", got)
	}
}

func TestEnqueueGroupsABatch(t *testing.T) {
	r := &Runner{working: true} // no loop: jobs stay queued
	r.Enqueue([]Job{{Issue: 5}, {Issue: 6}})
	r.Enqueue([]Job{{Issue: 6}, {Issue: 7}}) // added while the first batch waits: same batch
	if got := r.Snapshot()["batch"].([]int); !slices.Equal(got, []int{5, 6, 7}) {
		t.Fatalf("batch = %v", got)
	}
	r.queue = nil // everything finished
	r.Enqueue([]Job{{Issue: 9}})
	if got := r.Snapshot()["batch"].([]int); !slices.Equal(got, []int{9}) {
		t.Fatalf("new batch = %v", got)
	}
}

func TestPartialPRNamesItsFollowUpSubIssue(t *testing.T) {
	useProfile(t, defaultProfile("copilot"))
	dir := t.TempDir()
	log := filepath.Join(dir, "gh.log")
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
printf '%s|' "$@" >> "`+log+`"; echo >> "`+log+`"
case "$1 $2" in
  "issue create") echo 'https://github.com/example/repo/issues/77';;
  "api repos/{owner}/{repo}/issues/77") echo 9001;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SM_EPIC", "") // no epic: the follow-up is still filed
	c := &Ctx{Repo: dir, Scratch: dir, Issue: 5, Title: "t", Level: levelPR, Partial: true, PR: "42", RunBudget: 1, CostUnknown: 1}
	followUp(c, Ready)
	b, _ := os.ReadFile(log)
	got := string(b)
	for _, want := range []string{"Created by: PR #42, which delivers part of #5", "issues/5/sub_issues|-F|sub_issue_id=9001|", "pr|comment|42|--body|This PR delivers part of #5. The remaining work is in #77, a new sub-issue of #5."} {
		if !strings.Contains(got, want) {
			t.Fatalf("gh calls lack %q:\n%s", want, got)
		}
	}
}

func TestSplitCommentsOnTheIssueInsteadOfFilingACopy(t *testing.T) {
	useProfile(t, defaultProfile("copilot"))
	dir := t.TempDir()
	log := filepath.Join(dir, "gh.log")
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
printf '%s|' "$@" >> "`+log+`"; echo >> "`+log+`"
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &Ctx{Repo: dir, Scratch: dir, Issue: 5, Title: "t", Level: levelPR, Feedback: "Planner's breakdown:\n- piece one", RunBudget: 1, CostUnknown: 1}
	followUp(c, SplitExit)
	b, _ := os.ReadFile(log)
	got := string(b)
	if strings.Contains(got, "issue|create") || !strings.Contains(got, "issue|comment|5|") || !strings.Contains(got, "piece one") {
		t.Fatalf("a split must comment its breakdown on #5 and file nothing:\n%s", got)
	}
}

func TestPassReviewWithRemainingWorkMarksPartial(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dir, "opencode"), "#!/bin/sh\nprintf '%s' \"$SM_TEST_STDOUT\"\n", 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	p := defaultProfile("opencode")
	p.SessionLimit, p.RunLimit = 1, 5
	useProfile(t, p)
	oldEvents := eventsPath
	eventsPath = filepath.Join(scratch, "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })
	cost := 0.1
	for _, tc := range []struct {
		result  string
		partial bool
	}{
		{`{"verdict":"PASS","reasons":["ok"],"remaining":["read the copy-gate bounds"]}`, true},
		{`{"verdict":"PASS","reasons":["ok"],"remaining":[]}`, false},
		{`{"verdict":"PASS","reasons":["ok"]}`, false},
	} {
		t.Setenv("SM_TEST_STDOUT", opencodeOutput(t, tc.result, &cost))
		c := &Ctx{Repo: dir, Scratch: scratch, RunBudget: 5, Partial: true} // the executor said "partial": the review's list decides
		if got := review(c); got != Pass {
			t.Fatalf("%s: review = %v", tc.result, got)
		}
		if c.Partial != tc.partial || (tc.partial && !strings.Contains(c.Feedback, "copy-gate bounds")) {
			t.Fatalf("%s: Partial=%v Feedback=%q", tc.result, c.Partial, c.Feedback)
		}
		if tc.partial && followUpReason(c, Ready) == "" {
			t.Fatal("a partial PASS must need a follow-up")
		}
	}
}

func TestDiagnosis(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"exit":"errored"}`+"\n"), 0o644)
	// a second click while an audit runs starts no second session
	diagnosing.Store(dir, true)
	if _, err := diagnoseRun(dir, dir, "errored", settings); err == nil {
		t.Fatal("a second audit of the same run started")
	}
	diagnosing.Delete(dir)
	// the audit uses the agent the run recorded last, not the server's default
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{\"provider\":\"copilot\"}\nnot json\n{\"exit\":\"errored\",\"provider\":\"claude\"}\n"), 0o644)
	if p := runProvider(dir); p != "claude" {
		t.Fatalf("runProvider = %q, want claude", p)
	}
	// a diagnosis on disk shows on the board, and one already filed is not filed again (no gh call)
	os.WriteFile(filepath.Join(dir, "diagnosis.json"), []byte(`{"exit":"errored","kind":"bug","title":"t","cause":"c","fix":"f","body":"b","issue":"https://x/issues/3"}`), 0o644)
	if rs, _ := readRun(dir, time.Now()); rs.Diagnosis == nil || rs.Diagnosis.Kind != "bug" {
		t.Fatalf("readRun diagnosis = %+v", rs.Diagnosis)
	}
	if url, err := fileDiagnosis(dir, "/nonexistent", 1); err != nil || url != "https://x/issues/3" {
		t.Fatalf("fileDiagnosis = %q, %v", url, err)
	}
}
