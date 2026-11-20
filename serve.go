package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed examples/go.json examples/dotnet.json
var languageProfileFiles embed.FS

func languageProfile(name string) (Profile, error) {
	path := map[string]string{"go": "examples/go.json", "dotnet": "examples/dotnet.json"}[name]
	if path == "" {
		return Profile{}, fmt.Errorf("unknown language profile %q", name)
	}
	data, err := languageProfileFiles.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	p := defaultProfile("copilot")
	if err := decodeStrict(data, &p); err != nil {
		return Profile{}, fmt.Errorf("language profile %q: %w", name, err)
	}
	if err := p.validate(); err != nil {
		return Profile{}, fmt.Errorf("language profile %q: %w", name, err)
	}
	return p, nil
}

func autoProvider() string {
	if _, err := exec.LookPath("copilot"); err == nil {
		return "copilot"
	}
	if _, err := exec.LookPath("claude"); err == nil {
		return "claude"
	}
	if _, err := exec.LookPath("opencode"); err == nil {
		return "opencode"
	}
	return "copilot"
}

// installedProviders says which agent CLIs are on PATH, for the page's agent switch.
func installedProviders() map[string]bool {
	m := map[string]bool{}
	for _, name := range []string{"claude", "copilot", "opencode"} {
		_, err := exec.LookPath(name)
		m[name] = err == nil
	}
	return m
}

func autoLanguageProfile(repo string) (Profile, error) {
	root := repo
	if root == "" {
		root = "."
	}
	canonical, err := filepath.Abs(root)
	if err != nil {
		return Profile{}, err
	}
	if _, err := os.Stat(filepath.Join(canonical, "go.mod")); err == nil {
		return languageProfile("go")
	}
	if _, err := os.Stat(filepath.Join(canonical, "Inventory.ChannelX.slnx")); err == nil {
		return languageProfile("dotnet")
	}
	p := defaultProfile("copilot")
	p.Checks = []string{"git diff --check"}
	return p, nil
}

func autoProfile(repo string) (Profile, error) {
	p, err := autoLanguageProfile(repo)
	if err != nil {
		return Profile{}, err
	}
	provider := autoProvider()
	if v := os.Getenv("SM_PROVIDER"); v != "" {
		provider = v
	}
	p.selectProvider(provider)
	if err := p.applyLimitEnv(); err != nil {
		return p, err
	}
	if err := p.applyTranscriptEnv(); err != nil {
		return p, err
	}
	return p, p.validate()
}

func canonicalRepo(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("repository path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	out, err := sh(absolute, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a Git working tree: %w: %s", err, tail(out, 300))
	}
	return filepath.Abs(strings.TrimSpace(out))
}

type folderEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type folderListing struct {
	Path        string        `json:"path"`
	Parent      string        `json:"parent"`
	Directories []folderEntry `json:"directories"`
	Repo        string        `json:"repo"`
	GitError    string        `json:"git_error,omitempty"`
}

func folderRoutes(mux *http.ServeMux, authed func(http.ResponseWriter, *http.Request) bool) {
	mux.HandleFunc("/api/folders", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var req *struct {
			Path string `json:"path"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&req) != nil || req == nil || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "bad folder request", http.StatusBadRequest)
			return
		}
		path := req.Path
		if path == "" {
			var err error
			path, err = os.UserHomeDir()
			if err != nil {
				http.Error(w, "cannot locate home folder: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		path, err := filepath.Abs(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			http.Error(w, "cannot read folder: "+err.Error(), http.StatusBadRequest)
			return
		}
		listing := folderListing{Path: path, Parent: filepath.Dir(path), Directories: []folderEntry{}}
		for _, entry := range entries {
			if entry.IsDir() {
				listing.Directories = append(listing.Directories, folderEntry{Name: entry.Name(), Path: filepath.Join(path, entry.Name())})
			}
		}
		listing.Repo, err = canonicalRepo(path)
		if err != nil {
			listing.GitError = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(listing)
	})
}

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
	filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (entry.Name() != "status.json" && entry.Name() != "events.jsonl") {
			return nil
		}
		if fi, err := entry.Info(); err == nil && fi.ModTime().UnixNano() > best {
			best, dir, issue = fi.ModTime().UnixNano(), filepath.Dir(path), filepath.Base(filepath.Dir(path))
		}
		return nil
	})
	if strings.HasPrefix(issue, "attempt-") {
		issue = filepath.Base(filepath.Dir(dir))
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
// With no issue it follows the newest run under SM_SCRATCH_ROOT (default ~/.local/state/agent-sm).
// The page can list issues and start runs from any selected local Git repository (localhost,
// token-protected, up to `concurrency` runs at once, each in its own worktree).
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

	defaultRepo := ""
	if repo != "" {
		defaultRepo, _ = filepath.Abs(repo)
	}
	self, _ := os.Executable()
	self = strings.TrimSuffix(self, " (deleted)") // a rebuild replaces the file under a running server; the path still points at the new binary
	if v := os.Getenv("SM_SELF"); v != "" {       // test hook: a stand-in for the agent-sm binary
		self = v
	}
	runners := map[string]*Runner{}
	var runnersMu sync.Mutex
	slots := newSlots(settings.Concurrency) // jobs running at once, across all repositories
	runnerFor := func(path string) (*Runner, error) {
		if path == "" {
			path = defaultRepo
		}
		canonical, err := canonicalRepo(path)
		if err != nil {
			return nil, err
		}
		if settings.Management.Mode == "service" && canonical != defaultRepo {
			return nil, fmt.Errorf("repository selection is unavailable with service management; use the configured repository")
		}
		runnersMu.Lock()
		defer runnersMu.Unlock()
		if r := runners[canonical]; r != nil {
			return r, nil
		}
		r := &Runner{repo: canonical, self: self, stats: statsPath, slots: slots}
		runners[canonical] = r
		return r, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		repoJSON, _ := json.Marshal(defaultRepo)
		providerJSON, _ := json.Marshal(settings.Provider)
		page := strings.Replace(string(indexHTML), "__TOKEN__", token, 1)
		page = strings.Replace(page, "__REPO__", string(repoJSON), 1)
		page = strings.Replace(page, "__AGENT__", string(providerJSON), 1)
		w.Write([]byte(page))
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
		json.NewEncoder(w).Encode(map[string]any{"runs": runs, "stations": stateNames(), "limits": map[string]int{"execute": maxExecute}})
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
		json.NewEncoder(w).Encode(map[string]any{"issue": iss, "status": status, "events": readLines(events, 500)})
	})

	// Starting and stopping runs use a selected local repository and the page's token.
	var (
		issuesMu    sync.Mutex
		issuesCache = map[string]struct {
			data string
			at   time.Time
		}{}
	)
	mux.HandleFunc("/api/queue", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("repo")
		if path == "" && defaultRepo == "" {
			json.NewEncoder(w).Encode(map[string]any{"can_run": false})
			return
		}
		run, err := runnerFor(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		profile, err := queuedProfile(runRequest{Repo: run.repo, Language: r.URL.Query().Get("language"), Provider: r.URL.Query().Get("provider")})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(queueSnapshot(run, profile))
	})
	mux.HandleFunc("/api/issues", func(w http.ResponseWriter, r *http.Request) {
		run, err := runnerFor(r.URL.Query().Get("repo"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		issuesMu.Lock()
		defer issuesMu.Unlock()
		cached := issuesCache[run.repo]
		if r.URL.Query().Get("refresh") != "" || time.Since(cached.at) > 30*time.Second {
			// The REST list, not `gh issue list`: it carries the sub-issue and blocked-by counts.
			out, err := sh(run.repo, "gh", "api", "repos/{owner}/{repo}/issues?state=open&per_page=100", "--jq",
				`[.[] | select(.pull_request | not) | {number, title, updatedAt: .updated_at, labels: [.labels[] | {name, color}], subs: (.sub_issues_summary.total // 0), subsDone: (.sub_issues_summary.completed // 0), blocked: (.issue_dependencies_summary.blocked_by // 0)}]`)
			if err != nil {
				http.Error(w, "listing issues failed: "+tail(out, 300), http.StatusBadGateway)
				return
			}
			cached = struct {
				data string
				at   time.Time
			}{data: out, at: time.Now()}
			issuesCache[run.repo] = cached
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(cached.data))
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.Header.Get("X-SM-Token") != token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		return true
	}
	folderRoutes(mux, authed)
	ctx, cancel := context.WithCancel(runCtx)
	defer cancel()
	var management *manager
	if defaultRepo != "" && settings.Management.Mode == "service" {
		var err error
		management, err = newManager(ctx, defaultRepo, settings)
		if err != nil {
			return err
		}
		go management.watch(ctx)
	}
	managementRoutes(mux, management, authed)
	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var req runRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || len(req.Issues) == 0 || len(req.Issues) > 20 ||
			(req.Publish != "" && req.Publish != levelPR) || req.Concurrency < 0 || req.Concurrency > maxConcurrency || (req.Budget != "" && !budgetRe.MatchString(req.Budget)) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		run, err := runnerFor(req.Repo)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Repo = run.repo
		profile, err := queuedProfile(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var jobs []Job
		notes := []string{}
		seen := map[int]bool{}
		for _, n := range req.Issues {
			if n <= 0 {
				http.Error(w, "bad issue number", http.StatusBadRequest)
				return
			}
			// An issue with sub-issues is replaced by the sub-issues that can run now.
			list, why := expand(run.repo, n)
			notes = append(notes, why...)
			for _, i := range list {
				if !seen[i] {
					seen[i] = true
					jobs = append(jobs, Job{Issue: i, Repo: run.repo, Publish: req.Publish, Audit: req.Audit, Budget: req.Budget,
						Models: req.Models, Provider: profile.Provider, Profile: &profile})
				}
			}
		}
		if req.Concurrency > 0 && run.slots != nil { // the page's choice applies to the whole server
			run.slots.resize(req.Concurrency)
		}
		run.Enqueue(jobs)
		snap := queueSnapshot(run, profile)
		snap["notes"] = notes
		json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			run, err := runnerFor(r.URL.Query().Get("repo"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			run.Stop()
			profile, err := queuedProfile(runRequest{Repo: run.repo, Language: r.URL.Query().Get("language"), Provider: r.URL.Query().Get("provider")})
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(queueSnapshot(run, profile))
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
	mode := "can select local repos and start runs from the page"
	fmt.Fprintf(os.Stderr, "agent-sm: http://%s  (showing %s; %s; stats %s)\n", addr, what, mode, statsPath)
	server := &http.Server{Addr: addr, Handler: http.HandlerFunc(guard), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		runnersMu.Lock()
		for _, r := range runners { // SIGINT: children run in their own process groups, so end them here
			r.Stop()
		}
		runnersMu.Unlock()
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		server.Shutdown(stopCtx)
	}()
	defer cancel()
	return server.ListenAndServe()
}

type runRequest struct {
	Issues      []int             `json:"issues"`
	Repo        string            `json:"repo,omitempty"`
	Language    string            `json:"language,omitempty"`
	Publish     string            `json:"publish"`
	Audit       bool              `json:"audit"`
	Transcripts *bool             `json:"transcripts,omitempty"`
	Budget      string            `json:"budget"`
	Models      map[string]string `json:"models"`
	Provider    string            `json:"provider"`
	Concurrency int               `json:"concurrency,omitempty"`
}

func queueSnapshot(r *Runner, p Profile) map[string]any {
	snap := r.Snapshot()
	snap["provider"], snap["models"] = p.Provider, agentModelsFor(p)
	snap["model_options"], snap["providers"] = modelOptions(p.Provider), installedProviders()
	snap["transcripts"], snap["audit"] = p.Transcripts, p.Audit
	snap["session_limit"], snap["usage_unit"] = p.SessionLimit, p.unit()
	snap["run_limit"], snap["min_session_limit"] = p.RunLimit, 0.01
	if p.Provider == "copilot" {
		snap["min_session_limit"] = minCopilotCredits
	}
	return snap
}

func queuedProfile(req runRequest) (Profile, error) {
	p := settings
	if req.Language == "auto" || (req.Language != "" && req.Language != "server") {
		var language Profile
		var err error
		if req.Language == "auto" {
			language, err = autoLanguageProfile(req.Repo)
		} else {
			language, err = languageProfile(req.Language)
		}
		if err != nil {
			return Profile{}, err
		}
		p.Checks, p.TestReports, p.Format, p.Commands = language.Checks, language.TestReports, language.Format, language.Commands
		p.Legacy = language.Legacy
	}
	if req.Provider != "" {
		p.selectProvider(req.Provider)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return Profile{}, err
	}
	var resolved Profile
	if err := json.Unmarshal(b, &resolved); err != nil {
		return resolved, err
	}
	if resolved.Models == nil {
		resolved.Models = map[string]string{}
	}
	resolved.Audit = req.Audit
	if req.Transcripts != nil {
		resolved.Transcripts = *req.Transcripts
	}
	if req.Budget != "" {
		resolved.SessionLimit, err = strconv.ParseFloat(req.Budget, 64)
		if err != nil {
			return resolved, fmt.Errorf("invalid session limit")
		}
	}
	for state, model := range req.Models {
		resolved.Models[state] = model
	}
	return resolved, resolved.validate()
}
