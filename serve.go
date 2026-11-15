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
	// Durable by default: run history must survive a reboot, which /tmp does not.
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "agent-sm")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "agent-sm")
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

// serve shows a run live: GET / the page, /api/graph the transitions, /api/state the run so far.
// It reads files only (status.json, events, stats), so it can also replay a finished run.
// With no issue it follows the newest run under SM_SCRATCH_ROOT (default ~/.local/state/agent-sm).
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
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		scratch, iss := os.Getenv("SM_SCRATCH"), issue
		if scratch == "" && issue != "" {
			scratch = filepath.Join(root, issue)
		}
		if scratch == "" {
			scratch, iss = latestRun(root)
		}
		events := os.Getenv("SM_EVENTS")
		if events == "" {
			events = filepath.Join(scratch, "events.jsonl")
		}
		var status json.RawMessage
		if b, err := os.ReadFile(filepath.Join(scratch, "status.json")); err == nil && json.Valid(b) {
			status = b
		}
		json.NewEncoder(w).Encode(map[string]any{"issue": iss, "status": status, "events": readLines(events, 500)})
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
