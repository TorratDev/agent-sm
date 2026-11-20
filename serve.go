package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

// eventsPath is where logEvent also appends (set in main; SM_EVENTS overrides).
var eventsPath string

func json_unmarshal(b []byte, v any) bool { return json.Unmarshal(b, v) == nil }

func scratchRoot() string {
	if r := os.Getenv("SM_SCRATCH_ROOT"); r != "" {
		return r
	}
	return "/tmp/agent-sm"
}

func readLines(path string, keep int) []json.RawMessage {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []json.RawMessage
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "{") && json.Valid([]byte(l)) {
			out = append(out, json.RawMessage(l))
		}
	}
	if len(out) > keep {
		out = out[len(out)-keep:]
	}
	return out
}

// latestRun is the newest run directory under root (by the mtime of its status.json or events.jsonl).
func latestRun(root string) (dir, issue string) {
	var best int64
	dirs, _ := filepath.Glob(filepath.Join(root, "*"))
	for _, d := range dirs {
		for _, f := range []string{"status.json", "events.jsonl"} {
			if fi, err := os.Stat(filepath.Join(d, f)); err == nil && fi.ModTime().UnixNano() > best {
				best, dir, issue = fi.ModTime().UnixNano(), d, filepath.Base(d)
			}
		}
	}
	return dir, issue
}

// stallAfter: a run with no exit and no file written for this long is shown as stalled
// (a long silent step, or a process that died without logging its exit).
const stallAfter = 10 * time.Minute

type runStep struct {
	State string `json:"state"`
	Out   string `json:"out"`
}

// runSummary is one card on the board, read from a run's scratch directory.
type runSummary struct {
	Issue    string    `json:"issue"`
	Title    string    `json:"title"`
	Steps    []runStep `json:"steps"`
	Exit     string    `json:"exit,omitempty"`
	Live     string    `json:"live,omitempty"` // the state running now, from next() on the last step
	Stalled  bool      `json:"stalled,omitempty"`
	LastTool string    `json:"last_tool,omitempty"`
	Cost     float64   `json:"cost_usd"`
	Started  string    `json:"started,omitempty"`
	Ended    string    `json:"ended,omitempty"`
	PR       string    `json:"pr,omitempty"`
	Feedback string    `json:"feedback,omitempty"` // first line of the last check/review failure
	Verdict  string    `json:"verdict,omitempty"`  // first line of the reviewer's reasons
}

// firstLine keeps the first line of s, plus the next one when the first is only a heading
// ("review (REVISE):" is followed by the reasons).
func firstLine(s string) string {
	first, rest, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if strings.HasSuffix(strings.TrimSpace(first), ":") {
		next, _, _ := strings.Cut(rest, "\n")
		first += " " + strings.TrimLeft(strings.TrimSpace(next), "- ")
	}
	return oneLine(first, 200)
}

// readRun summarizes the run in dir. ok is false when the directory holds no run.
func readRun(dir string, now time.Time) (rs runSummary, ok bool) {
	evPath := filepath.Join(dir, "events.jsonl")
	fi, err := os.Stat(evPath)
	if err != nil {
		return rs, false
	}
	updated := fi.ModTime()
	rs = runSummary{Issue: filepath.Base(dir), Steps: []runStep{}}
	for _, l := range readLines(evPath, 5000) {
		var e struct {
			State, Out, Exit, Event, URL, T string
			Cost                            float64 `json:"cost_usd"`
		}
		if json.Unmarshal(l, &e) != nil {
			continue
		}
		if e.T != "" && rs.Started == "" {
			rs.Started = e.T
		}
		switch {
		case e.Event == "resume":
			rs.Exit, rs.Ended = "", "" // a resumed run continues after its earlier exit
		case e.State != "" && e.Out != "":
			rs.Steps = append(rs.Steps, runStep{e.State, e.Out})
			rs.Cost = e.Cost
		case e.Exit != "":
			rs.Exit, rs.Ended, rs.Cost = e.Exit, e.T, e.Cost
		case e.Event == "pr-opened":
			rs.PR = e.URL
		}
	}
	var c struct{ Title, Feedback, Verdict string }
	if b, err := os.ReadFile(filepath.Join(dir, "ctx.json")); err == nil && json.Unmarshal(b, &c) == nil {
		rs.Title, rs.Feedback, rs.Verdict = c.Title, firstLine(c.Feedback), firstLine(c.Verdict)
	}
	if rs.Exit != "" {
		return rs, true
	}
	rs.Live = liveState(rs.Steps)
	var st struct {
		LastTool string `json:"last_tool"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "status.json")); err == nil && json.Unmarshal(b, &st) == nil {
		rs.LastTool = st.LastTool
		if fi, err := os.Stat(filepath.Join(dir, "status.json")); err == nil && fi.ModTime().After(updated) {
			updated = fi.ModTime()
		}
	}
	rs.Stalled = now.Sub(updated) > stallAfter
	return rs, true
}

// liveState replays the logged steps through next() to find the state running now.
// status.json cannot say: it keeps the last agent step while check or publish run.
func liveState(steps []runStep) (live string) {
	if len(steps) == 0 {
		return Prepare.String()
	}
	defer func() { // a log from an older table can hold a pair next() no longer knows
		if recover() != nil {
			live = ""
		}
	}()
	byName := map[string]State{}
	for s := State(0); s < numStates; s++ {
		byName[s.String()] = s
	}
	outByName := map[string]Out{}
	for o := Out(0); o < numOuts; o++ {
		outByName[o.String()] = o
	}
	var visits [numStates]int
	var n Next
	for _, st := range steps {
		s, ok1 := byName[st.State]
		o, ok2 := outByName[st.Out]
		if !ok1 || !ok2 {
			return ""
		}
		visits[s]++
		n = next(s, o, visits)
		if o == More {
			visits[s] = 0 // as run() does
		}
	}
	if n.Exit != "" {
		return "" // the exit event is about to be written
	}
	return n.State.String()
}

// serve shows a run live: GET / the page, /api/graph the transitions, /api/state the run so far.
// It reads files only (status.json, events, stats), so it can also replay a finished run.
// With no issue it follows the newest run under SM_SCRATCH_ROOT (default /tmp/agent-sm).
// With repo set it also lists the repo's open issues and starts runs from the page (localhost,
// token-protected, one at a time).
func serve(issue, addr, repo string) error {
	root := scratchRoot()
	statsPath := os.Getenv("SM_STATS")
	if statsPath == "" {
		statsPath = "agent-sm-stats.jsonl"
	}
	statsPath, _ = filepath.Abs(statsPath)
	tok := make([]byte, 16)
	rand.Read(tok)
	token := hex.EncodeToString(tok)

	var run *Runner
	if repo != "" {
		self, _ := os.Executable()
		self = strings.TrimSuffix(self, " (deleted)") // a rebuild replaces the file under a running server; the path still points at the new binary
		if v := os.Getenv("SM_SELF"); v != "" {       // test hook: a stand-in for the agent-sm binary
			self = v
		}
		repo, _ = filepath.Abs(repo)
		run = &Runner{repo: repo, self: self, stats: statsPath}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(string(indexHTML), "__TOKEN__", token, 1)))
	})
	mux.HandleFunc("/api/graph", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(edges()) })
	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		runs := []runSummary{}
		dirs, _ := filepath.Glob(filepath.Join(root, "*"))
		for _, d := range dirs {
			if _, err := strconv.Atoi(filepath.Base(d)); err == nil {
				if rs, ok := readRun(d, time.Now()); ok {
					runs = append(runs, rs)
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"runs": runs, "stations": stateNames, "limits": map[string]int{"execute": maxExecute}})
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		scratch, iss := os.Getenv("SM_SCRATCH"), issue
		events := os.Getenv("SM_EVENTS")
		if n, err := strconv.Atoi(r.URL.Query().Get("issue")); err == nil && n > 0 { // a card picked on the board
			iss = strconv.Itoa(n)
			scratch, events = filepath.Join(root, iss), ""
		}
		if scratch == "" && iss != "" {
			scratch = filepath.Join(root, iss)
		}
		if scratch == "" {
			scratch, iss = latestRun(root)
		}
		if events == "" {
			events = filepath.Join(scratch, "events.jsonl")
		}
		var status json.RawMessage
		if b, err := os.ReadFile(filepath.Join(scratch, "status.json")); err == nil && json.Valid(b) {
			status = b
		}
		var stats []json.RawMessage
		for _, l := range readLines(statsPath, 2000) { // the stats file can hold many issues
			var h struct{ Issue json.Number }
			if json.Unmarshal(l, &h) == nil && (iss == "" || h.Issue.String() == iss) {
				stats = append(stats, l)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"issue": iss, "status": status, "events": readLines(events, 500), "stats": stats})
	})

	// Starting and stopping runs: only with --repo, and only with the page's token.
	var (
		issuesMu   sync.Mutex
		issuesJSON string
		issuesAt   time.Time
	)
	mux.HandleFunc("/api/queue", func(w http.ResponseWriter, r *http.Request) {
		if run == nil {
			json.NewEncoder(w).Encode(map[string]any{"can_run": false})
			return
		}
		json.NewEncoder(w).Encode(run.Snapshot())
	})
	mux.HandleFunc("/api/issues", func(w http.ResponseWriter, r *http.Request) {
		if run == nil {
			http.Error(w, "start the server with --repo to list issues", http.StatusNotFound)
			return
		}
		issuesMu.Lock()
		defer issuesMu.Unlock()
		if r.URL.Query().Get("refresh") != "" || time.Since(issuesAt) > 30*time.Second {
			out, err := sh(run.repo, "gh", "issue", "list", "--state", "open", "--limit", "100", "--json", "number,title,labels,updatedAt")
			if err != nil {
				http.Error(w, "gh issue list failed: "+tail(out, 300), http.StatusBadGateway)
				return
			}
			issuesJSON, issuesAt = out, time.Now()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(issuesJSON))
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if run == nil || r.Method != http.MethodPost || r.Header.Get("X-SM-Token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var req struct {
			Issues  []int  `json:"issues"`
			Publish string `json:"publish"`
			Audit   bool   `json:"audit"`
			Budget  string `json:"budget"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req) != nil || len(req.Issues) == 0 || len(req.Issues) > 20 ||
			(req.Publish != "" && req.Publish != levelPR) || (req.Budget != "" && !budgetRe.MatchString(req.Budget)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var jobs []Job
		for _, n := range req.Issues {
			if n <= 0 {
				http.Error(w, "bad issue number", http.StatusBadRequest)
				return
			}
			jobs = append(jobs, Job{Issue: n, Publish: req.Publish, Audit: req.Audit, Budget: req.Budget})
		}
		run.Enqueue(jobs)
		json.NewEncoder(w).Encode(run.Snapshot())
	})
	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			run.Stop()
			json.NewEncoder(w).Encode(run.Snapshot())
		}
	})

	// Only the page on this exact host may use the API: a foreign Host header (DNS rebinding)
	// or a cross-site Origin is refused.
	_, port, _ := net.SplitHostPort(addr)
	okHost := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true, "[::1]:" + port: true}
	guard := func(w http.ResponseWriter, r *http.Request) {
		if !okHost[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !okHost[strings.TrimPrefix(strings.TrimPrefix(o, "http://"), "https://")] {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}

	what := "issue " + issue
	if issue == "" {
		what = "the newest run under " + root
	}
	mode := "view only"
	if run != nil {
		mode = "can start runs in " + repo
	}
	fmt.Fprintf(os.Stderr, "agent-sm: http://%s  (showing %s; %s; stats %s)\n", addr, what, mode, statsPath)
	return http.ListenAndServe(addr, http.HandlerFunc(guard))
}
