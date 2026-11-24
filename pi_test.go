package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// piOutput is the JSONL `pi --mode json` prints for one assistant answer costing cost USD.
func piOutput(t *testing.T, result string, cost *float64) string {
	t.Helper()
	msg := map[string]any{"role": "assistant", "stopReason": "stop",
		"content": []map[string]any{{"type": "text", "text": result}},
		"usage":   map[string]any{"cost": map[string]any{"total": *cost}}}
	var b strings.Builder
	for _, ev := range []map[string]any{
		{"type": "session", "id": "test-session"},
		{"type": "agent_start"},
		{"type": "turn_end", "message": msg},
		{"type": "agent_end"},
	} {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestPiArgsGiveOnlyEditStepsWriteTools(t *testing.T) {
	read := strings.Join(piArgs(Plan, "opencode-go/glm-5.3-flash", "s.md", "p.md"), " ")
	if !strings.Contains(read, "--tools read,grep,find,ls,bash ") || strings.Contains(read, "edit") {
		t.Fatalf("read-only step args: %s", read)
	}
	if !strings.Contains(read, "--model opencode-go/glm-5.3-flash") || !strings.HasSuffix(read, "@p.md") {
		t.Fatalf("model or message missing: %s", read)
	}
	edit := strings.Join(piArgs(Execute, piWorkModel, "s.md", "p.md"), " ")
	if !strings.Contains(edit, "--tools read,grep,find,ls,bash,edit,write ") {
		t.Fatalf("edit step args: %s", edit)
	}
}

func TestPiAgentReadsResultAndCost(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dir, "pi"), "#!/bin/sh\nprintf '%s' \"$SM_TEST_STDOUT\"\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	p := defaultProfile("pi")
	p.SessionLimit, p.RunLimit = 1, 5
	useProfile(t, p)
	oldEvents := eventsPath
	eventsPath = filepath.Join(scratch, "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })

	cost := 0.25
	t.Setenv("SM_TEST_STDOUT", piOutput(t, `{"verdict":"PASS","reasons":["ok"]}`, &cost))
	var r struct {
		Verdict string
		Reasons []string
	}
	c := &Ctx{Repo: dir, Scratch: scratch, Issue: 1}
	if got := agent(c, Review, "review", &r); got != Ok || r.Verdict != "PASS" {
		t.Fatalf("agent = %v, verdict %q", got, r.Verdict)
	}
	if c.Cost != cost {
		t.Fatalf("cost = %v, want %v", c.Cost, cost)
	}
}

func TestPiModelCatalogCoversEveryProvider(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range piModels {
		prefix, _, _ := strings.Cut(m, "/")
		seen[prefix] = true
	}
	for _, want := range []string{"anthropic", "opencode-go", "opencode", "nvidia", "github-copilot"} {
		if !seen[want] {
			t.Errorf("no %s models in the catalog", want)
		}
	}
	if !slices.Contains(piModels, piWorkModel) || !slices.Contains(piModels, piDecisionModel) {
		t.Fatal("default models are not in the catalog")
	}
}
