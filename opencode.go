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

// maxOpencodeFallbacks bounds how many catalog models one step tries after a
// free-tier quota or rate-limit error, so a stale catalog cannot turn one step
// into a sweep.
const maxOpencodeFallbacks = 3

// opencodeAgent runs one `opencode run` session with a generated agent definition
// that maps agent-sm's per-state permission list onto OpenCode's rules. When
// OpenCode refuses the selected model because a free allowance or a rate limit is
// spent, the next catalog model is tried instead of failing the step outright.
func opencodeAgent(c *Ctx, state State, prompt string, out any, transcript *agentTranscript) Out {
	model := opencodeModelID(stepModel(steps[state]))
	tried := make([]string, 0, maxOpencodeFallbacks+1)
	for {
		// Every attempt gets its own session budget, bounded by what the run has left.
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
		tried = append(tried, model)
		res, quota := opencodeAttempt(c, state, prompt, model, limit, out, transcript)
		if quota == "" || len(tried) > maxOpencodeFallbacks {
			return res
		}
		next, ok := opencodeFallbackModel(tried, quota)
		if !ok {
			return res
		}
		logEvent(map[string]any{"event": "opencode-model-fallback", "issue": c.Issue,
			"state": state.String(), "from": model, "to": next, "reason": quota})
		model = next
	}
}

// opencodeAttempt runs one `opencode run` session. A non-empty quota detail means
// the failure is a free-usage or rate-limit refusal that another model may avoid.
func opencodeAttempt(c *Ctx, state State, prompt, model string, limit float64, out any, transcript *agentTranscript) (res Out, quota string) {
	step := steps[state]
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
			return Invalid, ""
		}
		defer os.RemoveAll(dir)
	}
	configPath, err := writeOpencodeConfig(dir, state)
	if err != nil {
		logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
		return Invalid, ""
	}
	args := opencodeArgs(state, prompt, model)
	if transcript != nil {
		if err := transcript.request(args, state, cancel); err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			return Invalid, ""
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
		return Invalid, ""
	}

	st := &Stats{Provider: "opencode", Repo: c.Repo, Issue: c.Issue, State: state.String(), Model: model, ToolCalls: map[string]int{}, BashCmds: map[string]int{}}
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
	// A refusal before any step reports usage spent nothing, so it must not look
	// like unknown usage and block the next model under a run limit.
	quotaDetail := ""
	if agentErr != nil {
		quotaDetail = agentErr.Error()
	} else if runErr != nil {
		quotaDetail = tail(diagnostics.String(), 500)
	}
	if quotaDetail != "" && !opencodeQuotaError(quotaDetail) {
		quotaDetail = ""
	}
	st.UsageUnknown = (!usageReported || usageSteps == 0) && quotaDetail == ""
	if st.UsageUnknown {
		c.CostUnknown++
	}
	defer func() {
		st.Outcome, st.OK = res.String(), res == Ok
		writeStats(st)
		logEvent(map[string]any{"event": "agent", "stats": st})
	}()
	if st.CostUSD >= limit {
		return OverBudget, ""
	}
	if ctx.Err() == context.DeadlineExceeded && len(result) == 0 {
		return Timeout, ""
	}
	if runErr != nil || agentErr != nil || len(result) == 0 {
		detail := ""
		if agentErr != nil {
			detail = agentErr.Error()
		} else if runErr != nil {
			detail = runErr.Error()
		}
		logEvent(map[string]any{"event": "agent-error", "err": detail, "stderr": tail(diagnostics.String(), 1500)})
		if quotaDetail != "" {
			return Invalid, quotaDetail
		}
		return Invalid, ""
	}
	if err := validateResult(result, step.Schema, out); err != nil {
		logEvent(map[string]any{"event": "invalid-result", "err": err.Error()})
		return Invalid, ""
	}
	st.Result = tail(string(result), 1500)
	return Ok, ""
}

// opencodeQuotaError reports whether OpenCode refused the session because a free
// allowance or a rate limit is spent. Another model may still work; authentication
// and schema failures are not retryable this way.
func opencodeQuotaError(detail string) bool {
	detail = strings.ToLower(detail)
	for _, marker := range []string{
		"free usage limit", "usage limit reached", "quota", "rate limit",
		"rate-limit", "too many requests", "429",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// opencodeFallbackFamilies lists the coding models agent-sm may move to after a
// quota error. Matching is by suffix so provider-qualified variants (for example
// nvidia/z-ai/glm-5.3-flash) also count.
var opencodeFallbackFamilies = []string{
	"/glm-5.3-flash", "/glm-5.3",
	"/deepseek-v4.1-flash-free", "/deepseek-v4.1-flash", "/deepseek-v4-pro",
	"/beam-free",
}

// opencodeFallbackModel returns the next catalog model to try. A free-usage failure
// skips every free model because they share one allowance; other quota and rate
// limit failures may move to any model in the list.
func opencodeFallbackModel(tried []string, reason string) (string, bool) {
	freeLimited := strings.Contains(strings.ToLower(reason), "free")
	seen := map[string]bool{}
	for _, model := range tried {
		seen[model] = true
	}
	models := opencodeModels()
	for _, family := range opencodeFallbackFamilies {
		for _, model := range models {
			if seen[model] || (freeLimited && strings.HasSuffix(model, "-free")) {
				continue
			}
			if strings.HasSuffix(model, family) {
				return model, true
			}
		}
	}
	return "", false
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

type modelCatalog struct {
	sync.Mutex
	at     time.Time
	models []string
}

var opencodeCatalog, copilotCatalog modelCatalog

// opencodeModels returns the installed OpenCode CLI's model IDs.
func opencodeModels() []string {
	return opencodeCatalog.list(func(out string) []string { return strings.Split(out, "\n") }, "opencode", "models")
}

// copilotModels returns the model IDs the installed Copilot CLI accepts for --model.
// Copilot has no models command; `copilot help config` lists them under `model`.
func copilotModels() []string {
	return copilotCatalog.list(func(out string) []string {
		_, rest, ok := strings.Cut(out, "`model`:")
		if !ok {
			return nil
		}
		var ids []string
		for _, line := range strings.Split(rest, "\n")[1:] {
			id, ok := strings.CutPrefix(strings.TrimSpace(line), "- \"")
			if !ok {
				break // the list ends at the first line that is not an entry
			}
			ids = append(ids, strings.TrimSuffix(id, "\""))
		}
		return ids
	}, "copilot", "help", "config")
}

// list runs the CLI and caches its parsed, valid model IDs, so the page's queue
// polling does not spawn a process every few seconds.
func (c *modelCatalog) list(parse func(string) []string, name string, args ...string) []string {
	c.Lock()
	defer c.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < 5*time.Minute {
		return append([]string(nil), c.models...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		c.at = time.Now()
		return append([]string(nil), c.models...)
	}
	var models []string
	seen := map[string]bool{}
	for _, line := range parse(string(out)) {
		line = strings.TrimSpace(line)
		if line != "" && modelRe.MatchString(line) && !seen[line] {
			seen[line] = true
			models = append(models, line)
		}
	}
	sort.Strings(models)
	c.models, c.at = models, time.Now()
	return append([]string(nil), models...)
}

// opencodeModelFor picks the default OpenCode model for a step. DeepSeek Flash is
// the safe working model. Plan and review prefer OpenCode's GLM Flash when the
// installed catalog offers it. Not Jev: it is a decision model and cannot run an agent step.
func opencodeModelFor(st Step) string {
	switch st.Name {
	case "plan", "review":
		for _, model := range opencodeModels() {
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
	}
	return model
}

// modelOptions is the suggestion list for the page's per-step model inputs.
// The inputs remain free text so a newly released model still works.
func modelOptions(provider string) []string {
	switch provider {
	case "claude":
		return []string{sonnet, opus, haiku, "claude-fable-5-2"}
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
	case "copilot":
		return append([]string{"auto"}, copilotModels()...)
	default:
		return []string{"auto"}
	}
}
