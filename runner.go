package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Job is one issue to process, started from the web page.
type Job struct {
	Issue      int               `json:"issue"`
	Repo       string            `json:"repo,omitempty"`
	Publish    string            `json:"publish"` // "" (local only) or "pr"; merge is CLI-only
	Audit      bool              `json:"audit"`
	Budget     string            `json:"budget"`
	Models     map[string]string `json:"models,omitempty"` // state -> model chosen on the page
	Started    string            `json:"started,omitempty"`
	Worktree   string            `json:"worktree,omitempty"`
	Exit       string            `json:"exit,omitempty"`
	Err        string            `json:"err,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Base       string            `json:"base,omitempty"`
	Branch     string            `json:"branch,omitempty"`
	Profile    *Profile          `json:"-"`
	Management string            `json:"management,omitempty"`
}

// Runner runs queued jobs, each as `agent-sm <worktree> <issue>` in its own worktree and
// process group so Stop can end the whole run (claude, probes and all). Jobs run at once up to
// the free slots, which every repository's runner shares; one issue never runs twice at once.
type Runner struct {
	repo, self, stats string
	slots             *slots // shared across runners; nil means one job at a time
	mu                sync.Mutex
	idle              *sync.Cond // signalled when a job finishes
	wt                sync.Mutex // git worktree add/fetch in one repository, one at a time
	queue             []Job
	running           []*Job
	cmds              map[*Job]*exec.Cmd
	stopped           map[*Job]bool
	history           []Job
	working           bool
}

var (
	budgetRe       = regexp.MustCompile(`^\d+(\.\d+)?$`)
	modelRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{0,63}$`)
	worktreeNameRe = regexp.MustCompile(`^([1-9][0-9]*)-sm(?:-[1-9][0-9]*)?$`)
)

// agentModels is each agent state's current default model, for the page's pickers.
func agentModels() map[string]string {
	return agentModelsFor(settings)
}

func agentModelsFor(profile Profile) map[string]string {
	m := map[string]string{}
	for _, st := range steps {
		if st.Role != "" {
			m[st.Name] = profile.model(st)
		}
	}
	return m
}

func (r *Runner) Enqueue(jobs []Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slots == nil {
		r.slots = newSlots(1)
	}
	if r.idle == nil {
		r.idle = sync.NewCond(&r.mu)
		r.cmds, r.stopped = map[*Job]*exec.Cmd{}, map[*Job]bool{}
	}
	r.queue = append(r.queue, jobs...)
	if !r.working {
		r.working = true
		go r.loop()
	}
}

// next is the index of the first queued job whose issue is not running, or -1. Holds r.mu.
func (r *Runner) next() int {
	for i, j := range r.queue {
		if !slices.ContainsFunc(r.running, func(x *Job) bool { return x.Issue == j.Issue }) {
			return i
		}
	}
	return -1
}

// loop starts queued jobs as slots free up, until the queue is empty.
func (r *Runner) loop() {
	for {
		r.mu.Lock()
		for len(r.queue) > 0 && r.next() < 0 { // only issues already running are queued
			r.idle.Wait()
		}
		if len(r.queue) == 0 {
			r.working = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		r.slots.acquire()
		r.mu.Lock()
		i := r.next()
		if i < 0 { // Stop emptied the queue, or a job for the same issue started meanwhile
			r.mu.Unlock()
			r.slots.release()
			continue
		}
		j := new(Job)
		*j = r.queue[i]
		r.queue = slices.Delete(r.queue, i, i+1)
		j.Started = time.Now().Format(time.RFC3339)
		if j.Repo == "" {
			j.Repo = r.repo
		}
		r.running = append(r.running, j)
		r.mu.Unlock()
		go func() {
			defer r.slots.release()
			r.runOne(j)
			r.mu.Lock()
			r.running = slices.DeleteFunc(r.running, func(x *Job) bool { return x == j })
			delete(r.cmds, j)
			delete(r.stopped, j)
			r.history = append(r.history, *j)
			if len(r.history) > 30 {
				r.history = r.history[1:]
			}
			r.idle.Broadcast()
			r.mu.Unlock()
		}()
	}
}

// slots bounds how many jobs run at once; the page can change the bound while jobs run.
type slots struct {
	mu          sync.Mutex
	free        *sync.Cond
	limit, used int
}

func newSlots(limit int) *slots {
	s := &slots{limit: max(1, limit)}
	s.free = sync.NewCond(&s.mu)
	return s
}

func (s *slots) acquire() {
	s.mu.Lock()
	for s.used >= s.limit {
		s.free.Wait()
	}
	s.used++
	s.mu.Unlock()
}

func (s *slots) release() { s.mu.Lock(); s.used--; s.free.Broadcast(); s.mu.Unlock() }

// resize changes the bound; lowering it lets running jobs finish and holds new ones back.
func (s *slots) resize(limit int) {
	s.mu.Lock()
	s.limit = max(1, limit)
	s.free.Broadcast()
	s.mu.Unlock()
}

func (s *slots) size() int {
	if s == nil {
		return 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

func (r *Runner) set(f func()) { r.mu.Lock(); f(); r.mu.Unlock() }

func (r *Runner) worktreeRoot() string {
	return filepath.Join(filepath.Dir(r.repo), filepath.Base(r.repo)+"-worktrees")
}

// leftovers lists the issues whose worktree from an earlier run is still there.
func (r *Runner) leftovers() []int {
	out := []int{}
	dirs, _ := filepath.Glob(filepath.Join(r.worktreeRoot(), "*-sm*"))
	for _, d := range dirs {
		match := worktreeNameRe.FindStringSubmatch(filepath.Base(d))
		if match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	slices.Sort(out)
	return out
}

// worktreeAt reserves a fresh branch and worktree without touching earlier attempts.
func (r *Runner) worktreeAt(n int, base string) (string, string, error) {
	branch, dir, err := nextIssueBranch(r.repo, n, r.worktreeRoot())
	if err != nil {
		return "", "", err
	}
	if strings.HasPrefix(base, "origin/") {
		if out, err := sh(r.repo, "git", "fetch", "--", "origin", strings.TrimPrefix(base, "origin/")); err != nil {
			return "", "", fmt.Errorf("fetch base: %s", tail(out, 300))
		}
	}
	if out, err := sh(r.repo, "git", "worktree", "add", "-b", branch, dir, base); err != nil {
		return "", "", fmt.Errorf("git worktree add: %s", tail(out, 300))
	}
	for _, f := range strings.Split(os.Getenv("SM_LINKS"), ",") { // gitignored files the run needs, e.g. the ROM dump
		if f = strings.TrimSpace(f); f != "" {
			os.Symlink(filepath.Join(r.repo, f), filepath.Join(dir, f))
		}
	}
	return dir, branch, nil
}

func (j *Job) scratch() string {
	dir := runScratch(j.Repo, j.Issue)
	if suffix := strings.TrimPrefix(j.Branch, issueBranchName(j.Issue, 0)); suffix != "" {
		dir = filepath.Join(dir, "attempt"+suffix)
	}
	return dir
}

func childEnv(j *Job, stats string) []string {
	skip := map[string]bool{"SM_SCRATCH": true, "SM_EVENTS": true, "SM_PUBLISH": true, "SM_AUDIT": true, "SM_BUDGET": true, "SM_STATS": true, "SM_RESOLVED_CONFIG": true, "SM_WORKTREE_BASE": true, "SM_WORKTREE_BRANCH": true}
	var env []string
	for _, e := range os.Environ() {
		if k, _, _ := strings.Cut(e, "="); !skip[k] {
			env = append(env, e)
		}
	}
	env = append(env, "SM_STATS="+stats)
	if j.Publish == levelPR {
		env = append(env, "SM_PUBLISH=pr")
	}
	if j.Audit {
		env = append(env, "SM_AUDIT=1")
	}
	if j.Budget != "" {
		env = append(env, "SM_BUDGET="+j.Budget)
	}
	for state, m := range j.Models { // appended last, so the page's choice wins over the server's environment
		env = append(env, "SM_MODEL_"+strings.ToUpper(state)+"="+m)
	}
	p := settings
	if j.Profile != nil {
		p = *j.Profile
	}
	models := map[string]string{}
	for state, m := range p.Models {
		models[state] = m
	}
	for state, m := range j.Models {
		models[state] = m
	}
	p.Models = models
	b, _ := json.Marshal(p)
	scratch := j.scratch()
	env = append(env, "SM_RESOLVED_CONFIG="+string(b), "SM_WORKTREE_BASE="+j.Base, "SM_WORKTREE_BRANCH="+j.Branch,
		"SM_SCRATCH="+scratch, "SM_EVENTS="+filepath.Join(scratch, "events.jsonl"))
	return env
}

func (r *Runner) runOne(j *Job) {
	if j.Repo == "" {
		j.Repo = r.repo
	}
	p := settings
	if j.Profile != nil {
		p = *j.Profile
	}
	out, err := sh(r.repo, "gh", "issue", "view", strconv.Itoa(j.Issue), "--json", "body")
	var issue struct{ Body string }
	if err != nil || json.Unmarshal([]byte(out), &issue) != nil {
		r.set(func() { j.Err = "cannot read issue metadata before creating worktree" })
		return
	}
	meta, err := issueMetadata(issue.Body)
	if err != nil {
		r.set(func() { j.Err = err.Error() })
		return
	}
	base := p.Base
	if meta.Base != "" {
		base = meta.Base
	}
	r.set(func() { j.Base = base })
	r.wt.Lock()
	wt, branch, err := r.worktreeAt(j.Issue, base)
	r.wt.Unlock()
	if err != nil {
		r.set(func() { j.Err = err.Error() })
		return
	}
	r.set(func() { j.Worktree, j.Branch = wt, branch })
	scratch := j.scratch()
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		r.set(func() { j.Err = err.Error() })
		return
	}
	eventsPath := filepath.Join(scratch, "events.jsonl")
	if _, err := os.Stat(eventsPath); err == nil {
		archive := filepath.Join(scratch, "events-"+time.Now().Format("20060102-150405")+".jsonl")
		if err := os.Rename(eventsPath, archive); err != nil {
			r.set(func() { j.Err = err.Error() })
			return
		}
	}
	logf, err := os.Create(filepath.Join(scratch, "run.log"))
	if err != nil {
		r.set(func() { j.Err = err.Error() })
		return
	}
	defer logf.Close()
	cmd := exec.Command(r.self, wt, strconv.Itoa(j.Issue))
	cmd.Env = childEnv(j, r.stats)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.mu.Lock()
	if r.stopped[j] { // stopped while the worktree was being made
		j.Exit = "stopped"
		r.mu.Unlock()
		return
	}
	err = cmd.Start()
	if err != nil {
		j.Err = err.Error()
	} else if r.cmds != nil {
		r.cmds[j] = cmd
	}
	r.mu.Unlock()
	if err != nil {
		return
	}
	cmd.Wait()
	exit := "died" // no exit event: the process ended abnormally
	diagnostic := ""
	for _, l := range readLines(eventsPath, 1<<20) {
		var e struct {
			Exit, Management string
			ManagementError  string `json:"management_error"`
			Err              string
			Issue            int
		}
		if !json_unmarshal(l, &e) {
			continue
		}
		if e.Err != "" {
			diagnostic = e.Err
			if e.Issue > 0 {
				diagnostic += fmt.Sprintf(" (#%d)", e.Issue)
			}
		}
		if e.Exit != "" {
			exit = e.Exit
			r.set(func() {
				j.Management = e.Management
				if e.ManagementError != "" {
					j.Err = "Synchronization incomplete: " + e.ManagementError
				}
			})
		}
	}
	r.mu.Lock()
	if r.stopped[j] {
		exit = "stopped"
	}
	j.Exit = exit
	if j.Err == "" && exit != string(Ready) && exit != "stopped" {
		j.Err = diagnostic
	}
	r.mu.Unlock()
	if p.Management.Mode == "service" && exit != string(Ready) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m, err := newManager(ctx, r.repo, p)
		if err == nil {
			err = m.deactivate(ctx, j.Issue, exit)
		}
		if err != nil {
			r.set(func() { j.Err = "synchronization deactivation failed: " + err.Error() })
		}
	}
}

// Stop clears the queue and ends every running job's whole process group.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = nil
	for _, j := range r.running {
		r.stopped[j] = true
		if cmd := r.cmds[j]; cmd != nil && cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}
	if r.idle != nil {
		r.idle.Broadcast()
	}
}

func runScratch(repo string, issue int) string {
	repoHash := sha256.Sum256([]byte(filepath.Clean(repo)))
	return filepath.Join(scratchRoot(), "repos", hex.EncodeToString(repoHash[:8]), strconv.Itoa(issue))
}

func (r *Runner) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	running := []Job{}
	for _, j := range r.running {
		running = append(running, *j)
	}
	return map[string]any{"can_run": true, "repo": r.repo, "leftover": r.leftovers(), "provider": settings.Provider,
		"models": agentModels(), "session_limit": settings.SessionLimit, "usage_unit": settings.unit(),
		"running": running, "concurrency": r.slots.size(), "queued": append([]Job{}, r.queue...), "history": append([]Job{}, r.history...)}
}
