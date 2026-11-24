package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// piModels is every model agent-sm accepts, as pi's provider/model IDs. Claude and OpenCode Go
// are pi's own providers; the OpenCode CLI and Copilot CLI catalogs keep their IDs under their
// own prefixes. Only providers configured in pi's models.json can actually run a model.
var piModels = []string{
	"anthropic/claude-fable-5", "anthropic/claude-fable-5-1", "anthropic/claude-fable-5-2",
	"anthropic/claude-haiku-4-5", "anthropic/claude-haiku-4-5-20251001", "anthropic/claude-haiku-5-5",
	"anthropic/claude-opus-4-5", "anthropic/claude-opus-4-5-20251101", "anthropic/claude-opus-4-6",
	"anthropic/claude-opus-4-7", "anthropic/claude-opus-4-8", "anthropic/claude-opus-5", "anthropic/claude-opus-5-5",
	"anthropic/claude-sonnet-4-5", "anthropic/claude-sonnet-4-5-20250929", "anthropic/claude-sonnet-4-6",
	"anthropic/claude-sonnet-5", "anthropic/claude-sonnet-5-5",
	"opencode-go/beam-free", "opencode-go/claude-haiku-5-5", "opencode-go/deepseek-v4-pro",
	"opencode-go/deepseek-v4.1-flash", "opencode-go/gemini-4-argon", "opencode-go/glm-5.2", "opencode-go/glm-5.3",
	"opencode-go/glm-5.3-flash", "opencode-go/gpt-5.6-luna", "opencode-go/gpt-6-luna", "opencode-go/grok-4.7",
	"opencode-go/hy3", "opencode-go/hy4", "opencode-go/hy4-flash-free", "opencode-go/hy4-preview",
	"opencode-go/iceman-contributor-free", "opencode-go/kimi-k2.7-code", "opencode-go/kimi-k3",
	"opencode-go/kimi-k3.1-pro", "opencode-go/longcat-2.0", "opencode-go/longcat-2.5-free", "opencode-go/mimo-v2.5",
	"opencode-go/mimo-v2.5-pro", "opencode-go/mimo-v2.6-flash", "opencode-go/mimo-v2.6-pro", "opencode-go/minimax-m3",
	"opencode-go/minimax-m3.1-flash", "opencode-go/muse-spark-1.2-contributor", "opencode-go/muse-spark-1.3-contributor",
	"opencode-go/qwen3.7-plus", "opencode-go/qwen3.8-flash", "opencode-go/qwen3.8-max", "opencode-go/qwen4-max",
	"opencode-go/step-5-preview",
	"opencode/beam-free", "opencode/big-pickle", "opencode/deepseek-v4.1-flash-free", "opencode/flint-40b-free",
	"opencode/hy4-flash-free", "opencode/iceman-free", "opencode/mimo-v2.6-flash-free",
	"opencode/nemotron-3.5-lightning-free", "opencode/nemotron-3.5-super-vl-free", "opencode/tidepool-alpha-free",
	"nvidia/deepseek-ai/deepseek-v4.1-flash", "nvidia/google/diffusiongemma-26b-a4b-it", "nvidia/google/gemma-4-31b-it",
	"nvidia/google/google-paligemma", "nvidia/meta/llama-3.2-11b-vision-instruct", "nvidia/meta/llama-3.2-90b-vision-instruct",
	"nvidia/meta/llama-guard-4-12b", "nvidia/meta/muse-glimmer-30b", "nvidia/minimaxai/minimax-m3.1-flash",
	"nvidia/nvidia/llama-3.1-nemotron-safety-guard-8b-v3", "nvidia/nvidia/nemotron-3.5-content-safety",
	"nvidia/nvidia/nemotron-3.5-lightning-30b-a3b", "nvidia/nvidia/nemotron-3.5-super-vl-120b-a12b",
	"nvidia/nvidia/nemotron-3-nano-omni-30b-a3b-reasoning", "nvidia/nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia/nvidia/nemotron-voicechat", "nvidia/openai/gpt-oss-20b", "nvidia/poolside/laguna-xs-2.1",
	"nvidia/stepfun-ai/step-5-preview", "nvidia/z-ai/glm-5.3-flash",
	"github-copilot/claude-sonnet-5", "github-copilot/claude-fable-5.1", "github-copilot/claude-fable-5",
	"github-copilot/claude-opus-5", "github-copilot/claude-opus-4.8", "github-copilot/claude-opus-4.8-fast",
	"github-copilot/claude-opus-4.7", "github-copilot/claude-sonnet-4.6", "github-copilot/claude-haiku-4.5",
	"github-copilot/gpt-6-astra", "github-copilot/gpt-5.6-sol", "github-copilot/gpt-5.6-terra", "github-copilot/gpt-5.6-luna",
	"github-copilot/gpt-5.5", "github-copilot/gpt-5.4", "github-copilot/gpt-5.4-mini", "github-copilot/gpt-5.3-codex",
	"github-copilot/gpt-5-mini", "github-copilot/mai-code-1.1-flash", "github-copilot/gemini-3.8-flash",
	"github-copilot/gemini-3.7-flash", "github-copilot/gemini-3.6-flash", "github-copilot/gemini-3.5-flash",
	"github-copilot/grok-4.5", "github-copilot/kimi-k3", "github-copilot/kimi-k2.7-code",
}

// Default models: OpenCode Go's working model for the steps that write or read code, and its
// GLM Flash for the steps that decide (plan, review, the failed-run audit).
const (
	piWorkModel     = "opencode-go/deepseek-v4.1-flash"
	piDecisionModel = "opencode-go/glm-5.3-flash"
)

// piAgent runs one `pi -p` session inside the sandbox (sandbox.go) and validates its structured
// result. It returns Ok, or why there is no result: Invalid (missing or off-schema output), Timeout
// or OverBudget.
func piAgent(c *Ctx, state State, prompt string, out any, transcript *agentTranscript) (res Out) {
	step := steps[state]
	limit := settings.SessionLimit
	if c.RunBudget > 0 {
		limit = min(limit, c.RunBudget-c.Cost)
		if c.CostUnknown > 0 {
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
		dir, err = os.MkdirTemp(c.Scratch, "pi-")
		if err != nil {
			logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
			return Invalid
		}
		defer os.RemoveAll(dir)
	}
	// pi reads its system prompt and the user message from files: argv has a size limit on Windows.
	systemPath, messagePath := filepath.Join(dir, "system.md"), filepath.Join(dir, "prompt.md")
	message := prompt + "\n\nReturn exactly one JSON object, without markdown or other text. Required schema:\n" + step.Schema
	if err := os.WriteFile(systemPath, []byte(rolePrompt(state)), 0o600); err != nil {
		logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
		return Invalid
	}
	if err := os.WriteFile(messagePath, []byte(message), 0o600); err != nil {
		logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
		return Invalid
	}
	model := stepModel(step)
	args := piArgs(state, model, systemPath, messagePath)
	if transcript != nil {
		if err := transcript.request(args, state, cancel); err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			return Invalid
		}
	}
	cmd, err := sandboxed(ctx, c.Repo, dir, step.Edit, args)
	if err != nil {
		logEvent(map[string]any{"event": "agent-error", "err": err.Error()})
		return Invalid
	}
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

	st := &Stats{Provider: "pi", Repo: c.Repo, Issue: c.Issue, State: state.String(), Model: model, ToolCalls: map[string]int{}, BashCmds: map[string]int{}}
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
	usageReported := false
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			ToolName string `json:"toolName"`
			Args     struct {
				Command string `json:"command"`
				Path    string `json:"path"`
				Pattern string `json:"pattern"`
			} `json:"args"`
			Message struct {
				Role         string `json:"role"`
				StopReason   string `json:"stopReason"`
				ErrorMessage string `json:"errorMessage"`
				Content      []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Usage struct {
					Cost struct {
						Total float64 `json:"total"`
					} `json:"cost"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "session":
			st.SessionID = ev.ID
		case "tool_execution_start":
			st.ToolCalls[ev.ToolName]++
			in := toolInput{Command: ev.Args.Command, Path: ev.Args.Path, Pattern: ev.Args.Pattern}
			if in.Command != "" {
				for _, w := range cmdWords(in.Command) {
					st.BashCmds[w]++
				}
			}
			prog.tool(ev.ToolName, in)
		case "turn_end":
			if ev.Message.Role != "assistant" {
				continue
			}
			if finiteNonnegative(ev.Message.Usage.Cost.Total) {
				st.CostUSD += ev.Message.Usage.Cost.Total
				usageReported = true
			}
			if ev.Message.StopReason == "error" {
				agentErr = fmt.Errorf("pi reported an error: %s", ev.Message.ErrorMessage)
			}
			if ev.Message.StopReason == "stop" {
				var b strings.Builder
				for _, part := range ev.Message.Content {
					if part.Type == "text" {
						b.WriteString(part.Text)
					}
				}
				result = []byte(strings.TrimSpace(b.String()))
			}
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
	st.UsageUnknown = !usageReported
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

// piArgs builds the pi command line. The message is passed as an @file so long prompts never
// reach argv. Every step gets a shell: the sandbox, not a command list, limits what it reaches,
// and steps that do not edit see the worktree read-only.
func piArgs(state State, model, systemPath, messagePath string) []string {
	tools := "read,grep,find,ls,bash"
	if steps[state].Edit {
		tools += ",edit,write"
	}
	return []string{"-p", "--mode", "json", "--no-session", "--no-extensions", "--no-skills",
		"--no-prompt-templates", "--no-themes", "--no-mcp", "--tools", tools,
		"--model", model, "--append-system-prompt", systemPath, "@" + messagePath}
}

// modelOptions is the model list the page offers for each step.
func modelOptions() []string {
	return piModels
}
