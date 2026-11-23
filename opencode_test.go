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

func opencodeEvent(v any) string {
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

func opencodeOutput(t *testing.T, content string, cost *float64) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(opencodeEvent(map[string]any{"type": "step_start", "sessionID": "ses_test"}))
	b.WriteString(opencodeEvent(map[string]any{"type": "text", "sessionID": "ses_test",
		"part": map[string]any{"id": "part_1", "messageID": "msg_1", "type": "text", "text": content}}))
	finish := map[string]any{"id": "part_2", "messageID": "msg_1", "type": "step-finish", "reason": "stop",
		"tokens": map[string]any{"input": 12, "output": 3, "cache": map[string]int{"read": 4, "write": 5}}}
	if cost != nil {
		finish["cost"] = *cost
	}
	b.WriteString(opencodeEvent(map[string]any{"type": "step_finish", "sessionID": "ses_test", "part": finish}))
	return b.String()
}

func TestOpencodeArgsAndConfig(t *testing.T) {
	p := defaultProfile("opencode")
	p.Checks = []string{"true"}
	p.Commands = map[string][]string{"execute": {"dotnet test"}}
	useProfile(t, p)

	args := strings.Join(opencodeArgs(Execute, "task", "opencode/beam-free#high"), "\n")
	for _, want := range []string{"--standalone", "--format\njson", "--agent\nagent-sm",
		"--model\nopencode/beam-free#high", "Required schema", "task"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}

	for _, tc := range []struct {
		state       State
		edit        bool
		wantCommand string
	}{
		{Plan, false, "git log *"},
		{Execute, true, "git diff *"},
		{Review, false, "git diff *"},
	} {
		config := opencodeConfig(tc.state)
		agents, ok := config["agents"].(map[string]any)
		if !ok {
			t.Fatal("missing agents object")
		}
		agent, ok := agents["agent-sm"].(map[string]any)
		if !ok {
			t.Fatal("missing agent-sm definition")
		}
		permissions, ok := agent["permissions"].([]map[string]string)
		if !ok || len(permissions) == 0 || permissions[0]["effect"] != "deny" {
			t.Fatalf("%v permissions = %#v", tc.state, agent["permissions"])
		}
		var actions, resources []string
		for _, rule := range permissions {
			if rule["effect"] == "allow" {
				actions = append(actions, rule["action"])
				resources = append(resources, rule["resource"])
			}
		}
		if slices.Contains(actions, "edit") != tc.edit || slices.Contains(actions, "write") != tc.edit {
			t.Fatalf("%v edit permissions = %v", tc.state, actions)
		}
		if !slices.Contains(resources, tc.wantCommand) {
			t.Fatalf("%v shell resources = %v", tc.state, resources)
		}
		if tc.state == Execute && !slices.Contains(resources, "dotnet test *") {
			t.Fatalf("execute command resource missing: %v", resources)
		}
	}
}

func TestCopilotModelOptions(t *testing.T) {
	oldModels, oldAt := copilotCatalog.models, copilotCatalog.at
	t.Cleanup(func() { copilotCatalog.models, copilotCatalog.at = oldModels, oldAt })
	copilotCatalog.models, copilotCatalog.at = nil, time.Time{}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "copilot"), "#!/bin/sh\n[ \"$1 $2\" = \"help config\" ] || exit 2\n"+
		"printf '  `logLevel`: x\\n    - \"not-a-model\"\\n\\n  `model`: AI model to use.\\n    - \"gpt-5.5\"\\n    - \"claude-opus-5\"\\n\\n  `contextTier`: y\\n    - \"default\"\\n'\n", 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if got := modelOptions("copilot"); !slices.Equal(got, []string{"auto", "claude-opus-5", "gpt-5.5"}) {
		t.Fatalf("Copilot options = %v", got)
	}
}

func TestOpencodeModelOptions(t *testing.T) {
	if got := modelOptions("claude"); !slices.Contains(got, sonnet) || !slices.Contains(got, opus) {
		t.Fatalf("Claude options = %v", got)
	}
	oldModels, oldAt := opencodeCatalog.models, opencodeCatalog.at
	t.Cleanup(func() { opencodeCatalog.models, opencodeCatalog.at = oldModels, oldAt })
	opencodeCatalog.models, opencodeCatalog.at = nil, time.Time{}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "opencode"), "#!/bin/sh\n[ \"$1\" = models ] || exit 2\nprintf 'opencode/other\\nopencode/jev-1.13-free\\nopencode/glm-5.3-flash\\n'\n", 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	got := modelOptions("opencode")
	if !slices.Contains(got, opencodeDefaultModel) || !slices.Contains(got, opencodeDecisionModel) ||
		!slices.Contains(got, "opencode/other") {
		t.Fatalf("OpenCode options = %v", got)
	}
	// Jev is a decision model, not an agent: plan skips it even when it is listed.
	if model := opencodeModelFor(steps[Plan]); model != opencodeDecisionModel {
		t.Fatalf("decision model = %q, want %q", model, opencodeDecisionModel)
	}
	if model := opencodeModelFor(steps[Execute]); model != opencodeDefaultModel {
		t.Fatalf("working model = %q, want %q", model, opencodeDefaultModel)
	}

	// Review also uses OpenCode's GLM Flash when it is installed.
	opencodeCatalog.models, opencodeCatalog.at = nil, time.Time{}
	writeFile(t, filepath.Join(dir, "opencode"), "#!/bin/sh\n[ \"$1\" = models ] || exit 2\nprintf 'opencode/glm-5.3-flash\n'\n", 0o755)
	if model := opencodeModelFor(steps[Review]); model != opencodeDecisionModel {
		t.Fatalf("GLM decision model = %q, want %q", model, opencodeDecisionModel)
	}
	if got := opencodeModelID("glm-5.3-flash"); got != opencodeDecisionModel {
		t.Fatalf("GLM alias = %q, want %q", got, opencodeDecisionModel)
	}
}

func TestOpencodeAgentResults(t *testing.T) {
	pass := `{"verdict":"PASS","reasons":[]}`
	cost := func(n float64) *float64 { return &n }
	for _, tc := range []struct {
		name    string
		stdout  string
		mode    string
		want    Out
		unknown int
		cost    float64
	}{
		{"valid", opencodeOutput(t, pass, cost(0.1)), "", Ok, 0, 0.1},
		{"unknown usage", opencodeOutput(t, pass, nil), "", Ok, 1, 0},
		{"incomplete result", opencodeOutput(t, `{}`, cost(0.1)), "", Invalid, 0, 0.1},
		{"error event", opencodeEvent(map[string]any{"type": "error", "error": map[string]string{"message": "quota"}}), "", Invalid, 0, 0},
		{"process failure", opencodeOutput(t, pass, cost(0.1)), "failure", Invalid, 0, 0.1},
		{"budget", opencodeOutput(t, pass, cost(1)), "", OverBudget, 0, 1},
		{"timeout", "", "timeout", Timeout, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "opencode"), `#!/bin/sh
printf '%s' "$SM_TEST_STDOUT"
printf 'diagnostic\n' >&2
if [ "$SM_TEST_MODE" = "timeout" ]; then sleep 300; fi
if [ "$SM_TEST_MODE" = "failure" ]; then exit 1; fi
`, 0o755)
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("SM_TEST_STDOUT", tc.stdout)
			t.Setenv("SM_TEST_MODE", tc.mode)
			t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
			p := defaultProfile("opencode")
			p.SessionLimit, p.RunLimit = 1, 5
			if tc.mode == "timeout" {
				p.TimeoutMinutes = .002
			}
			useProfile(t, p)
			c := &Ctx{Repo: dir, Scratch: dir, RunBudget: 5}
			var r struct {
				Verdict string
				Reasons []string
			}
			if got := agent(c, Review, "bounded test", &r); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if c.CostUnknown != tc.unknown || c.Cost != tc.cost {
				t.Fatalf("cost = %v unknown = %d, want %v / %d", c.Cost, c.CostUnknown, tc.cost, tc.unknown)
			}
			if tc.want == Ok && (r.Verdict != "PASS" || len(r.Reasons) != 0) {
				t.Fatalf("decoded result = %+v", r)
			}
		})
	}
}

func TestOpencodeQuotaError(t *testing.T) {
	for _, detail := range []string{
		"OpenCode reported an error: Free usage limit reached. Connect a provider or subscribe to OpenCode Go to keep going, or switch models.",
		"rate limit exceeded",
		"429 Too Many Requests",
		"quota",
	} {
		if !opencodeQuotaError(detail) {
			t.Errorf("%q should be retryable", detail)
		}
	}
	for _, detail := range []string{"", "model not found", "permission denied", "invalid api key"} {
		if opencodeQuotaError(detail) {
			t.Errorf("%q should not be retryable", detail)
		}
	}
}

func TestOpencodeFallbackOnQuota(t *testing.T) {
	oldModels, oldAt := opencodeCatalog.models, opencodeCatalog.at
	t.Cleanup(func() { opencodeCatalog.models, opencodeCatalog.at = oldModels, oldAt })
	opencodeCatalog.models, opencodeCatalog.at = nil, time.Time{}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "opencode"), `#!/bin/sh
if [ "$1" = "models" ]; then
  printf 'opencode/deepseek-v4.1-flash-free\nnvidia/z-ai/glm-5.3-flash\n'
  exit 0
fi
model=
prev=
for arg in "$@"; do
  if [ "$prev" = "--model" ]; then model=$arg; fi
  prev=$arg
done
case "$model" in
  *-free) printf '%s' "$SM_QUOTA" ;;
  *) printf '%s' "$SM_PASS" ;;
esac
`, 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_QUOTA", opencodeEvent(map[string]any{"type": "error", "error": map[string]string{
		"message": "Free usage limit reached. Connect a provider or subscribe to OpenCode Go to keep going, or switch models."}}))
	t.Setenv("SM_PASS", opencodeOutput(t, `{"verdict":"PASS","reasons":[]}`, func() *float64 { n := .05; return &n }()))
	statsPath := filepath.Join(dir, "stats.jsonl")
	t.Setenv("SM_STATS", statsPath)
	p := defaultProfile("opencode")
	p.SessionLimit, p.RunLimit = 1, 5
	useProfile(t, p)
	c := &Ctx{Repo: dir, Scratch: dir, RunBudget: 5}
	var r struct {
		Verdict string
		Reasons []string
	}
	if got := agent(c, Review, "fallback test", &r); got != Ok {
		t.Fatalf("agent = %v", got)
	}
	if r.Verdict != "PASS" || c.Cost != .05 || c.CostUnknown != 0 {
		t.Fatalf("result = %+v, cost = %v, unknown = %d", r, c.Cost, c.CostUnknown)
	}
	b, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var st Stats
		if err := json.Unmarshal([]byte(line), &st); err != nil {
			t.Fatal(err)
		}
		models = append(models, st.Model)
	}
	if !slices.Equal(models, []string{opencodeDefaultModel, "nvidia/z-ai/glm-5.3-flash"}) {
		t.Fatalf("attempt models = %v", models)
	}
}

func TestOpencodeFallbackSkipsFreeModels(t *testing.T) {
	oldModels, oldAt := opencodeCatalog.models, opencodeCatalog.at
	t.Cleanup(func() { opencodeCatalog.models, opencodeCatalog.at = oldModels, oldAt })
	opencodeCatalog.models = []string{
		"opencode/deepseek-v4.1-flash-free", "opencode/beam-free", "nvidia/z-ai/glm-5.3-flash",
	}
	opencodeCatalog.at = time.Now()
	if got, ok := opencodeFallbackModel([]string{"opencode/deepseek-v4.1-flash-free"}, "free usage limit reached"); !ok || got != "nvidia/z-ai/glm-5.3-flash" {
		t.Fatalf("free fallback = %q, %v", got, ok)
	}
	// A rate limit may move to another free model; only a free-usage failure skips them.
	opencodeCatalog.models = []string{"opencode/deepseek-v4.1-flash-free", "opencode/beam-free"}
	if got, ok := opencodeFallbackModel([]string{"opencode/deepseek-v4.1-flash-free"}, "rate limit"); !ok || got != "opencode/beam-free" {
		t.Fatalf("rate-limit fallback = %q, %v", got, ok)
	}
	if got, ok := opencodeFallbackModel([]string{"opencode/deepseek-v4.1-flash-free"}, "free usage limit"); ok {
		t.Fatalf("exhausted fallback = %q", got)
	}
}

func TestOpencodeTranscript(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dir, "opencode"), "#!/bin/sh\nprintf '%s' \"$SM_TEST_STDOUT\"\n", 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_TEST_STDOUT", opencodeOutput(t, `{"verdict":"PASS","reasons":[]}`, func() *float64 { n := 0.1; return &n }()))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	p := defaultProfile("opencode")
	p.SessionLimit, p.RunLimit, p.Transcripts = 1, 5, true
	useProfile(t, p)
	oldEvents := eventsPath
	eventsPath = filepath.Join(scratch, "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })
	c := &Ctx{Repo: dir, Scratch: scratch, RunBudget: 5}
	if got := agent(c, Review, "prompt", new(any)); got != Ok {
		t.Fatalf("agent = %v", got)
	}
	entries, err := os.ReadDir(filepath.Join(scratch, "transcripts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("transcripts = %v, %v", entries, err)
	}
	capture := filepath.Join(scratch, "transcripts", entries[0].Name())
	config, err := os.ReadFile(filepath.Join(capture, "opencode.json"))
	if err != nil || !strings.Contains(string(config), `"agent-sm"`) || !strings.Contains(string(config), `"effect":"deny"`) {
		t.Fatalf("generated config = %s, %v", config, err)
	}
	info, err := os.Stat(filepath.Join(capture, "opencode.json"))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config permissions = %v, %v", info, err)
	}
	request, err := os.ReadFile(filepath.Join(capture, "request.json"))
	if err != nil || !strings.Contains(string(request), "--standalone") || !strings.Contains(string(request), "prompt") {
		t.Fatalf("request = %s, %v", request, err)
	}
}

func TestOpencodeDefaultsAndModelIDs(t *testing.T) {
	oldModels, oldAt := opencodeCatalog.models, opencodeCatalog.at
	t.Cleanup(func() { opencodeCatalog.models, opencodeCatalog.at = oldModels, oldAt })
	opencodeCatalog.models, opencodeCatalog.at = nil, time.Time{}
	t.Setenv("PATH", t.TempDir()) // no OpenCode catalog: use the deterministic defaults
	p := defaultProfile("opencode")
	if p.SessionLimit != 2 || p.RunLimit != 20 || p.unit() != "USD" ||
		p.model(steps[Plan]) != opencodeDefaultModel || p.model(steps[Execute]) != opencodeDefaultModel {
		t.Fatalf("OpenCode defaults = %+v, unit %q, plan %q, execute %q", p, p.unit(), p.model(steps[Plan]), p.model(steps[Execute]))
	}
	p.Checks = []string{"true"}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{opencodeDefaultModel, "opencode/beam-free#high", "claude-sonnet-5-5"} {
		if !modelRe.MatchString(model) {
			t.Errorf("model id rejected: %q", model)
		}
	}
	for _, model := range []string{"", "-bad", "with space", "UPPER"} {
		if modelRe.MatchString(model) {
			t.Errorf("invalid model id accepted: %q", model)
		}
	}
}

func TestAutoProfileOpenCode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "opencode"), "#!/bin/sh\n", 0o755)
	t.Setenv("PATH", dir)
	t.Setenv("SM_PROVIDER", "")
	p, err := autoProfile(t.TempDir())
	if err != nil || p.Provider != "opencode" || p.SessionLimit != 2 || p.RunLimit != 20 || p.unit() != "USD" {
		t.Fatalf("automatic OpenCode profile = %+v, %v", p, err)
	}
}
