// agent-sm: a state machine drives the work; agents only run inside single steps.
// The code decides what happens next from each step's returned Out, never the agent.
//
// usage: agent-sm <repo-dir> <issue-number>
package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed prompts/*.md
var promptFS embed.FS

// rolePrompt is the RTK rule followed by the step's role prompt (prompts/*.md).
func rolePrompt(s State) string {
	rtk, _ := promptFS.ReadFile("prompts/rtk.md")
	role, _ := promptFS.ReadFile("prompts/" + steps[s].Role + ".md")
	return string(rtk) + "\n\n" + string(role)
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

// Default models, as the rust-mm orchestrator assigned them (prompts/original/orchestrator.md):
// sonnet for the working roles, opus for the reviewer. Never the CLI's own default, which
// follows the user's account and can be the most expensive model.
const (
	sonnet = "claude-sonnet-5-5"
	opus   = "claude-opus-5-5"
)

// stepModel is the model a step runs on: SM_MODEL_<STATE> if set, else the step's default.
func stepModel(st Step) string {
	if v := os.Getenv("SM_MODEL_" + strings.ToUpper(st.Name)); v != "" {
		return v
	}
	return st.Model
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
			Allow:  "Read,Grep,Glob," + shRead + "," + shProbe + ",Bash(ghidra:*),Bash(git log:*),Bash(git grep:*),Bash(git status:*),Bash(gh issue view:*),Bash(cp:*),Bash(sleep:*)"},
		Execute: {Name: "execute", Run: execute, Outs: []Out{Ok, Blocked, Invalid, Timeout, OverBudget},
			Role: "executor", Model: sonnet,
			Schema: `{"type":"object","properties":{"status":{"enum":["done","partial","blocked"]},"summary":{"type":"string"},"closes_issue":{"type":"boolean"}},"required":["status","summary","closes_issue"],"additionalProperties":false}`,
			Allow:  "Read,Grep,Glob,Edit,Write," + shRead + "," + shProbe + ",Bash(cargo:*),Bash(scripts/check-deps.sh:*),Bash(./scripts/check-deps.sh:*),Bash(git diff:*),Bash(git status:*)",
			Edit:   true},
		Check: {Name: "check", Run: check, Outs: []Out{Pass, Fail}},
		Review: {Name: "review", Run: review, Outs: []Out{Pass, Fail, Split, Escalate, Invalid, Timeout, OverBudget},
			Role: "reviewer", Model: opus,
			Schema: `{"type":"object","properties":{"verdict":{"enum":["PASS","REVISE","SPLIT","ESCALATE"]},"reasons":{"type":"array","items":{"type":"string"}}},"required":["verdict","reasons"],"additionalProperties":false}`,
			Allow:  "Read,Grep,Glob," + shRead + "," + shProbe + ",Bash(git diff:*),Bash(git log:*),Bash(ghidra:*)"},
		Publish: {Name: "publish", Run: publish, Outs: []Out{Ok, Fail}},
	}
}

const (
	maxExecute = 3 // execute runs before giving up
	maxInvalid = 2 // runs of one state before invalid output becomes errored
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
		case Blocked:
			return done(GaveUp)
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
	Repo          string
	Issue         int
	Title         string
	Body          string
	Evidence      string // investigate -> execute
	Feedback      string // check/review failure -> next execute
	Diff          string // check -> review
	Summary       string // execute -> review
	Scratch       string // outside the repo
	Ghidra        string // ghidra project copy, or "none"
	Level         string // SM_PUBLISH: "", "pr" or "merge"
	Model         string // executor model id, for the commit trailer
	Verdict       string // reviewer reasons
	ClosesIssue   bool   // executor: every Done-when item met
	Rounds        int    // execute runs so far
	Objective     string // plan -> everyone
	Points        []string
	PointsDerived bool     // the planner derived the points; publish posts them on the issue
	Files         []string // plan -> check (scope gate), execute, review
	Verification  string
	Merged        bool   // publish merged the PR
	Units         []Unit // plan -> investigate: one session per unit, each sized for about 30 minutes
	UnitIdx       int
	Handoff       string // notes earlier units leave for later ones
	BlockedUnits  int
	Cost          float64 // known cost so far; killed sessions report none, see CostUnknown
	CostUnknown   int     // sessions killed before they reported a cost
	RunBudget     float64 // SM_RUN_BUDGET: the run starts no step once Cost reaches it; 0 is no limit
	Export        string  // prepare -> dispatch: whether the Ghidra export is usable, with its STAMP
}

// runCtx is cancelled by SIGINT or SIGTERM (set in main); agent sessions end with it.
var runCtx = context.Background()

func logEvent(kv map[string]any) {
	kv["t"] = time.Now().Format(time.RFC3339) // the page orders events against status.json with it
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
// (default agent-sm-stats.jsonl) and echoed in the step log.
type Stats struct {
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
	CostUSD       float64        `json:"cost_usd"`
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

func writeStats(st *Stats) {
	path := os.Getenv("SM_STATS")
	if path == "" {
		path = "agent-sm-stats.jsonl"
	}
	b, _ := json.Marshal(st)
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

// agent runs one `claude -p` session and decodes its schema-checked result into out.
// allow is the tool allow list; only those built-in tools are loaded and no MCP servers,
// skills or hooks, to keep the cached prefix small. It returns Ok, or why there is no result:
// Invalid (missing or off-schema output), Timeout or OverBudget.
func agent(c *Ctx, state State, prompt string, out any) (res Out) {
	step := steps[state]
	minutes := 60 // SM_TIMEOUT overrides, in minutes
	if v, err := strconv.Atoi(os.Getenv("SM_TIMEOUT")); err == nil && v > 0 {
		minutes = v
	}
	ctx, cancel := context.WithTimeout(runCtx, time.Duration(minutes)*time.Minute)
	defer cancel()
	budget := 2.0
	if v, err := strconv.ParseFloat(os.Getenv("SM_BUDGET"), 64); err == nil && v > 0 {
		budget = v
	}
	if c.RunBudget > 0 { // a session may not spend more than the run has left
		budget = min(budget, c.RunBudget-c.Cost)
	}
	// shortcut: per-run env overrides instead of per-repo config. SM_EXTRA_ALLOW adds allow
	// entries to every agent; SM_ADD_DIRS (comma separated) lets them read outside the repo.
	allow := allowFor(state, step.Allow)
	if x := os.Getenv("SM_EXTRA_ALLOW"); x != "" {
		allow += "," + x
	}
	allow = withRTK(allow)
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--json-schema", step.Schema,
		"--no-session-persistence", "--allowedTools", allow, "--max-budget-usd", strconv.FormatFloat(budget, 'f', 4, 64),
		"--append-system-prompt", rolePrompt(state)}
	if model := stepModel(step); model != "" {
		args = append(args, "--model", model)
	}
	// SM_AUDIT=1: every built-in tool is listed and permissions are bypassed, so `agent-sm stats`
	// can show what each state really uses. Outward commands stay denied (auditDeny), MCP servers
	// and skills stay off. Normal runs load only the allow list.
	audit := os.Getenv("SM_AUDIT") != ""
	args = append(args, "--strict-mcp-config", "--disable-slash-commands")
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
	for _, d := range strings.Split(os.Getenv("SM_ADD_DIRS"), ",") {
		if d != "" {
			args = append(args, "--add-dir", d)
		}
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = c.Repo
	// Its own process group, killed whole on timeout or stop, so probes and the Ghidra bridge go too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdout, perr := cmd.StdoutPipe()
	if perr == nil {
		perr = cmd.Start()
	}
	if perr != nil {
		logEvent(map[string]any{"event": "agent-error", "err": perr.Error()})
		return Invalid
	}

	st := &Stats{Issue: c.Issue, State: state.String(), ToolCalls: map[string]int{}, BashCmds: map[string]int{}}
	if state == Investigate && len(c.Units) > 0 {
		st.Unit = c.Units[c.UnitIdx].ID
	}
	prog := &progress{c: c, state: state, unit: st.Unit, start: time.Now()}
	var structured json.RawMessage
	isErr, subtype := true, ""
	sc := bufio.NewScanner(stdout)
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
			IsError bool    `json:"is_error"`
			Turns   int     `json:"num_turns"`
			Dur     int     `json:"duration_ms"`
			Cost    float64 `json:"total_cost_usd"`
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
			isErr, structured, subtype = ev.IsError, ev.Structured, ev.Subtype
			st.Turns, st.DurationMs, st.CostUSD, st.Denials = ev.Turns, ev.Dur, ev.Cost, len(ev.Denied)
			st.DeniedTools = map[string]int{}
			for _, d := range ev.Denied {
				st.DeniedTools[d.Tool]++
				if len(st.DeniedSample) < 5 {
					st.DeniedSample = append(st.DeniedSample, d.Tool+" "+tail(string(d.Input), 200))
				}
			}
			st.InputTokens, st.OutputTokens = ev.RawUsage.In, ev.RawUsage.Out
			st.CacheCreation, st.CacheRead = ev.RawUsage.Create, ev.RawUsage.Read
		}
	}
	runErr := cmd.Wait()
	st.Killed = ctx.Err() != nil && len(structured) == 0
	prog.flush(true)
	c.Cost += st.CostUSD
	if st.Killed {
		c.CostUnknown++
	}
	if state == Execute && st.Model != "" {
		c.Model = st.Model
	}
	defer func() {
		st.Outcome, st.OK = res.String(), res == Ok
		writeStats(st)
		logEvent(map[string]any{"event": "agent", "stats": st})
	}()
	if runErr != nil && len(structured) == 0 {
		logEvent(map[string]any{"event": "agent-error", "err": runErr.Error()})
	}
	switch {
	case subtype == "error_max_budget_usd":
		return OverBudget
	case ctx.Err() == context.DeadlineExceeded && len(structured) == 0:
		return Timeout
	case isErr || len(structured) == 0:
		return Invalid
	}
	dec := json.NewDecoder(bytes.NewReader(structured))
	dec.DisallowUnknownFields()
	st.Result = string(structured)
	if len(st.Result) > 1500 {
		st.Result = st.Result[:1500]
	}
	if dec.Decode(out) != nil {
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
	out, err := sh(c.Repo, "gh", "issue", "view", strconv.Itoa(c.Issue), "--json", "title,body")
	var iss struct{ Title, Body string }
	if err != nil || json.Unmarshal([]byte(out), &iss) != nil {
		logEvent(map[string]any{"event": "prepare", "err": "gh issue view failed", "out": tail(out, 500)})
		return Fail
	}
	c.Title, c.Body = iss.Title, iss.Body
	for _, d := range dependencies(c.Body) {
		if st, _ := sh(c.Repo, "gh", "issue", "view", strconv.Itoa(d), "--json", "state", "--jq", ".state"); strings.TrimSpace(st) == "OPEN" {
			logEvent(map[string]any{"event": "prepare", "err": "dependency still open", "issue": d})
			return Fail
		}
	}
	branch := "sm/issue-" + strconv.Itoa(c.Issue)
	if n, _ := sh(c.Repo, "gh", "pr", "list", "--head", branch, "--state", "all", "--json", "number", "--jq", "length"); strings.TrimSpace(n) != "0" && strings.TrimSpace(n) != "" {
		logEvent(map[string]any{"event": "prepare", "err": "a PR from " + branch + " already exists; this is abort and restart, resolve it first"})
		return Fail
	}
	_, localErr := sh(c.Repo, "git", "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	_, remoteErr := sh(c.Repo, "git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	if localErr == nil || remoteErr == nil {
		logEvent(map[string]any{"event": "prepare", "err": "branch " + branch + " already exists (local or origin); delete it if it holds nothing you want"})
		return Fail
	}
	if out, err := sh(c.Repo, "git", "checkout", "-b", branch); err != nil {
		logEvent(map[string]any{"event": "prepare", "err": "branch", "out": tail(out, 500)})
		return Fail
	}
	c.Export = exportState()
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
// dispatch text (prompts/original/orchestrator.md) so the role prompts read it as before.
func dispatch(c *Ctx, role, extra string) string {
	notes := ""
	if d := os.Getenv("SM_NOTES_DIR"); d != "" { // operator notes: all.md for every step, <role>.md for one
		for _, f := range []string{"all.md", role + ".md"} {
			if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
				notes += "\n\nOperator notes:\n" + string(b)
			}
		}
	}
	return fmt.Sprintf("Dispatch: you are the %s. Ghidra copy: %s. Export: %s. Sub-issue #%d. Scratch directory: %s.%s%s\n\n%s\n\nStart now.",
		role, c.Ghidra, c.Export, c.Issue, c.Scratch, extra, notes, issueText(c))
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
	}
	if o := agent(c, Plan, dispatch(c, steps[Plan].Role, ""), &r); o != Ok {
		return o
	}
	c.Objective, c.Points, c.PointsDerived, c.Files, c.Verification, c.Units = r.Objective, r.Points, r.PointsDerived, r.Files, r.Verification, r.Units
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
	// shortcut: fixed cargo commands, override with SM_CHECKS (newline separated) for other repos.
	cmds := []string{"cargo fmt --all -- --check", "cargo clippy --workspace --all-targets -- -D warnings", "cargo test --workspace"}
	if v := os.Getenv("SM_CHECKS"); v != "" {
		cmds = strings.Split(v, "\n")
	}
	for _, cmd := range cmds {
		if out, err := sh(c.Repo, "sh", "-c", cmd); err != nil {
			c.Feedback = "check failed: " + cmd + "\n" + tail(out, 4000)
			return false
		}
	}
	return true
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
	fmtCmd := "cargo fmt --all"
	if os.Getenv("SM_CHECKS") != "" {
		fmtCmd = ""
	}
	if v, ok := os.LookupEnv("SM_FMT"); ok {
		fmtCmd = v
	}
	if fmtCmd != "" {
		sh(c.Repo, "sh", "-c", fmtCmd) // a failure here shows up in the checks below
	}
	if out, err := sh(c.Repo, "git", "add", "-A"); err != nil {
		c.Feedback = "git add failed: " + out
		return Fail
	}
	if len(c.Files) > 0 { // the scope gate: a diff outside the planned files never reaches review
		names, _ := sh(c.Repo, "git", "diff", "--cached", "--name-only")
		var bad []string
		for _, n := range strings.Fields(names) {
			if !inScope(n, c.Files) {
				bad = append(bad, n)
			}
		}
		if len(bad) > 0 {
			c.Feedback = "files outside the task's list (" + strings.Join(c.Files, ", ") + "): " + strings.Join(bad, ", ") + ". Revert them."
			return Fail
		}
	}
	if !runChecks(c) {
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
		"\nChecks: all passed.\nDiff (staged):\n" + c.Diff
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

func run(c *Ctx) Exit {
	var visits [numStates]int
	s := Prepare
	for {
		if runCtx.Err() != nil {
			return Stopped
		}
		if c.RunBudget > 0 && c.Cost >= c.RunBudget {
			logEvent(map[string]any{"event": "run-budget-reached", "before": s.String(), "cost_usd": c.Cost, "limit_usd": c.RunBudget})
			return GaveUp
		}
		visits[s]++
		c.Rounds = visits[Execute]
		o := steps[s].Run(c)
		if runCtx.Err() != nil { // a stopped step's result means nothing
			return Stopped
		}
		if !contains(steps[s].Outs, o) {
			panic(fmt.Sprintf("step %v returned undeclared out %v", s, o))
		}
		logEvent(map[string]any{"state": s.String(), "out": o.String(), "visit": visits[s], "cost_usd": c.Cost})
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
	if len(os.Args) >= 2 && os.Args[1] == "stats" {
		path := "agent-sm-stats.jsonl"
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
		fmt.Fprintln(os.Stderr, serve(issue, addr, repo))
		os.Exit(1)
	}
	if len(os.Args) >= 2 && os.Args[1] == "allow" {
		path := "agent-sm-stats.jsonl"
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
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: agent-sm <repo-dir> <issue-number> | agent-sm stats|allow [file] | agent-sm serve [--repo DIR] [issue] [addr]  (SM_PUBLISH=pr|merge to push, open PRs, merge; see README.md)")
		os.Exit(2)
	}
	n, err := strconv.Atoi(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "issue number must be an integer")
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
	c := &Ctx{Repo: os.Args[1], Issue: n, Level: level, Ghidra: "none", Scratch: filepath.Join(scratchRoot(), strconv.Itoa(n)), RunBudget: 20}
	if v, err := strconv.ParseFloat(os.Getenv("SM_RUN_BUDGET"), 64); err == nil && v >= 0 {
		c.RunBudget = v // 0 turns the limit off
	}
	if v := os.Getenv("SM_GHIDRA_COPY"); v != "" {
		c.Ghidra = v
	}
	if v := os.Getenv("SM_SCRATCH"); v != "" {
		c.Scratch = v
	}
	os.MkdirAll(c.Scratch, 0o755)
	eventsPath = os.Getenv("SM_EVENTS")
	if eventsPath == "" {
		eventsPath = filepath.Join(c.Scratch, "events.jsonl")
		// A new run starts a new event log; the earlier attempt's log is kept beside it.
		os.Rename(eventsPath, strings.TrimSuffix(eventsPath, ".jsonl")+"-"+time.Now().Format("20060102-150405")+".jsonl")
	}
	// SIGINT or SIGTERM (the page's Stop) ends the current step; the cleanup and exit event below still run.
	var stop context.CancelFunc
	runCtx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exit := run(c)
	followUp(c, exit)
	stopGhidra(c)
	cleanupWorktree(c)
	logEvent(map[string]any{"exit": string(exit), "cost_usd": c.Cost, "sessions_with_unknown_cost": c.CostUnknown})
	fmt.Println(exit)
	if exit != Ready {
		os.Exit(1)
	}
}
