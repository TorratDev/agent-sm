package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var commandPrefix = regexp.MustCompile(`^[a-zA-Z0-9_./~-]+(?: [a-zA-Z0-9_./~-]+)*$`)

func agent(c *Ctx, state State, prompt string, out any) (result Out) {
	var transcript *agentTranscript
	if settings.Transcripts {
		var err error
		// Transcripts are diagnostics, on by default: a capture failure is logged and never
		// changes the step's outcome, so a paid-for result is not thrown away.
		transcript, err = newTranscript(c, state)
		if err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			transcript = nil
		} else {
			defer func() {
				if err := transcript.finish(result); err != nil {
					logEvent(map[string]any{"event": "transcript-error", "path": transcript.dir, "err": err.Error()})
				}
			}()
		}
	}
	switch settings.Provider {
	case "claude":
		return claudeAgent(c, state, prompt, out, transcript)
	case "opencode":
		return opencodeAgent(c, state, prompt, out, transcript)
	default:
		return copilotAgent(c, state, prompt, out, transcript)
	}
}

func process(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	name, args = agentCommand(name, args)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid, syscall.SIGKILL) }
	// A descendant holding a pipe must not keep an otherwise exited session alive.
	cmd.WaitDelay = time.Second
	return cmd
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		remaining := max(0, b.limit-b.Len())
		b.Buffer.Write(p[:remaining])
		return n, fmt.Errorf("output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

func copilotArgs(state State, prompt, usagePath string, limit float64) []string {
	// Text mode renders Markdown and wraps lines, corrupting structured responses.
	prompt = rolePrompt(state) + "\n\n" + prompt + "\n\nReturn exactly one JSON object, without markdown or other text. Required schema:\n" + steps[state].Schema
	args := []string{"-p", prompt, "--stream=off", "--output-format=json",
		"--model", stepModel(steps[state]), "--max-ai-credits", strconv.FormatFloat(limit, 'f', -1, 64),
		"--usage-output-file", usagePath, "--no-ask-user", "--no-auto-update", "--disable-builtin-mcps"}
	// Restrict visibility as well as permissions: inherited MCPs/subagents cannot
	// become available merely because the operator has them installed.
	tools := "view,grep,glob,bash"
	if steps[state].Edit {
		tools += ",edit,create"
		args = append(args, "--allow-tool=write")
	} else {
		args = append(args, "--deny-tool=write")
	}
	args = append(args, "--available-tools="+tools, "--allow-tool=read")
	for _, entry := range strings.Split(configuredAllow(state), ",") {
		if prefix, ok := strings.CutPrefix(entry, "Bash("); ok {
			prefix = strings.TrimSuffix(prefix, ":*)")
			args = append(args, "--allow-tool=shell("+prefix+":*)")
		}
	}
	for _, prefix := range outward {
		args = append(args, "--deny-tool=shell("+prefix+":*)",
			"--deny-tool=shell(rtk "+prefix+":*)", "--deny-tool=shell(rtk proxy "+prefix+":*)")
	}
	if settings.Management.Mode == "service" {
		for _, prefix := range []string{"gh", "curl", "wget"} {
			for _, wrapper := range []string{"", "rtk ", "rtk proxy "} {
				args = append(args, "--deny-tool=shell("+wrapper+prefix+":*)")
			}
		}
	}
	for _, dir := range settings.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	return args
}

func copilotAgent(c *Ctx, state State, prompt string, out any, transcript *agentTranscript) (res Out) {
	limit := settings.SessionLimit
	if c.RunBudget > 0 {
		limit = min(limit, c.RunBudget-c.Cost)
		if c.CostUnknown > 0 {
			return OverBudget
		}
	}
	if limit < minCopilotCredits {
		logEvent(map[string]any{"event": "run-budget-reached",
			"err": fmt.Sprintf("Copilot requires at least %d AI credits for a step; effective limit is %.2f", minCopilotCredits, limit)})
		return OverBudget
	}
	ctx, cancel := context.WithTimeout(runCtx, time.Duration(settings.TimeoutMinutes*float64(time.Minute)))
	defer cancel()
	var dir string
	var err error
	if transcript != nil {
		dir = transcript.dir
	} else {
		dir, err = os.MkdirTemp(c.Scratch, "copilot-")
		if err != nil {
			logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
			return Invalid
		}
		defer os.RemoveAll(dir) // exact per-session directory, never a caller-provided root
	}
	usagePath := filepath.Join(dir, "usage.json")
	args := copilotArgs(state, prompt, usagePath, limit)
	if transcript != nil {
		args = append(args, "--session-id", transcript.sessionID, "--no-remote-export")
		if err := transcript.request(args, state, cancel); err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			return Invalid
		}
	}
	cmd := process(ctx, c.Repo, "copilot", args...)
	// Never inherit permission bypasses from a parent agent session.
	cmd.Env = cleanAgentEnv()
	stdout := &boundedOutput{limit: 8 << 20}
	stderr := &boundedOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if transcript != nil {
		cmd.Stdout = io.MultiWriter(transcript.stdout, stdout)
		cmd.Stderr = io.MultiWriter(transcript.stderr, stderr)
	}
	st := &Stats{Provider: "copilot", Repo: c.Repo, Issue: c.Issue, State: state.String(), UsageUnknown: true}
	if state == Investigate && len(c.Units) > 0 {
		st.Unit = c.Units[c.UnitIdx].ID
	}
	start := time.Now()
	prog := &progress{c: c, state: state, unit: st.Unit, start: start}
	prog.add("say", "Copilot session started (tool-level telemetry unavailable in final-response mode)")
	err = cmd.Run()
	st.DurationMs = int(time.Since(start).Milliseconds())
	st.Killed = ctx.Err() != nil
	if b, readErr := os.ReadFile(usagePath); readErr == nil {
		credits, model, usageErr := copilotUsage(b)
		if usageErr == nil {
			st.Credits, st.Model, st.UsageUnknown = &credits, model, false
			c.Cost += credits
		} else {
			logEvent(map[string]any{"event": "usage-error", "err": usageErr.Error()})
		}
	} else {
		logEvent(map[string]any{"event": "usage-error", "err": readErr.Error()})
	}
	if st.UsageUnknown {
		c.CostUnknown++
	}
	defer func() {
		st.Outcome, st.OK = res.String(), res == Ok
		writeStats(st)
		logEvent(map[string]any{"event": "agent", "stats": st})
		prog.add("say", "Copilot session ended: "+res.String())
		prog.flush(true)
	}()
	if ctx.Err() == context.DeadlineExceeded {
		return Timeout
	}
	if err != nil {
		// Native budget errors have changed between CLI releases; preserve diagnostics
		// and fail rather than treating an arbitrary error string as a completed result.
		logEvent(map[string]any{"event": "agent-error", "err": err.Error(), "stderr": tail(stderr.String(), 1500)})
		if st.Credits != nil && *st.Credits >= limit {
			return OverBudget
		}
		return Invalid
	}
	if st.Credits != nil && *st.Credits >= limit {
		return OverBudget
	}
	result, resultErr := copilotResult(stdout.Bytes())
	if resultErr != nil {
		logEvent(map[string]any{"event": "invalid-result", "err": resultErr.Error()})
		return Invalid
	}
	if err := validateResult(result, steps[state].Schema, out); err != nil {
		logEvent(map[string]any{"event": "invalid-result", "err": err.Error()})
		return Invalid
	}
	st.Result = tail(string(result), 1500)
	return Ok
}

func copilotResult(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	var result []byte
	completed := false
	for {
		var event struct {
			Type     string          `json:"type"`
			AgentID  string          `json:"agentId"`
			Data     json.RawMessage `json:"data"`
			ExitCode *int            `json:"exitCode"`
		}
		if err := d.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("Copilot JSON output: %w", err)
		}
		if event.Type == "" || completed {
			return nil, fmt.Errorf("Copilot JSON output: missing event type or event after final result")
		}
		if event.AgentID != "" {
			continue
		}
		switch event.Type {
		case "assistant.turn_start", "assistant.message_start", "assistant.message_delta":
			result = nil
		case "assistant.message":
			var message struct {
				Content          *string           `json:"content"`
				ToolRequests     []json.RawMessage `json:"toolRequests"`
				ParentToolCallID string            `json:"parentToolCallId"`
			}
			if err := json.Unmarshal(event.Data, &message); err != nil {
				return nil, fmt.Errorf("Copilot assistant message: %w", err)
			}
			if message.ParentToolCallID != "" {
				continue
			}
			// Never fall back to an earlier answer if the last main-agent message is incomplete.
			result = nil
			if message.Content != nil && len(message.ToolRequests) == 0 {
				result = []byte(*message.Content)
			}
		case "session.error":
			return nil, fmt.Errorf("Copilot reported a session error: %s", tail(string(event.Data), 1500))
		case "result":
			if event.ExitCode == nil || *event.ExitCode != 0 {
				return nil, fmt.Errorf("Copilot JSON output: missing or unsuccessful final exitCode")
			}
			completed = true
		}
	}
	if !completed || len(bytes.TrimSpace(result)) == 0 {
		return nil, fmt.Errorf("Copilot JSON output: missing final result or complete assistant response")
	}
	return result, nil
}

func copilotUsage(b []byte) (float64, string, error) {
	var u struct {
		TotalNanoAIU *float64                   `json:"totalNanoAiu"`
		ModelMetrics map[string]json.RawMessage `json:"modelMetrics"`
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return 0, "", err
	}
	if u.TotalNanoAIU == nil || !finiteNonnegative(*u.TotalNanoAIU) {
		return 0, "", fmt.Errorf("Copilot usage contains no complete AI-credit total")
	}
	model := ""
	if len(u.ModelMetrics) == 1 {
		for m := range u.ModelMetrics {
			model = m
		}
	}
	return *u.TotalNanoAIU / 1e9, model, nil
}

func cleanAgentEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		switch k {
		case "COPILOT_ALLOW_ALL", "COPILOT_ASSISTED_APPROVAL", "GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS",
			"SM_RESOLVED_CONFIG", "JIRA_API_TOKEN", "JIRA_USER_EMAIL", "BASH_ENV", "ENV",
			"OPENCODE_CONFIG", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CONFIG_PROJECT_DISABLE", "OPENCODE_DISABLE_AUTOUPDATE":
			continue
		}
		if settings.Management.Mode == "service" {
			switch k {
			case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
				continue
			}
		}
		env = append(env, entry)
	}
	env = append(env, "COPILOT_ALLOW_ALL=false", "COPILOT_ASSISTED_APPROVAL=false", "GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS=false")
	return env
}

func checkCommand(dir, command string, env ...string) (string, error) {
	ctx, cancel := context.WithTimeout(runCtx, time.Duration(settings.TimeoutMinutes*float64(time.Minute)))
	defer cancel()
	cmd := process(ctx, dir, shellPath(), "-c", command)
	cmd.Env = append(cleanAgentEnv(), env...)
	output := &boundedOutput{limit: 4 << 20}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	ev := map[string]any{"event": "check-command", "command": command, "ok": err == nil}
	if err != nil {
		ev["err"] = err.Error()
	}
	logEvent(ev)
	return output.String(), err
}

func verifyTestReports(dir string, patterns []string) error {
	for _, pattern := range patterns {
		files, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(files) == 0 {
			return fmt.Errorf("required test report %q missing", pattern)
		}
		for _, path := range files {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var report struct {
				XMLName  xml.Name `xml:"TestRun"`
				Counters struct {
					Total    int `xml:"total,attr"`
					Executed int `xml:"executed,attr"`
					Passed   int `xml:"passed,attr"`
				} `xml:"ResultSummary>Counters"`
			}
			if err := xml.Unmarshal(b, &report); err != nil {
				return fmt.Errorf("invalid TRX %s: %w", path, err)
			}
			n := report.Counters
			if n.Executed == 0 || n.Executed != n.Total || n.Passed != n.Executed {
				return fmt.Errorf("required test report %s: total=%d executed=%d passed=%d", path, n.Total, n.Executed, n.Passed)
			}
		}
	}
	return nil
}
