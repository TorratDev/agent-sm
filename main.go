// agent-sm: a state machine drives the work; agents only run inside single steps.
// The code decides what happens next from each step's returned Out, never the agent.
//
// usage: agent-sm <repo-dir> <issue-number> [--from <state>]
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed prompts/*.md prompts/generic/*.md
var promptFS embed.FS

// rolePrompt is the RTK rule followed by the step's role prompt (prompts/*.md).
func rolePrompt(s State) string {
	rtk, _ := promptFS.ReadFile("prompts/rtk.md")
	dir := "prompts/generic/"
	if settings.Legacy {
		dir = "prompts/"
	}
	role, _ := promptFS.ReadFile(dir + steps[s].Role + ".md")
	text := string(rtk) + "\n\n" + string(role)
	if settings.Management.Mode == "service" {
		text += "\n\nThe service supplies tracker context and owns all GitHub/Jira operations. Do not run tracker commands or skills. Only when decomposition is needed, you may include an issue_plan matching the supplied result schema; it is an unapproved proposal, never an instruction to apply changes."
	}
	return text
}

// Unit is one investigation session's share of the work.
type Unit struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Points []string `json:"points"`
}

type State int

const (
	Prepare State = iota
	Plan
	Investigate
	Execute
	Check
	Review
	Publish
	numStates
)

func (s State) String() string { return steps[s].Name }

type Out int

const (
	Ok Out = iota
	Blocked
	Pass
	Fail
	Direct     // planner: no investigation needed, go straight to execute
	More       // investigate: this unit is done, another unit remains
	Split      // too large for one PR: follow-up needed
	Escalate   // a human must decide
	Invalid    // agent output missing or off-schema
	Timeout    // agent session killed at SM_TIMEOUT
	OverBudget // agent session stopped by its budget (SM_BUDGET, or what is left of SM_RUN_BUDGET)
	numOuts
)

var outNames = [numOuts]string{"ok", "blocked", "pass", "fail", "direct", "more", "split", "escalate", "invalid", "timeout", "over-budget"}

func (o Out) String() string { return outNames[o] }

// Every way the pipeline can end. Declared in one place on purpose.
type Exit string

const (
	Ready     Exit = "ready"     // branch is local, checks pass, reviewer said PASS
	GaveUp    Exit = "gave-up"   // blocked, too many fix rounds, a timed-out session, or the budget is spent
	Errored   Exit = "errored"   // setup failed or agent output stayed invalid
	SplitExit Exit = "split"     // needs a follow-up issue
	Escalated Exit = "escalated" // a human must decide
	Stopped   Exit = "stopped"   // SIGINT or SIGTERM
)

// Step declares one state in one place: what runs, what it may return and, for an agent step,
// its role prompt, output schema, tool allow list and model.
type Step struct {
	Name string
	Run  func(*Ctx) Out
	Outs []Out // results outside this set panic; next() is tested against it
	// Agent steps only (Role != ""):
	Role   string // prompts/<Role>.md
	Schema string // JSON schema of the result; must match the struct the step decodes into
	Allow  string // default tools; SM_ALLOW_FILE replaces it, SM_EXTRA_ALLOW adds to it
	Model  string // default model; SM_MODEL_<STATE> (e.g. SM_MODEL_PLAN=haiku) or the page's choice overrides
	Edit   bool   // the step may edit files
}

// Read-only shell commands every agent step may run, so a batched call is not denied for an `ls`.
const shRead = "Bash(grep:*),Bash(head:*),Bash(tail:*),Bash(ls:*),Bash(cat:*),Bash(wc:*),Bash(sed:*),Bash(awk:*),Bash(cut:*),Bash(sort:*),Bash(echo:*),Bash(cd:*),Bash(test:*)"

// The py-desmume interpreter the prompts name for runtime probes, and what a probe run needs.
const shProbe = "Bash(~/venvs/desmume/bin/python:*),Bash(python3:*),Bash(export:*),Bash(mkdir:*),Bash(sha1sum:*)"

// Default models, as rust-mm's spawn.sh assigns them (prompts/original/orchestrator.md, Roles):
// sonnet for the working roles, opus for the reviewer. Never the CLI's own default, which
// follows the user's account and can be the most expensive model.
const (
	sonnet = "claude-sonnet-5-5"
	opus   = "claude-opus-5-5"
)

// stepModel is the model a step runs on: SM_MODEL_<STATE> if set, else the step's default.
func stepModel(st Step) string {
	return settings.model(st)
}

var steps [numStates]Step

func init() { // not a composite literal at package level: the step functions refer back to steps
	steps = [numStates]Step{
		Prepare: {Name: "prepare", Run: prepare, Outs: []Out{Ok, Fail}},
		Plan: {Name: "plan", Run: plan, Outs: []Out{Ok, Direct, Blocked, Split, Invalid, Timeout, OverBudget},
			Role: "planner", Model: sonnet,
			Schema: `{"type":"object","properties":{"status":{"enum":["plan","blocked"]},"objective":{"type":"string"},"points":{"type":"array","items":{"type":"string"}},"points_derived":{"type":"boolean"},"files":{"type":"array","items":{"type":"string"}},"verification":{"type":"string"},"needs_investigation":{"type":"boolean"},"fits_one_pr":{"type":"boolean"},"units":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"points":{"type":"array","items":{"type":"string"}}},"required":["id","title","points"],"additionalProperties":false}}},"required":["status","objective","points","points_derived","files","verification","needs_investigation","fits_one_pr","units"],"additionalProperties":false}`,
			Allow:  "Read,Grep,Glob," + shRead + ",Bash(gh issue view:*),Bash(gh issue list:*),Bash(git log:*)"},
		Investigate: {Name: "investigate", Run: investigate, Outs: []Out{Ok, More, Blocked, Split, Invalid, Timeout, OverBudget},
			Role: "investigator", Model: sonnet,
			Schema: `{"type":"object","properties":{"status":{"enum":["evidence","blocked"]},"evidence":{"type":"string"},"handoff":{"type":"string"},"fits_one_pr":{"type":"boolean"}},"required":["status","evidence","handoff","fits_one_pr"],"additionalProperties":false}`,
			Allow:  "Read,Grep,Glob," + shRead + ",Bash(git log:*),Bash(git grep:*),Bash(git status:*),Bash(gh issue view:*)"},
		Execute: {Name: "execute", Run: execute, Outs: []Out{Ok, Blocked, Fail, Invalid, Timeout, OverBudget},
			Role: "executor", Model: sonnet,
			Schema: `{"type":"object","properties":{"status":{"enum":["done","partial","blocked"]},"summary":{"type":"string"},"closes_issue":{"type":"boolean"}},"required":["status","summary","closes_issue"],"additionalProperties":false}`,
			Allow:  "Read,Grep,Glob,Edit,Write," + shRead + ",Bash(git diff:*),Bash(git status:*)",
			Edit:   true},
		Check: {Name: "check", Run: check, Outs: []Out{Pass, Fail}},
		Review: {Name: "review", Run: review, Outs: []Out{Pass, Fail, Split, Escalate, Invalid, Timeout, OverBudget},
			Role: "reviewer", Model: opus,
			Schema: `{"type":"object","properties":{"verdict":{"enum":["PASS","REVISE","SPLIT","ESCALATE"]},"reasons":{"type":"array","items":{"type":"string"}}},"required":["verdict","reasons"],"additionalProperties":false}`,
			Allow:  "Read,Grep,Glob," + shRead + ",Bash(git diff:*),Bash(git log:*)"},
		Publish: {Name: "publish", Run: publish, Outs: []Out{Ok, Fail}},
	}
	var schema map[string]any
	var proposal map[string]any
	json.Unmarshal([]byte(steps[Plan].Schema), &schema)
	json.Unmarshal([]byte(workPlanSchema), &proposal)
	schema["properties"].(map[string]any)["issue_plan"] = proposal
	b, _ := json.Marshal(schema)
	steps[Plan].Schema = string(b)
}

const (
	maxExecute = 3 // execute runs before giving up
	maxInvalid = 2 // runs of one state before invalid output becomes errored
	maxPlan    = 2 // plan runs: the first, and one re-plan after a blocked execute
)

type Next struct {
	State State
	Exit  Exit // non-empty means the run is over
}

func goTo(s State) Next { return Next{State: s} }
func done(e Exit) Next  { return Next{Exit: e} }

// next is the whole transition table. visits counts how many times each state has run.
func next(s State, o Out, visits [numStates]int) Next {
	fix := func() Next {
		if visits[Execute] >= maxExecute {
			return done(GaveUp)
		}
		return goTo(Execute)
	}
	if o == Invalid {
		if visits[s] < maxInvalid {
			return goTo(s)
		}
		return done(Errored)
	}
	if o == Timeout || o == OverBudget { // never rerun: the same session would spend the same again
		return done(GaveUp)
	}
	switch s {
	case Prepare:
		switch o {
		case Ok:
			return goTo(Plan)
		case Fail:
			return done(Errored)
		}
	case Plan:
		switch o {
		case Ok:
			return goTo(Investigate)
		case Direct:
			return goTo(Execute)
		case Blocked:
			return done(GaveUp)
		case Split:
			return done(SplitExit)
		}
	case Investigate:
		switch o {
		case Ok:
			return goTo(Execute)
		case More:
			return goTo(Investigate)
		case Blocked:
			return done(GaveUp)
		case Split:
			return done(SplitExit)
		}
	case Execute:
		switch o {
		case Ok:
			return goTo(Check)
		case Blocked: // plan is the step that sets the file list, the usual blocker
			if visits[Plan] < maxPlan {
				return goTo(Plan)
			}
			return done(GaveUp)
		case Fail:
			return done(Errored)
		}
	case Check:
		switch o {
		case Pass:
			return goTo(Review)
		case Fail:
			return fix()
		}
	case Review:
		switch o {
		case Pass:
			return goTo(Publish)
		case Fail:
			return fix()
		case Split:
			return done(SplitExit)
		case Escalate:
			return done(Escalated)
		}
	case Publish:
		switch o {
		case Ok:
			return done(Ready)
		case Fail:
			return done(Errored)
		}
	}
	panic(fmt.Sprintf("no transition for state=%v out=%v", s, o))
}

// Ctx is the typed data steps pass to each other.
type Ctx struct {
	Repo            string
	Issue           int
	Title           string
	Body            string
	Evidence        string // investigate -> execute
	Feedback        string // check/review failure -> next execute
	LastRevise      string // review REVISE -> next review, which confirms each item was done
	CheckOut        string // check -> review: what the passing checks printed (WARN lines)
	Diff            string // check -> review
	Summary         string // execute -> review
	Scratch         string // outside the repo
	Ghidra          string // ghidra project copy, or "none"
	Level           string // SM_PUBLISH: "", "pr" or "merge"
	Verdict         string // reviewer reasons
	ClosesIssue     bool   // executor: every Done-when item met
	Rounds          int    // execute runs so far
	Objective       string // plan -> everyone
	Points          []string
	PointsDerived   bool     // the planner derived the points; publish posts them on the issue
	Files           []string // plan -> check (scope gate), execute, review
	Verification    string
	Merged          bool   // publish merged the PR
	Units           []Unit // plan -> investigate: one session per unit, each sized for about 30 minutes
	UnitIdx         int
	Handoff         string // notes earlier units leave for later ones
	BlockedUnits    int
	Cost            float64 // known native usage: Claude USD or Copilot credits
	CostUnknown     int     // sessions without complete native usage
	RunBudget       float64 // native SM_RUN_LIMIT; 0 disables the run limit
	Export          string  // prepare -> dispatch: whether the Ghidra export is usable, with its STAMP
	Base            string
	Branch          string
	IssueURL        string
	JiraKey         string
	Proposal        *WorkPlan
	ManagementError string
	Comments        string
}

// runCtx is cancelled by SIGINT or SIGTERM (set in main); agent sessions end with it.
var runCtx = context.Background()

func logEvent(kv map[string]any) {
	kv["t"] = time.Now().UTC().Format(time.RFC3339) // the board shows how long a run took
	b, _ := json.Marshal(kv)
	fmt.Fprintln(os.Stderr, string(b))
	if eventsPath != "" { // the live view reads this file
		if f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}

func sh(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if name == "gh" && settings.Management.Mode == "service" {
		cmd.Env = serviceEnv()
	}
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func tail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// Stats is one agent session's measurements, appended as a JSON line to SM_STATS
// (default stats.jsonl under the run history) and echoed in the step log.
type Stats struct {
	T             string         `json:"t"`    // when the session ended, so history can be queried by time
	Repo          string         `json:"repo"` // which repository the run was in; one stats file holds them all
	Provider      string         `json:"provider,omitempty"`
	UsageUnknown  bool           `json:"usage_unknown,omitempty"`
	Credits       *float64       `json:"credits,omitempty"`
	Issue         int            `json:"issue"`
	State         string         `json:"state"`
	SessionID     string         `json:"session_id"`
	Model         string         `json:"model"`
	ToolsListed   []string       `json:"tools_listed"`
	MCP           []string       `json:"mcp_servers"`
	ToolCalls     map[string]int `json:"tool_calls"`
	Denials       int            `json:"permission_denials"`
	DeniedTools   map[string]int `json:"denied_tools"`  // tool name -> times denied
	DeniedSample  []string       `json:"denied_sample"` // first few attempts, truncated
	Turns         int            `json:"turns"`
	DurationMs    int            `json:"duration_ms"`
	CostUSD       float64        `json:"cost_usd,omitempty"`
	InputTokens   int            `json:"input_tokens"`
	OutputTokens  int            `json:"output_tokens"`
	CacheCreation int            `json:"cache_creation_tokens"`
	CacheRead     int            `json:"cache_read_tokens"`
	BashCmds      map[string]int `json:"bash_commands"` // command word(s) -> times run
	Killed        bool           `json:"killed"`        // timed out or stopped: the cost above is unknown (reported as 0)
	Outcome       string         `json:"outcome"`       // ok, invalid, timeout or over-budget
	Unit          string         `json:"unit"`
	OK            bool           `json:"ok"`
	Result        string         `json:"result"` // the structured output, first 1500 bytes
}

// statsFile is SM_STATS, or one stats file under the run history so every working directory shares it.
func statsFile() string {
	if p := os.Getenv("SM_STATS"); p != "" {
		return p
	}
	return filepath.Join(scratchRoot(), "stats.jsonl")
}

func writeStats(st *Stats) {
	st.T = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(st)
	path := statsFile()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Write(append(b, '\n'))
		f.Close()
	}
}

// builtin turns an allow list like "Read,Bash(git log:*)" into the tool names "Read,Bash".
func builtin(allow string) string {
	seen := map[string]bool{}
	var names []string
	for _, a := range strings.Split(allow, ",") {
		n, _, _ := strings.Cut(strings.TrimSpace(a), "(")
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return strings.Join(names, ",")
}

// claudeAgent runs one `claude -p` session and validates its structured result.
// allow is the tool allow list; only those built-in tools are loaded and no MCP servers,
// skills or hooks, to keep the cached prefix small. It returns Ok, or why there is no result:
// Invalid (missing or off-schema output), Timeout or OverBudget.
func claudeAgent(c *Ctx, state State, prompt string, out any, transcript *agentTranscript) (res Out) {
	step := steps[state]
	ctx, cancel := context.WithTimeout(runCtx, time.Duration(settings.TimeoutMinutes*float64(time.Minute)))
	defer cancel()
	budget := settings.SessionLimit
	if c.RunBudget > 0 { // a session may not spend more than the run has left
		budget = min(budget, c.RunBudget-c.Cost)
	}
	allow := configuredAllow(state)
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--json-schema", step.Schema,
		"--no-session-persistence", "--allowedTools", allow, "--max-budget-usd", strconv.FormatFloat(budget, 'f', 4, 64),
		"--append-system-prompt", rolePrompt(state)}
	if model := stepModel(step); model != "" {
		args = append(args, "--model", model)
	}
	// SM_AUDIT=1: every built-in tool is listed and permissions are bypassed, so `agent-sm stats`
	// can show what each state really uses. Outward commands stay denied (auditDeny), MCP servers
	// and skills stay off. Normal runs load only the allow list.
	audit := settings.Audit
	args = append(args, "--strict-mcp-config", "--disable-slash-commands")
	if settings.Management.Mode == "service" {
		var denied []string
		for _, prefix := range []string{"gh", "curl", "wget"} {
			for _, wrapper := range []string{"", "rtk ", "rtk proxy "} {
				denied = append(denied, "Bash("+wrapper+prefix+":*)")
			}
		}
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}
	if audit {
		deny := auditDeny
		if state != Execute {
			deny += ",Edit,Write,NotebookEdit"
		}
		args = append(args, "--permission-mode", "bypassPermissions", "--disallowedTools", deny)
	} else {
		args = append(args, "--tools", builtin(allow))
		if step.Edit {
			args = append(args, "--permission-mode", "acceptEdits")
		}
	}
	for _, d := range settings.AddDirs {
		if d != "" {
			args = append(args, "--add-dir", d)
		}
	}
	if transcript != nil {
		if err := transcript.request(args, state, cancel); err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			return Invalid
		}
	}
	cmd := process(ctx, c.Repo, "claude", args...)
	cmd.Env = cleanAgentEnv()
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

	st := &Stats{Provider: "claude", Repo: c.Repo, Issue: c.Issue, State: state.String(), ToolCalls: map[string]int{}, BashCmds: map[string]int{}}
	if state == Investigate && len(c.Units) > 0 {
		st.Unit = c.Units[c.UnitIdx].ID
	}
	prog := &progress{c: c, state: state, unit: st.Unit, start: time.Now()}
	var structured json.RawMessage
	usageReported := false
	isErr, subtype := true, ""
	var reader io.Reader = stdout
	if transcript != nil {
		reader = io.TeeReader(stdout, transcript.stdout)
	}
	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var ev struct {
			Type    string   `json:"type"`
			Subtype string   `json:"subtype"`
			Session string   `json:"session_id"`
			Model   string   `json:"model"`
			Tools   []string `json:"tools"`
			MCP     []struct {
				Name string `json:"name"`
			} `json:"mcp_servers"`
			Message struct {
				Content []struct {
					Type    string          `json:"type"`
					Name    string          `json:"name"`
					Text    string          `json:"text"`
					IsError bool            `json:"is_error"`
					Input   json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
			IsError bool     `json:"is_error"`
			Turns   int      `json:"num_turns"`
			Dur     int      `json:"duration_ms"`
			Cost    *float64 `json:"total_cost_usd"`
			Denied  []struct {
				Tool  string          `json:"tool_name"`
				Input json.RawMessage `json:"tool_input"`
			} `json:"permission_denials"`
			RawUsage struct {
				In     int `json:"input_tokens"`
				Out    int `json:"output_tokens"`
				Create int `json:"cache_creation_input_tokens"`
				Read   int `json:"cache_read_input_tokens"`
			} `json:"usage"`
			Structured json.RawMessage `json:"structured_output"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			st.SessionID, st.Model, st.ToolsListed = ev.Session, ev.Model, ev.Tools
			for _, m := range ev.MCP {
				st.MCP = append(st.MCP, m.Name)
			}
		case ev.Type == "assistant":
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "tool_use":
					st.ToolCalls[b.Name]++
					var in toolInput
					json.Unmarshal(b.Input, &in)
					if b.Name == "Bash" {
						for _, w := range cmdWords(in.Command) {
							st.BashCmds[w]++
						}
					}
					prog.tool(b.Name, in)
				case "text":
					prog.add("say", b.Text)
				}
			}
		case ev.Type == "user":
			for _, b := range ev.Message.Content {
				if b.Type == "tool_result" && b.IsError {
					prog.add("error", "the last tool call returned an error")
				}
			}
		case ev.Type == "result":
			// A finished background task resumes the session and emits another result; the
			// last one is the final answer. Its total_cost_usd is cumulative for the session,
			// while num_turns, duration_ms and usage cover only that segment.
			isErr, structured, subtype = ev.IsError, ev.Structured, ev.Subtype
			st.Turns, st.DurationMs, st.Denials = st.Turns+ev.Turns, st.DurationMs+ev.Dur, len(ev.Denied)
			usageReported = ev.Cost != nil && finiteNonnegative(*ev.Cost)
			if usageReported {
				st.CostUSD = *ev.Cost
			}
			st.DeniedTools = map[string]int{}
			for _, d := range ev.Denied {
				st.DeniedTools[d.Tool]++
				if len(st.DeniedSample) < 5 {
					st.DeniedSample = append(st.DeniedSample, d.Tool+" "+tail(string(d.Input), 200))
				}
			}
			st.InputTokens, st.OutputTokens = st.InputTokens+ev.RawUsage.In, st.OutputTokens+ev.RawUsage.Out
			st.CacheCreation, st.CacheRead = st.CacheCreation+ev.RawUsage.Create, st.CacheRead+ev.RawUsage.Read
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
	st.Killed = ctx.Err() != nil && len(structured) == 0
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
	if runErr != nil && len(structured) == 0 {
		logEvent(map[string]any{"event": "agent-error", "err": runErr.Error(), "stderr": tail(diagnostics.String(), 1500)})
	} else if runErr == nil && (isErr || len(structured) == 0) && subtype != "error_max_budget_usd" {
		// The CLI exited cleanly but gave no usable answer; say why instead of failing silently.
		logEvent(map[string]any{"event": "invalid-result", "err": fmt.Sprintf("claude result subtype %q, is_error %v, structured output %v",
			subtype, isErr, len(structured) > 0), "stderr": tail(diagnostics.String(), 1500)})
	}
	switch {
	case subtype == "error_max_budget_usd":
		return OverBudget
	case ctx.Err() == context.DeadlineExceeded && len(structured) == 0:
		return Timeout
	case runErr != nil || isErr || len(structured) == 0:
		return Invalid
	}
	st.Result = string(structured)
	if len(st.Result) > 1500 {
		st.Result = st.Result[:1500]
	}
	if err := validateResult(structured, step.Schema, out); err != nil {
		logEvent(map[string]any{"event": "invalid-result", "err": err.Error()})
		return Invalid
	}
	return Ok
}

func issueText(c *Ctx) string {
	return fmt.Sprintf("Issue #%d: %s\n\n%s", c.Issue, c.Title, c.Body)
}

func prepare(c *Ctx) Out {
	if out, err := sh(c.Repo, "git", "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		logEvent(map[string]any{"event": "prepare", "err": "working tree not clean"})
		return Fail
	}
	out, err := sh(c.Repo, "gh", "issue", "view", strconv.Itoa(c.Issue), "--json", "title,body,url")
	var iss struct{ Title, Body, URL string }
	if err != nil || json.Unmarshal([]byte(out), &iss) != nil {
		logEvent(map[string]any{"event": "prepare", "err": "gh issue view failed", "out": tail(out, 500)})
		return Fail
	}
	c.Title, c.Body = iss.Title, iss.Body
	c.IssueURL = iss.URL
	meta, err := issueMetadata(c.Body)
	if err != nil {
		logEvent(map[string]any{"event": "prepare", "err": err.Error()})
		return Fail
	}
	if c.Base != "" && meta.Base != "" && c.Base != meta.Base {
		logEvent(map[string]any{"event": "prepare", "err": "issue base changed after worktree creation"})
		return Fail
	}
	c.Base = settings.Base
	if meta.Base != "" {
		c.Base = meta.Base
	}
	c.JiraKey = meta.JiraKey
	if c.Branch != "" {
		if err := checkRunBranch(c); err != nil {
			logEvent(map[string]any{"event": "prepare", "err": err.Error()})
			return Fail
		}
	}
	if settings.Jira.Mode == "actions" && c.JiraKey != "" {
		result := reconcileJira(c)
		logEvent(map[string]any{"event": "jira", "result": result})
		if result.Status == "conflict" || result.Status == "blocked" {
			return Fail
		}
	}
	if settings.Management.Mode == "service" {
		if err := manageRun(c, false, 0, "", false); err != nil {
			return Fail
		}
		if err := managedDependencies(runCtx, c.Repo, c.Issue); err != nil {
			logEvent(map[string]any{"event": "prepare", "err": err.Error()})
			return Fail
		}
		if err := managementContext(c); err != nil {
			logEvent(map[string]any{"event": "prepare", "err": err.Error()})
			return Fail
		}
	} else if err := githubDependencies(c.Repo, c.Issue); err != nil {
		logEvent(map[string]any{"event": "prepare", "err": err.Error()})
		return Fail
	}
	for _, d := range dependencies(c.Body) {
		if st, err := sh(c.Repo, "gh", "issue", "view", strconv.Itoa(d), "--json", "state", "--jq", ".state"); err != nil || strings.TrimSpace(st) != "CLOSED" {
			logEvent(map[string]any{"event": "prepare", "err": "dependency still open", "issue": d})
			return Fail
		}
	}
	if c.Branch == "" {
		branch, _, err := nextIssueBranch(c.Repo, c.Issue, "")
		if err != nil {
			logEvent(map[string]any{"event": "prepare", "err": err.Error()})
			return Fail
		}
		if strings.HasPrefix(c.Base, "origin/") {
			if out, err := sh(c.Repo, "git", "fetch", "--", "origin", strings.TrimPrefix(c.Base, "origin/")); err != nil {
				logEvent(map[string]any{"event": "prepare", "err": "fetch base failed", "out": tail(out, 500)})
				return Fail
			}
		}
		if out, err := sh(c.Repo, "git", "checkout", "-b", branch, c.Base); err != nil {
			logEvent(map[string]any{"event": "prepare", "err": "branch", "out": tail(out, 500)})
			return Fail
		}
		c.Branch = branch
	}
	logEvent(map[string]any{"event": "branch", "branch": c.Branch, "base": c.Base})
	if settings.Legacy {
		c.Export = exportState()
	}
	return Ok
}

// exportState checks the Ghidra export's STAMP once, so no agent spends a turn on it.
// SM_EXPORT overrides the directory (default ~/rescue-re/export).
func exportState() string {
	dir := os.Getenv("SM_EXPORT")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "rescue-re", "export")
	}
	b, err := os.ReadFile(filepath.Join(dir, "STAMP"))
	if err != nil {
		return dir + " has no STAMP: it is unfinished or missing, do not use it"
	}
	return dir + " is ready, STAMP: " + oneLine(string(b), 200)
}

// dispatch is the user message of an agent step, in the form of the old orchestrator's
// dispatch text (now built by rust-mm's scripts/orch/spawn.sh) so the role prompts read it as before.
func dispatch(c *Ctx, role, extra string) string {
	notes := ""
	if d := settings.NotesDir; d != "" {
		for _, f := range []string{"all.md", role + ".md"} {
			if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
				notes += "\n\nOperator notes:\n" + string(b)
			}
		}
	}
	project := ""
	if settings.Legacy {
		project = fmt.Sprintf(" Ghidra copy: %s. Export: %s.", c.Ghidra, c.Export)
	}
	return fmt.Sprintf("Dispatch: you are the %s.%s Issue #%d. Scratch directory: %s.%s%s\n\n%s\n\nStart now.",
		role, project, c.Issue, c.Scratch, extra, notes, issueText(c)+c.Comments)
}

func plan(c *Ctx) Out {
	var r struct {
		Status             string
		Objective          string
		Points             []string
		PointsDerived      bool `json:"points_derived"`
		Files              []string
		Verification       string
		NeedsInvestigation bool `json:"needs_investigation"`
		FitsOnePR          bool `json:"fits_one_pr"`
		Units              []Unit
		IssuePlan          *WorkPlan `json:"issue_plan"`
	}
	extra := ""
	if c.Rounds > 0 {
		extra = " Re-plan: the executor stopped as blocked, and its work is in the working tree. Fix the task so it can finish " +
			"(for example add the files it needs); return blocked if it cannot. Its report:\n" + c.Summary
	}
	if o := agent(c, Plan, dispatch(c, steps[Plan].Role, extra), &r); o != Ok {
		return o
	}
	if c.Rounds > 0 {
		c.UnitIdx, c.BlockedUnits = 0, 0
		c.Feedback = "your last session stopped as blocked and the task was re-planned. Its report:\n" + c.Summary
	}
	c.Objective, c.Points, c.PointsDerived, c.Files, c.Verification, c.Units = r.Objective, r.Points, r.PointsDerived, r.Files, r.Verification, r.Units
	if r.IssuePlan != nil {
		if err := r.IssuePlan.validate(); err != nil {
			logEvent(map[string]any{"event": "invalid-issue-plan", "err": err.Error()})
			return Invalid
		}
		c.Proposal = r.IssuePlan
	}
	switch {
	case r.Status == "blocked":
		return Blocked
	case !r.FitsOnePR:
		return Split
	case !r.NeedsInvestigation:
		return Direct
	}
	return Ok
}

func investigate(c *Ctx) Out {
	var r struct {
		Status    string
		Evidence  string
		Handoff   string
		FitsOnePR bool `json:"fits_one_pr"`
	}
	u := Unit{ID: "all", Title: c.Objective, Points: c.Points}
	if len(c.Units) > 0 {
		u = c.Units[c.UnitIdx]
	}
	extra := fmt.Sprintf(" Unit %d of %d: %s (%s). Your points: %s. Objective: %s. This unit must fit about 30 minutes: stop when its points are answered; "+
		"if you cannot finish in that time, return what you have and say what is open.", c.UnitIdx+1, max(1, len(c.Units)), u.ID, u.Title, strings.Join(u.Points, "; "), c.Objective)
	if c.Handoff != "" {
		extra += "\n\nHandoff notes from earlier units:" + c.Handoff
	}
	if o := agent(c, Investigate, dispatch(c, steps[Investigate].Role, extra), &r); o != Ok {
		return o // the unit index does not move, so an invalid-output retry redoes the same unit
	}
	if r.Status == "blocked" {
		c.BlockedUnits++
		c.Evidence += fmt.Sprintf("\n\n## Unit %s BLOCKED\n%s", u.ID, r.Evidence)
	} else {
		c.Evidence += fmt.Sprintf("\n\n## Unit %s\n%s", u.ID, r.Evidence)
	}
	if r.Handoff != "" {
		c.Handoff += "\n- " + u.ID + ": " + r.Handoff
		os.WriteFile(filepath.Join(c.Scratch, "handoff.md"), []byte(c.Handoff), 0o644)
	}
	if !r.FitsOnePR {
		return Split
	}
	c.UnitIdx++
	switch {
	case c.UnitIdx < len(c.Units):
		return More
	case c.BlockedUnits >= max(1, len(c.Units)): // every unit blocked: nothing to go on
		return Blocked
	}
	return Ok
}

func taskText(c *Ctx) string {
	v := c.Verification
	if v == "" || v == "default" {
		v = "the program's checks (default or SM_CHECKS)"
	}
	return " Objective: " + c.Objective + ". Files you may touch: " + strings.Join(c.Files, ", ") +
		". Verification: " + v + ". Points: " + strings.Join(c.Points, "; ") + "."
}

func execute(c *Ctx) Out {
	if err := manageRun(c, true, 0, "Implementation started.", true); err != nil {
		return Fail // the service, not the task, is stuck: a re-plan would not help
	}
	var r struct {
		Status      string
		Summary     string
		ClosesIssue bool `json:"closes_issue"`
	}
	extra := taskText(c)
	if c.Evidence != "" {
		extra += " Findings:\n" + c.Evidence
	}
	if c.Feedback != "" {
		extra += "\n\nRevise: the work is already in the working tree. Make every change this lists:\n" + c.Feedback
	}
	if o := agent(c, Execute, dispatch(c, steps[Execute].Role, extra), &r); o != Ok {
		return o
	}
	c.Summary, c.ClosesIssue = r.Summary, r.ClosesIssue && r.Status == "done"
	if r.Status == "blocked" {
		return Blocked
	}
	return Ok // done or partial: both go to check and review; partial never closes the issue
}

func runChecks(c *Ctx) bool {
	if len(settings.Checks) == 0 {
		c.Feedback = "no checks configured"
		return false
	}
	resultDir, err := os.MkdirTemp(c.Scratch, "checks-")
	if err != nil {
		c.Feedback = "cannot create check output directory: " + err.Error()
		return false
	}
	c.CheckOut = ""
	for _, cmd := range settings.Checks {
		out, err := checkCommand(c.Repo, cmd, "SM_CHECK_RESULTS="+resultDir, "SM_SCRATCH="+c.Scratch)
		if err != nil {
			c.Feedback = "check failed: " + cmd + "\n" + tail(out, 4000)
			return false
		}
		c.CheckOut += "$ " + cmd + "\n" + ends(out, 500, 1000) + "\n"
	}
	if err := verifyTestReports(resultDir, settings.TestReports); err != nil {
		c.Feedback = err.Error()
		return false
	}
	return true
}

// ends keeps the start and the end of s: check-diff puts its WARN headers first, test runners their totals last.
func ends(s string, head, end int) string {
	if len(s) <= head+end {
		return s
	}
	return s[:head] + "\n[...]\n" + s[len(s)-end:]
}

// inScope: a staged path is allowed when it equals a listed file or lies under a listed directory.
func inScope(path string, files []string) bool {
	for _, f := range files {
		if path == f || strings.HasPrefix(path, strings.TrimSuffix(f, "/")+"/") {
			return true
		}
	}
	return false
}

// maxDiff is the largest staged diff put into the review dispatch, in bytes.
const maxDiff = 60000

// reviewDiff is the diff as the reviewer gets it: whole, or a note that says it was left out.
// A cut diff is never passed on.
func reviewDiff(diff, stat string) string {
	if len(diff) <= maxDiff {
		return diff
	}
	return fmt.Sprintf("NOT INCLUDED: the staged diff is %d bytes, over the %d-byte limit. Changed files:\n%s\n"+
		"Read every file's diff yourself with `git diff --cached -- <path>` before you judge.", len(diff), maxDiff, stat)
}

func check(c *Ctx) Out {
	// Formatting needs no judgment: fix it here instead of failing the format check and
	// spending an execute round. SM_FMT overrides; with SM_CHECKS set and no SM_FMT, nothing runs.
	fmtCmd := settings.Format
	if fmtCmd != "" {
		if out, err := checkCommand(c.Repo, fmtCmd, "SM_SCRATCH="+c.Scratch); err != nil {
			c.Feedback = "formatter failed: " + fmtCmd + "\n" + tail(out, 4000)
			return Fail
		}
	}
	if out, err := sh(c.Repo, "git", "add", "-A"); err != nil {
		c.Feedback = "git add failed: " + out
		return Fail
	}
	if len(c.Files) > 0 { // the scope gate: a diff outside the planned files never reaches review
		names, err := sh(c.Repo, "git", "diff", "--cached", "--name-only", "-z")
		if err != nil {
			c.Feedback = "cannot read staged paths: " + tail(names, 500)
			return Fail
		}
		var bad []string
		for _, n := range strings.Split(strings.TrimSuffix(names, "\x00"), "\x00") {
			if n == "" {
				continue
			}
			if !inScope(n, c.Files) {
				bad = append(bad, n)
			}
		}
		if len(bad) > 0 {
			c.Feedback = "files outside the task's list (" + strings.Join(c.Files, ", ") + "): " + strings.Join(bad, ", ") + ". Revert them."
			return Fail
		}
	}
	candidate, err := sh(c.Repo, "git", "write-tree")
	if err != nil {
		c.Feedback = "cannot record staged candidate: " + tail(candidate, 500)
		return Fail
	}
	if !runChecks(c) {
		return Fail
	}
	after, err := sh(c.Repo, "git", "write-tree")
	if err != nil || after != candidate {
		c.Feedback = "checks changed the staged candidate; review the changes before retrying"
		return Fail
	}
	if _, err := sh(c.Repo, "git", "diff", "--quiet"); err != nil {
		c.Feedback = "checks changed the worktree; review and stage the intended changes before retrying"
		return Fail
	}
	if out, err := sh(c.Repo, "git", "ls-files", "--others", "--exclude-standard", "-z"); err != nil || out != "" {
		c.Feedback = "checks left untracked files; review them or put check outputs outside the worktree"
		return Fail
	}
	diff, _ := sh(c.Repo, "git", "diff", "--cached")
	stat, _ := sh(c.Repo, "git", "diff", "--cached", "--stat")
	c.Diff = reviewDiff(diff, stat)
	if len(diff) > maxDiff {
		logEvent(map[string]any{"event": "diff-not-inlined", "bytes": len(diff)})
	}
	return Pass
}

func review(c *Ctx) Out {
	var r struct {
		Verdict string
		Reasons []string
	}
	extra := taskText(c) + " Executor's summary: " + c.Summary + "\nFindings:\n" + c.Evidence +
		"\nChecks: all passed. Their output (judge any WARN lines):\n" + c.CheckOut
	if c.LastRevise != "" {
		extra += "\nRe-review: the last review asked for the changes below. Confirm each one was made:\n" + c.LastRevise
	}
	extra += "\nDiff (staged):\n" + c.Diff
	if o := agent(c, Review, dispatch(c, steps[Review].Role, extra), &r); o != Ok {
		return o
	}
	c.Verdict = strings.Join(r.Reasons, "; ")
	switch r.Verdict {
	case "PASS":
		return Pass
	case "SPLIT":
		return Split
	case "ESCALATE":
		return Escalate
	}
	c.Feedback = "review (REVISE):\n- " + strings.Join(r.Reasons, "\n- ")
	c.LastRevise = "- " + strings.Join(r.Reasons, "\n- ")
	return Fail
}

func contains(l []Out, o Out) bool {
	for _, x := range l {
		if x == o {
			return true
		}
	}
	return false
}

// stateNames lists every station name in state order.
func stateNames() []string {
	names := make([]string, numStates)
	for s := State(0); s < numStates; s++ {
		names[s] = s.String()
	}
	return names
}

// saveCtx writes the run's data after every step, so a later run can pick it up with --from
// (and an operator can correct it by hand first).
func saveCtx(c *Ctx) {
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(filepath.Join(c.Scratch, "ctx.json"), b, 0o644)
}

// resume loads the Ctx an earlier run of this issue saved and returns the state to start from.
// Paths, publish level and Ghidra copy come from this invocation, not the file.
// shortcut: retry limits start again and the working tree is trusted as it is; add a tree check
// if resumed runs start from the wrong files.
func resume(c *Ctx, from string) (State, error) {
	b, err := os.ReadFile(filepath.Join(c.Scratch, "ctx.json"))
	if err != nil {
		return 0, fmt.Errorf("nothing to resume: %v", err)
	}
	saved := *c
	if err := json.Unmarshal(b, &saved); err != nil {
		return 0, fmt.Errorf("ctx.json: %v", err)
	}
	if saved.Issue != c.Issue {
		return 0, fmt.Errorf("ctx.json is for issue %d, not %d", saved.Issue, c.Issue)
	}
	saved.Repo, saved.Scratch, saved.Level, saved.Ghidra = c.Repo, c.Scratch, c.Level, c.Ghidra
	*c = saved
	for s := Plan; s < numStates; s++ { // prepare is a normal run
		if s.String() == from {
			return s, nil
		}
	}
	return 0, fmt.Errorf("--from must be one of: %s", strings.Join(stateNames()[Plan:], ", "))
}

func run(c *Ctx, s State) Exit {
	var visits [numStates]int
	for {
		if runCtx.Err() != nil {
			return Stopped
		}
		if c.RunBudget > 0 && (c.Cost >= c.RunBudget || c.CostUnknown > 0) {
			logEvent(map[string]any{"event": "run-budget-reached", "before": s.String(),
				"usage": c.Cost, "limit": c.RunBudget, "usage_unit": settings.unit(), "usage_unknown": c.CostUnknown > 0})
			return GaveUp
		}
		visits[s]++
		c.Rounds = visits[Execute]
		o := steps[s].Run(c)
		saveCtx(c)
		if !contains(steps[s].Outs, o) {
			panic(fmt.Sprintf("step %v returned undeclared out %v", s, o))
		}
		logEvent(map[string]any{"state": s.String(), "out": o.String(), "visit": visits[s],
			"usage": c.Cost, "usage_unit": settings.unit(), "provider": settings.Provider, "usage_unknown": c.CostUnknown > 0})
		n := next(s, o, visits)
		if o == More {
			visits[s] = 0 // invalid-output retries count per unit
		}
		if n.Exit != "" {
			return n.Exit
		}
		s = n.State
	}
}

func main() {
	args := os.Args[1:]
	configPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 == len(args) {
				fmt.Fprintln(os.Stderr, "--config requires a path")
				os.Exit(2)
			}
			configPath = args[i+1]
			args = append(args[:i], args[i+2:]...)
			i--
		}
	}
	if len(args) == 0 {
		args = []string{"serve"}
	}
	os.Args = append(os.Args[:1], args...)
	managementCommand = len(args) > 0 && (args[0] == "issues" || args[0] == "reconcile")
	if len(os.Args) >= 2 && os.Args[1] == "stats" {
		path := statsFile()
		if len(os.Args) > 2 {
			path = os.Args[2]
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Print(report(b))
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "allow" {
		if os.Getenv("SM_PROVIDER") == "copilot" {
			fmt.Fprintln(os.Stderr, "allow suggestions use Claude syntax; use profile commands for Copilot")
			os.Exit(2)
		}
	} else if configPath == "" && len(args) > 0 && args[0] == "serve" {
		p, err := autoProfile(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		settings, autoSettings = p, true
	} else {
		p, err := loadProfile(configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		settings = p
	}
	var stop context.CancelFunc
	runCtx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) > 0 && args[0] == "issues" {
		if err := issuesCLI(runCtx, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if (len(os.Args) == 4 || (len(os.Args) == 5 && os.Args[4] == "--apply")) && os.Args[1] == "reconcile" {
		n, err := strconv.Atoi(os.Args[3])
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "issue number must be positive")
			os.Exit(2)
		}
		if settings.Management.Mode == "service" {
			m, err := newManager(runCtx, os.Args[2], settings)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if len(os.Args) == 4 {
				result := reconcileJira(&Ctx{Repo: os.Args[2], Issue: n})
				b, _ := json.Marshal(result)
				fmt.Println(string(b))
				if result.Status != "already aligned" {
					os.Exit(1)
				}
				return
			}
			if err == nil && len(os.Args) == 5 {
				err = m.register(runCtx, n, false, 0, "")
				if err == nil {
					err = m.reconcile(runCtx, n, true)
				}
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			b, err := m.snapshot(runCtx)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			fmt.Println(string(b))
			return
		}
		if len(os.Args) == 5 {
			fmt.Fprintln(os.Stderr, "--apply requires exclusive service-management mode")
			os.Exit(1)
		}
		result := reconcileJira(&Ctx{Repo: os.Args[2], Issue: n})
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		if result.Status != "already aligned" {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "serve" { // agent-sm serve [--repo DIR] [issue] [addr]
		issue, addr, repo := "", "127.0.0.1:8787", ""
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			if args[i] == "--repo" && i+1 < len(args) {
				repo = args[i+1]
				i++
			} else if _, err := strconv.Atoi(args[i]); err == nil {
				issue = args[i]
			} else {
				addr = args[i]
			}
		}
		if repo == "" && configPath == "" {
			var err error
			repo, err = canonicalRepo(".")
			if err != nil {
				fmt.Fprintf(os.Stderr, "agent-sm: could not auto-select the current repository: %v\n", err)
			}
		}
		fmt.Fprintln(os.Stderr, serve(issue, addr, repo))
		os.Exit(1)
	}
	if len(os.Args) >= 2 && os.Args[1] == "allow" {
		path := statsFile()
		if len(os.Args) > 2 {
			path = os.Args[2]
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(allowJSON(b))
		return
	}
	from := "" // agent-sm <repo-dir> <issue-number> --from <state>: resume from the saved ctx.json
	if len(os.Args) == 5 && os.Args[3] == "--from" {
		from, os.Args = os.Args[4], os.Args[:3]
	}
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: agent-sm <repo-dir> <issue-number> [--from <state>] | agent-sm stats|allow [file] | agent-sm serve [--repo DIR] [issue] [addr]  (SM_PUBLISH=pr|merge to push, open PRs, merge)")
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Args[2])
	if err != nil || n <= 0 {
		fmt.Fprintln(os.Stderr, "issue number must be a positive integer")
		os.Exit(2)
	}
	// A panic means a hole in our table: log it, leave the branch as it is, exit non-zero.
	defer func() {
		if r := recover(); r != nil {
			logEvent(map[string]any{"event": "panic", "err": fmt.Sprint(r)})
			os.Exit(2)
		}
	}()
	level := os.Getenv("SM_PUBLISH")
	if level != levelNone && level != levelPR && level != levelMerge {
		fmt.Fprintln(os.Stderr, "SM_PUBLISH must be empty, pr or merge")
		os.Exit(2)
	}
	c := &Ctx{Repo: os.Args[1], Issue: n, Level: level, Ghidra: "none", Scratch: filepath.Join(scratchRoot(), strconv.Itoa(n)), RunBudget: settings.RunLimit, Base: os.Getenv("SM_WORKTREE_BASE"), Branch: os.Getenv("SM_WORKTREE_BRANCH")}
	if v := os.Getenv("SM_GHIDRA_COPY"); settings.Legacy && v != "" {
		c.Ghidra = v
	}
	if v := os.Getenv("SM_SCRATCH"); v != "" {
		c.Scratch = v
	}
	os.MkdirAll(c.Scratch, 0o755)
	eventsPath = os.Getenv("SM_EVENTS")
	if eventsPath == "" {
		eventsPath = filepath.Join(c.Scratch, "events.jsonl")
		if from == "" {
			os.Remove(eventsPath) // a new run starts a new event log; a resumed one adds to it
		}
	}
	start := Prepare
	if from != "" {
		if start, err = resume(c, from); err == nil {
			// prepare is skipped, so its branch must already be checked out
			branch := "sm/issue-" + strconv.Itoa(n)
			if cur, _ := sh(c.Repo, "git", "rev-parse", "--abbrev-ref", "HEAD"); strings.TrimSpace(cur) != branch {
				err = fmt.Errorf("%s is not on branch %s", c.Repo, branch)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		logEvent(map[string]any{"event": "resume", "from": from})
	}
	exit := run(c, start)
	followUp(c, exit)
	if settings.Management.Mode == "service" && exit != Ready {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		m, err := newManager(ctx, c.Repo, settings)
		if err == nil {
			err = m.deactivate(ctx, c.Issue, string(exit))
		}
		if err != nil {
			c.ManagementError = err.Error()
			logEvent(map[string]any{"event": "management-deactivation-failed", "err": err.Error()})
		}
		cancel()
	}
	if settings.Management.Mode == "service" && exit != Stopped {
		manageRun(c, false, 0, fmt.Sprintf("Coding outcome: %s\n%s\n%s\n\nPoints:\n%s\n\nEvidence:\n%s", exit, c.Summary, c.Verdict, strings.Join(c.Points, "\n"), c.Evidence), true)
	}
	stopGhidra(c)
	cleanupWorktree(c)
	management := ""
	if settings.Management.Mode == "service" {
		management = "aligned"
		if c.ManagementError != "" {
			management = "pending"
		}
	}
	logEvent(map[string]any{"exit": string(exit), "management": management, "management_error": c.ManagementError, "provider": settings.Provider, "usage": c.Cost, "usage_unit": settings.unit(), "sessions_with_unknown_cost": c.CostUnknown})
	fmt.Println(exit)
	if c.ManagementError != "" {
		fmt.Fprintln(os.Stderr, "Synchronization incomplete: "+c.ManagementError)
	}
	if exit != Ready || c.ManagementError != "" {
		os.Exit(1)
	}
}
