package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	named := map[State][]string{
		Plan:        {"gh issue view"},
		Investigate: {"ghidra", "grep", "~/venvs/desmume/bin/python", "Export JAVA_HOME"},
		Execute:     {"cargo fmt", "cargo clippy", "cargo test", "scripts/check-deps.sh", "~/venvs/desmume/bin/python"},
		Review:      {"git diff --cached", "grep", "ghidra", "~/venvs/desmume/bin/python", "Export JAVA_HOME"},
	}
	for s, cmds := range named {
		prompt := rolePrompt(s)
		for _, cmd := range cmds {
			if !strings.Contains(prompt, cmd) {
				t.Errorf("%v: %q is no longer in the prompt; update this test", s, cmd)
			}
			allowed := false
			for _, a := range strings.Split(steps[s].Allow, ",") {
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

// A stopped (or timed-out) session takes its child processes with it.
func TestAgentKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\nwait\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	ctx, cancel := context.WithCancel(context.Background())
	runCtx = ctx
	t.Cleanup(func() { runCtx = context.Background() })
	go func() {
		for {
			if _, err := os.Stat(pidFile); err == nil {
				time.Sleep(50 * time.Millisecond) // let the shell finish writing it
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	c := &Ctx{Repo: dir, Scratch: dir}
	var r struct{}
	if o := agent(c, Review, "x", &r); o == Ok {
		t.Fatalf("a killed session returned %v", o)
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("no child pid recorded")
	}
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child process %d outlived the killed session", pid)
	}
	if c.CostUnknown != 1 {
		t.Errorf("CostUnknown = %d, want 1 for a killed session", c.CostUnknown)
	}
}
