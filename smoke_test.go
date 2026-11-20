package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Live check of agent(): one real, cheap call that reads a file, so the activity feed has
// something to show. Run with SM_SMOKE=1 go test -run Smoke -v
func TestSmokeAgent(t *testing.T) {
	if os.Getenv("SM_SMOKE") == "" {
		t.Skip("set SM_SMOKE=1 to spend a few cents on a real agent call")
	}
	t.Setenv("SM_STATS", t.TempDir()+"/stats.jsonl")
	p := defaultProfile(os.Getenv("SM_PROVIDER"))
	if p.Provider == "" {
		p.Provider = "copilot"
	}
	p.Checks = []string{"true"}
	useProfile(t, p)
	var r struct{ Verdict string }
	c := &Ctx{Repo: t.TempDir(), Issue: 1, Scratch: t.TempDir()}
	old := steps[Review]
	defer func() { steps[Review] = old }()
	steps[Review].Schema = `{"type":"object","properties":{"verdict":{"enum":["PASS","FAIL"]}},"required":["verdict"],"additionalProperties":false}`
	steps[Review].Allow = "Read"
	if err := os.WriteFile(filepath.Join(c.Repo, "sample.txt"), []byte("smoke"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := agent(c, Review, "Read sample.txt in this checkout, then return verdict PASS.", &r) == Ok
	st, _ := os.ReadFile(filepath.Join(c.Scratch, "status.json"))
	t.Logf("ok=%v verdict=%q\nstatus.json:\n%s", ok, r.Verdict, st)
	if !ok {
		t.Fatal("agent returned invalid")
	}
}
