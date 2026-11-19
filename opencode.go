package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// opencodeAgent runs one `opencode run` session with a generated agent definition
// that maps agent-sm's per-state permission list onto OpenCode's rules.
func opencodeAgent(c *Ctx, state State, prompt string, out any, transcript *agentTranscript) (res Out) {
	step := steps[state]
	limit := settings.SessionLimit
	if c.RunBudget > 0 {
		if c.CostUnknown > 0 {
			return OverBudget
		}
		limit = min(limit, c.RunBudget-c.Cost)
		if limit <= 0 {
			logEvent(map[string]any{"event": "run-budget-reached", "err": "the run budget is exhausted"})
			return OverBudget
		}
	}
	ctx, cancel := context.WithTimeout(runCtx, time.Duration(settings.TimeoutMinutes*float64(time.Minute)))
	defer cancel()

	var dir string
	var err error
	if transcript != nil {
		dir = transcript.dir
	} else {
		dir, err = os.MkdirTemp(c.Scratch, "opencode-")
		if err != nil {
			logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
			return Invalid
		}
		defer os.RemoveAll(dir)
	}
	configPath, err := writeOpencodeConfig(dir, state)
	if err != nil {
		logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
		return Invalid
	}
	args := opencodeArgs(state, prompt, opencodeModelID(stepModel(step)))
	if transcript != nil {
		if err := transcript.request(args, state, cancel); err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			return Invalid
		}
	}
	cmd := process(ctx, c.Repo, "opencode", args...)
	cmd.Env = append(cleanAgentEnv(), "OPENCODE_CONFIG="+configPath,
		"OPENCODE_CONFIG_PROJECT_DISABLE=1", "OPENCODE_DISABLE_AUTOUPDATE=1")
	diagnostics := &boundedOutput{limit: 64 << 10}
	cmd.Stderr = diagnostics
	if transcript != nil {
		cmd.Stderr = io.MultiWriter(transcript.stderr, diagnostics)
	}
	stdout, perr := cmd.StdoutPipe()
	if perr == nil {
		perr = cmd.Start()
	}
	if perr != nil {
		logEvent(map[string]any{"event": "agent-error", "err": perr.Error()})
		return Invalid
	}

	st := &Stats{Provider: "opencode", Issue: c.Issue, State: state.String(), Model: opencodeModelID(stepModel(step)), ToolCalls: map[string]int{}, BashCmds: map[string]int{}}
	if state == Investigate && len(c.Units) > 0 {
		st.Unit = c.Units[c.UnitIdx].ID
	}
	start := time.Now()
	prog := &progress{c: c, state: state, unit: st.Unit, start: start}
	var reader io.Reader = stdout
	if transcript != nil {
		reader = io.TeeReader(stdout, transcript.stdout)
	}
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 1<<20), 64<<20)

	var result []byte
	var agentErr error
	usageReported, usageSteps := true, 0
	type textPart struct {
		id, text string
	}
	var parts []textPart
	index := map[string]int{}
	resetParts := func() {
		parts = parts[:0]
		clear(index)
	}
	for sc.Scan() {
		line := sc.Bytes()
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Error     struct {
				Message string `json:"message"`
			} `json:"error"`
			Part struct {
				ID        string   `json:"id"`
				MessageID string   `json:"messageID"`
				Type      string   `json:"type"`
				Text      string   `json:"text"`
				Tool      string   `json:"tool"`
				Reason    string   `json:"reason"`
				Cost      *float64 `json:"cost"`
				State     struct {
					Status string `json:"status"`
					Error  string `json:"error"`
					Input  struct {
						Command  string `json:"command"`
						Path     string `json:"path"`
						FilePath string `json:"filePath"`
						Pattern  string `json:"pattern"`
					} `json:"input"`
				} `json:"state"`
				Tokens struct {
					Input  int `json:"input"`
					Output int `json:"output"`
					Cache  struct {
						Read  int `json:"read"`
						Write int `json:"write"`
					} `json:"cache"`
				} `json:"tokens"`
			} `json:"part"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.SessionID != "" {
			st.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "step_start":
			resetParts()
		case "text":
			if ev.Part.Text == "" {
				continue
			}
			if i, ok := index[ev.Part.ID]; ok {
				parts[i].text = ev.Part.Text
			} else {
				index[ev.Part.ID] = len(parts)
				parts = append(parts, textPart{id: ev.Part.ID, text: ev.Part.Text})
			}
			prog.add("say", ev.Part.Text)
		case "tool_use":
			if ev.Part.Tool == "" {
				continue
			}
			st.ToolCalls[ev.Part.Tool]++
			in := toolInput{Command: ev.Part.State.Input.Command, Path: ev.Part.State.Input.Path,
				FilePath: ev.Part.State.Input.FilePath, Pattern: ev.Part.State.Input.Pattern}
			if in.Command != "" {
				for _, w := range cmdWords(in.Command) {
					st.BashCmds[w]++
				}
			}
			prog.tool(ev.Part.Tool, in)
			if ev.Part.State.Status == "error" && strings.Contains(ev.Part.State.Error, "Permission denied") {
				st.Denials++
				if st.DeniedTools == nil {
					st.DeniedTools = map[string]int{}
				}
				st.DeniedTools[ev.Part.Tool]++
				if len(st.DeniedSample) < 5 {
					st.DeniedSample = append(st.DeniedSample, ev.Part.Tool+" "+tail(in.Command+" "+in.Path+" "+in.FilePath+" "+in.Pattern, 200))
				}
			}
		case "step_finish":
			st.Turns++
			usageSteps++
			if ev.Part.Cost == nil || !finiteNonnegative(*ev.Part.Cost) {
				usageReported = false
			} else {
				st.CostUSD += *ev.Part.Cost
			}
			st.InputTokens += ev.Part.Tokens.Input
			st.OutputTokens += ev.Part.Tokens.Output
			st.CacheCreation += ev.Part.Tokens.Cache.Write
			st.CacheRead += ev.Part.Tokens.Cache.Read
			if ev.Part.Reason == "stop" {
				var b strings.Builder
				for _, p := range parts {
					b.WriteString(p.text)
				}
				result = []byte(strings.TrimSpace(b.String()))
			}
		case "error":
			msg := ev.Error.Message
			if msg == "" {
				msg = tail(string(line), 1500)
			}
			agentErr = fmt.Errorf("OpenCode reported an error: %s", msg)
		}
	}
	if sc.Err() != nil {
		cancel()
	}
	runErr := cmd.Wait()
	if err := sc.Err(); err != nil {
		logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
		runErr = err
	}
	st.DurationMs = int(time.Since(start).Milliseconds())
	st.Killed = ctx.Err() != nil && len(result) == 0
	prog.flush(true)
	c.Cost += st.CostUSD
	st.UsageUnknown = !usageReported || usageSteps == 0
	if st.UsageUnknown {
		c.CostUnknown++
	}
	defer func() {
		st.Outcome, st.OK = res.String(), res == Ok
		writeStats(st)
		logEvent(map[string]any{"event": "agent", "stats": st})
	}()
	if st.CostUSD >= limit {
		return OverBudget
	}
	if ctx.Err() == context.DeadlineExceeded && len(result) == 0 {
		return Timeout
	}
	if runErr != nil || agentErr != nil || len(result) == 0 {
		detail := ""
		if agentErr != nil {
			detail = agentErr.Error()
		} else if runErr != nil {
			detail = runErr.Error()
		}
		logEvent(map[string]any{"event": "agent-error", "err": detail, "stderr": tail(diagnostics.String(), 1500)})
		return Invalid
	}
	if err := validateResult(result, step.Schema, out); err != nil {
		logEvent(map[string]any{"event": "invalid-result", "err": err.Error()})
		return Invalid
	}
	st.Result = tail(string(result), 1500)
	return Ok
}

// opencodeArgs mirrors copilotArgs: the prompt carries the schema because OpenCode
// has no structured-output flag, and the final answer is validated locally.
func opencodeArgs(state State, prompt, model string) []string {
	prompt = rolePrompt(state) + "\n\n" + prompt + "\n\nReturn exactly one JSON object, without markdown or other text. Required schema:\n" + steps[state].Schema
	return []string{"run", "--standalone", "--archived", "--format", "json",
		"--agent", "agent-sm", "--model", model, prompt}
}

func writeOpencodeConfig(dir string, state State) (string, error) {
	b, err := json.Marshal(opencodeConfig(state))
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "opencode.json")
	return path, os.WriteFile(path, append(b, '\n'), 0o600)
}

// opencodeConfig denies everything, then allows the same read-only tools and shell
// command prefixes that Claude and Copilot receive for the state. Writes are enabled
// only for steps that may edit files.
func opencodeConfig(state State) map[string]any {
	allow := func(action, resource string) map[string]string {
		return map[string]string{"action": action, "resource": resource, "effect": "allow"}
	}
	permissions := []map[string]string{{"action": "*", "resource": "*", "effect": "deny"}}
	for _, action := range []string{"read", "glob", "grep", "list"} {
		permissions = append(permissions, allow(action, "*"))
	}
	permissions = append(permissions, allow("todowrite", "*"))
	if steps[state].Edit {
		for _, action := range []string{"edit", "write"} {
			permissions = append(permissions, allow(action, "*"))
		}
	}
	for _, entry := range strings.Split(configuredAllow(state), ",") {
		prefix, ok := strings.CutPrefix(strings.TrimSpace(entry), "Bash(")
		if !ok {
			continue
		}
		prefix = strings.TrimSuffix(prefix, ":*)")
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			permissions = append(permissions, allow("shell", prefix+" *"))
		}
	}
	return map[string]any{
		"agents": map[string]any{
			"agent-sm": map[string]any{
				"mode":        "primary",
				"description": "agent-sm workflow step",
				"permissions": permissions,
			},
		},
	}
}

var opencodeCatalog struct {
	sync.Mutex
	at     time.Time
	models []string
}

// opencodeModels returns the installed OpenCode CLI's model IDs. The catalog is
// cached so the page's queue polling does not spawn a process every few seconds.
func opencodeModels() []string {
	opencodeCatalog.Lock()
	defer opencodeCatalog.Unlock()
	if !opencodeCatalog.at.IsZero() && time.Since(opencodeCatalog.at) < 5*time.Minute {
		return append([]string(nil), opencodeCatalog.models...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", "models")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		opencodeCatalog.at = time.Now()
		return append([]string(nil), opencodeCatalog.models...)
	}
	var models []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && modelRe.MatchString(line) && !seen[line] {
			seen[line] = true
			models = append(models, line)
		}
	}
	sort.Strings(models)
	opencodeCatalog.models, opencodeCatalog.at = models, time.Now()
	return append([]string(nil), models...)
}

// opencodeModelFor picks the default OpenCode model for a step. DeepSeek Flash is
// the safe working model. Plan and review prefer jev-1.13-free, then OpenCode's
// GLM Flash, when the installed catalog offers them.
func opencodeModelFor(st Step) string {
	switch st.Name {
	case "plan", "review":
		models := opencodeModels()
		for _, model := range models {
			if model == opencodeJevModel || strings.HasSuffix(model, "/jev-1.13-free") {
				return model
			}
		}
		for _, model := range models {
			if model == opencodeDecisionModel {
				return model
			}
		}
	}
	return opencodeDefaultModel
}

// opencodeModelID accepts the short names operators commonly use in profiles.
func opencodeModelID(model string) string {
	switch model {
	case "deepseek-v4.1-flash-free":
		return opencodeDefaultModel
	case "glm-5.3-flash":
		return opencodeDecisionModel
	case "jev-1.13-free":
		return opencodeJevModel
	}
	return model
}

// modelOptions is the suggestion list for the page's per-step model inputs.
// The inputs remain free text so a newly released model still works.
func modelOptions(provider string) []string {
	switch provider {
	case "claude":
		return []string{sonnet, opus, "claude-haiku-5-5"}
	case "opencode":
		models := []string{opencodeDefaultModel, opencodeDecisionModel}
		seen := map[string]bool{opencodeDefaultModel: true, opencodeDecisionModel: true}
		for _, m := range opencodeModels() {
			if !seen[m] {
				seen[m] = true
				models = append(models, m)
			}
		}
		return models
	default:
		return []string{"auto"}
	}
}
