package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Job is one issue to process, started from the web page.
type Job struct {
	Issue    int    `json:"issue"`
	Publish  string `json:"publish"` // "" (local only) or "pr"; merge is CLI-only
	Audit    bool   `json:"audit"`
	Budget   string `json:"budget"`
	Started  string `json:"started,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Exit     string `json:"exit,omitempty"`
	Err      string `json:"err,omitempty"`
}

// Runner runs queued jobs one at a time, each as `agent-sm <worktree> <issue>` in its own
// process group so Stop can end the whole run (claude, probes and all).
type Runner struct {
	repo, self, stats string
	mu                sync.Mutex
	queue             []Job
	cur               *Job
	cmd               *exec.Cmd
	history           []Job
	working, stopping bool
}

var budgetRe = regexp.MustCompile(`^\d+(\.\d+)?$`)

func (r *Runner) Enqueue(jobs []Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = append(r.queue, jobs...)
	if !r.working {
		r.working = true
		go r.loop()
	}
}

func (r *Runner) loop() {
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.working, r.cur = false, nil
			r.mu.Unlock()
			return
		}
		j := r.queue[0]
		r.queue = r.queue[1:]
		j.Started = time.Now().Format(time.RFC3339)
		r.cur = &j
		r.mu.Unlock()

		r.runOne(&j)

		r.mu.Lock()
		r.history = append(r.history, j)
		if len(r.history) > 30 {
			r.history = r.history[1:]
		}
		r.cur, r.cmd, r.stopping = nil, nil, false
		r.mu.Unlock()
	}
}

func (r *Runner) set(f func()) { r.mu.Lock(); f(); r.mu.Unlock() }

// worktree makes a fresh detached worktree of the base branch for the issue.
func (r *Runner) worktree(n int) (string, error) {
	dir := filepath.Join(filepath.Dir(r.repo), filepath.Base(r.repo)+"-worktrees", fmt.Sprintf("%d-sm", n))
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("worktree %s already exists: remove it with git worktree remove, or run it from the CLI", dir)
	}
	base := baseBranch()
	sh(r.repo, "git", "fetch", "origin", strings.TrimPrefix(base, "origin/"))
	if out, err := sh(r.repo, "git", "worktree", "add", "--detach", dir, base); err != nil {
		return "", fmt.Errorf("git worktree add: %s", tail(out, 300))
	}
	for _, f := range strings.Split(os.Getenv("SM_LINKS"), ",") { // gitignored files the run needs, e.g. the ROM dump
		if f = strings.TrimSpace(f); f != "" {
			os.Symlink(filepath.Join(r.repo, f), filepath.Join(dir, f))
		}
	}
	return dir, nil
}

func childEnv(j *Job, stats string) []string {
	skip := map[string]bool{"SM_SCRATCH": true, "SM_EVENTS": true, "SM_PUBLISH": true, "SM_AUDIT": true, "SM_BUDGET": true, "SM_STATS": true}
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
	return env
}

func (r *Runner) runOne(j *Job) {
	wt, err := r.worktree(j.Issue)
	if err != nil {
		r.set(func() { j.Err = err.Error() })
		return
	}
	r.set(func() { j.Worktree = wt })
	scratch := filepath.Join(scratchRoot(), strconv.Itoa(j.Issue))
	os.MkdirAll(scratch, 0o755)
	logf, _ := os.Create(filepath.Join(scratch, "run.log"))
	defer logf.Close()
	cmd := exec.Command(r.self, wt, strconv.Itoa(j.Issue))
	cmd.Env = childEnv(j, r.stats)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.set(func() { j.Err = err.Error() })
		return
	}
	r.set(func() { r.cmd = cmd })
	cmd.Wait()
	exit := "died" // no exit event: the process ended abnormally
	for _, l := range readLines(filepath.Join(scratch, "events.jsonl"), 1<<20) {
		var e struct{ Exit string }
		if json_unmarshal(l, &e) && e.Exit != "" {
			exit = e.Exit
		}
	}
	r.mu.Lock()
	if r.stopping {
		exit = "stopped"
	}
	j.Exit = exit
	r.mu.Unlock()
}

// Stop clears the queue and ends the running job's whole process group.
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue, r.stopping = nil, true
	if r.cmd != nil && r.cmd.Process != nil {
		syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
	}
}

func (r *Runner) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var cur *Job
	if r.cur != nil {
		c := *r.cur
		cur = &c
	}
	return map[string]any{"can_run": true, "repo": r.repo, "running": cur, "queued": append([]Job{}, r.queue...), "history": append([]Job{}, r.history...)}
}
