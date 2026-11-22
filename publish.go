package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Publish levels (SM_PUBLISH). Default "" stops at a local, staged branch.
//
//	"pr":    commit, rebase, re-check, push, open a draft PR, post findings and verdict.
//	"merge": also mark the PR ready, wait for checks, squash-merge it (the old "merge on PASS").
const (
	levelNone  = ""
	levelPR    = "pr"
	levelMerge = "merge"
)

func attribution() (string, string) {
	switch settings.Provider {
	case "claude":
		return "Claude <noreply@anthropic.com>", "Generated with Claude Code via agent-sm."
	case "opencode":
		return "OpenCode <noreply@opencode.ai>", "Generated with OpenCode via agent-sm."
	}
	return "Copilot App <223556219+Copilot@users.noreply.github.com>", "Generated with GitHub Copilot via agent-sm."
}

func gh(c *Ctx, args ...string) (string, bool) {
	out, err := sh(c.Repo, "gh", args...)
	if err != nil {
		logEvent(map[string]any{"event": "publish-error", "cmd": "gh " + strings.Join(args[:min(len(args), 3)], " "), "out": tail(out, 500)})
	}
	return strings.TrimSpace(out), err == nil
}

func git(c *Ctx, args ...string) (string, bool) {
	out, err := sh(c.Repo, "git", args...)
	if err != nil {
		logEvent(map[string]any{"event": "publish-error", "cmd": "git " + strings.Join(args[:min(len(args), 2)], " "), "out": tail(out, 500)})
	}
	return strings.TrimSpace(out), err == nil
}

// publish delivers a reviewed branch. Every failure is the Fail outcome (errored exit);
// the local branch is left as it is.
func publish(c *Ctx) Out {
	if c.Level == levelNone {
		return Ok
	}
	if err := checkRunBranch(c); err != nil {
		logEvent(map[string]any{"event": "publish-error", "err": err.Error()})
		return Fail
	}
	if why := handledElsewhere(c); why != "" {
		logEvent(map[string]any{"event": "publish", "stop": why})
		return Blocked
	}
	n := strconv.Itoa(c.Issue)
	branch := c.Branch
	if _, clean := sh(c.Repo, "git", "diff", "--cached", "--quiet"); clean == nil {
		logEvent(map[string]any{"event": "publish-error", "err": "nothing staged"})
		return Fail
	}
	author, footer := attribution()
	msg := fmt.Sprintf("%s (#%d)\n\nCo-authored-by: %s", c.Title, c.Issue, author)
	if _, ok := git(c, "commit", "-q", "-m", msg); !ok {
		return Fail
	}

	// Rebase onto the latest base branch and rerun the checks before pushing.
	base := c.Base
	if base == "" {
		base = baseBranch()
	}
	if strings.HasPrefix(base, "origin/") {
		if _, ok := git(c, "fetch", "--", "origin", strings.TrimPrefix(base, "origin/")); !ok {
			return Fail
		}
	}
	before, _ := git(c, "rev-parse", "HEAD")
	if _, ok := git(c, "rebase", base); !ok {
		git(c, "rebase", "--abort")
		return Fail
	}
	if after, _ := git(c, "rev-parse", "HEAD"); after != before {
		if !runChecks(c) {
			logEvent(map[string]any{"event": "publish-error", "err": "checks failed after rebase", "feedback": tail(c.Feedback, 500)})
			return Fail
		}
		if status, ok := git(c, "status", "--porcelain"); !ok || status != "" {
			logEvent(map[string]any{"event": "publish-error", "err": "checks changed the rebased worktree"})
			return Fail
		}
	}
	// Create only: a branch pushed by another run must never be overwritten.
	if _, ok := git(c, "push", "--force-with-lease=refs/heads/"+branch+":", "-u", "origin", branch); !ok {
		return Fail
	}

	prBody := fmt.Sprintf("Close #%d\n\n%s\n\n%s", c.Issue, c.Summary, footer)
	title := c.Title
	if c.JiraKey != "" && !jiraTitleKey(title, c.JiraKey) {
		title = c.JiraKey + " " + title
	}
	url, ok := gh(c, "pr", "create", "--draft", "--base", strings.TrimPrefix(base, "origin/"), "--head", branch, "--title", title, "--body", prBody)
	if !ok {
		return Fail
	}
	pr := url[strings.LastIndex(url, "/")+1:]
	c.PR = pr
	logEvent(map[string]any{"event": "pr-opened", "url": url})
	if settings.Management.Mode == "service" {
		number, err := strconv.Atoi(pr)
		if err != nil || number <= 0 {
			c.ManagementError = "invalid created PR identity"
			return Fail
		}
		if err := manageRun(c, false, number, "PR: "+url+"\nPASS: "+c.Verdict+"\n"+c.Evidence, false); err != nil {
			return Fail
		}
	}

	if settings.Management.Mode != "service" {
		if c.PointsDerived && len(c.Points) > 0 { // the planner derived the points: post them on the issue
			f := filepath.Join(c.Scratch, "points.md")
			if os.WriteFile(f, []byte("Derived points:\n\n- "+strings.Join(c.Points, "\n- ")), 0o644) == nil {
				gh(c, "issue", "comment", n, "--body-file", f)
			}
		}
		if c.Evidence != "" { // the investigator's findings, for later readers of the issue
			f := filepath.Join(c.Scratch, "findings.md")
			if os.WriteFile(f, []byte(c.Evidence), 0o644) == nil {
				gh(c, "issue", "comment", n, "--body-file", f)
			}
		}
		gh(c, "pr", "comment", pr, "--body", "PASS: "+c.Verdict)
	}

	if c.Level != levelMerge {
		return Ok
	}
	if _, ok := gh(c, "pr", "ready", pr); !ok {
		return Fail
	}
	// shortcut: --watch has no timeout; main has no required checks, so "no checks" passes.
	if out, ok := gh(c, "pr", "checks", pr, "--watch"); !ok && !strings.Contains(out, "no checks reported") {
		return Fail
	}
	gh(c, "pr", "merge", pr, "--squash", "--delete-branch") // local branch delete can fail in a worktree
	if st, _ := gh(c, "pr", "view", pr, "--json", "state", "--jq", ".state"); st != "MERGED" {
		return Fail
	}
	c.Merged = true
	logEvent(map[string]any{"event": "merged", "pr": pr, "note": "the worktree is left in place: remove it with git worktree remove"})
	return Ok
}

// followUpReason says why a run needs a follow-up issue, or "" when it needs none.
func followUpReason(c *Ctx, exit Exit) string {
	switch {
	case exit == SplitExit:
		return "work does not fit one PR"
	case exit == GaveUp && c.Rounds >= maxExecute:
		return fmt.Sprintf("%d REVISE rounds without a PASS", maxExecute)
	case exit == Ready && c.Partial: // the PR closes the issue, so the rest needs its own
		return "the PR delivers part of the work"
	}
	return ""
}

// followUpBody is the done and remaining work for a follow-up, shortened by a Haiku session.
// The raw notes stand in when that session fails, so a follow-up is never lost to it.
func followUpBody(c *Ctx, reason string) string {
	raw := fmt.Sprintf("## Done so far\n%s\n\n## Remaining work\n%s", c.Summary, c.Feedback+c.Evidence)
	var r struct{ Body string }
	prompt := fmt.Sprintf("Issue #%d: %s\nReason for the follow-up: %s\n\n%s", c.Issue, c.Title, reason, tail(raw, 60000))
	if agent(c, Summarize, prompt, &r) != Ok || strings.TrimSpace(r.Body) == "" {
		return tail(raw, 60000)
	}
	return r.Body
}

// followUp files the follow-up issue after 3 REVISE rounds or a partial delivery, and comments a split's breakdown on the issue.
// It needs a publish level; SM_EPIC, when set, is named in the follow-up.
func followUp(c *Ctx, exit Exit) {
	reason := followUpReason(c, exit)
	if settings.Management.Mode == "service" {
		if c.Proposal != nil || reason != "" {
			if err := pendingFollowUp(c, exit); err != nil {
				c.ManagementError = "cannot persist issue proposal: " + err.Error()
				logEvent(map[string]any{"event": "issue-proposal-error", "err": err.Error()})
			}
		}
		return
	}
	if c.Level == levelNone || reason == "" {
		return
	}
	work := followUpBody(c, reason)
	if exit == SplitExit { // no PR, so the issue stays open: a copy of it would only split the same way again
		gh(c, "issue", "comment", strconv.Itoa(c.Issue), "--body", fmt.Sprintf(
			"agent-sm stopped: %s. Split this issue into sub-issues; agent-sm runs them in its place.\n\n%s", reason, work))
		logEvent(map[string]any{"event": "split-comment", "issue": c.Issue})
		return
	}
	body := fmt.Sprintf("%s\n\nReason for split: %s\n\nParent: #%d\nDependencies: #%d", work, reason, c.Issue, c.Issue)
	if epic := os.Getenv("SM_EPIC"); epic != "" {
		body += "\nEpic: #" + epic
	}
	if c.PR != "" {
		body += fmt.Sprintf("\nCreated by: PR #%s, which delivers part of #%d", c.PR, c.Issue)
	}
	url, ok := gh(c, "issue", "create", "--title", c.Title, "--body", body, "--label", "follow-up")
	if !ok { // e.g. no "follow-up" label: keep the remaining work on the issue, or it is lost
		gh(c, "issue", "comment", strconv.Itoa(c.Issue), "--body", fmt.Sprintf(
			"agent-sm ended (%s: %s) and could not file a follow-up issue.\n\n%s", exit, reason, work))
		return
	}
	logEvent(map[string]any{"event": "follow-up", "url": url})
	num := url[strings.LastIndex(url, "/")+1:]
	// a real sub-issue of the issue it came from, so that issue's sub-issue list shows what is left
	if id, ok := gh(c, "api", "repos/{owner}/{repo}/issues/"+num, "--jq", ".id"); ok {
		gh(c, "api", "-X", "POST", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/sub_issues", c.Issue), "-F", "sub_issue_id="+id)
	}
	if c.PR != "" {
		gh(c, "pr", "comment", c.PR, "--body", fmt.Sprintf("This PR delivers part of #%d. The remaining work is in #%s, a new sub-issue of #%d.", c.Issue, num, c.Issue))
	}
}

// Diagnosis is what the auditor found in a failed run's logs, kept as diagnosis.json in its
// scratch directory; the board shows it and can file it as an issue (Issue holds that URL).
type Diagnosis struct {
	Exit  string `json:"exit"`
	Cause string `json:"cause"`
	Fix   string `json:"fix"`
	Kind  string `json:"kind"` // "bug" or "follow-up"
	Title string `json:"title"`
	Body  string `json:"body"`
	Issue string `json:"issue,omitempty"`
}

var diagnosing sync.Map // run directories with an audit going, so a double click starts one

// diagnoseRun runs the auditor on the finished run in dir (the page's Diagnose button) and
// writes diagnosis.json there. It never runs on its own: the audit is a paid session.
// The audit runs in a child `agent-sm diagnose` with profile, as runs do, so it uses the run's
// agent without touching the server's own settings that other requests read.
func diagnoseRun(dir, repo, exit string, profile Profile) (*Diagnosis, error) {
	if _, busy := diagnosing.LoadOrStore(dir, true); busy {
		return nil, fmt.Errorf("a diagnosis of this run is already going")
	}
	defer diagnosing.Delete(dir)
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(runCtx, self, "diagnose", dir, repo, exit)
	for _, e := range os.Environ() { // the audit must not write into the run's own events
		if k, _, _ := strings.Cut(e, "="); k != "SM_RESOLVED_CONFIG" && k != "SM_EVENTS" && k != "SM_SCRATCH" {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "SM_RESOLVED_CONFIG="+string(b))
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("the auditor failed: %s", tail(string(out), 300))
	}
	var d Diagnosis
	b, err = os.ReadFile(filepath.Join(dir, "diagnosis.json"))
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	return &d, err
}

// runProvider is the agent a run used, from the provider its events record; "" when none does.
func runProvider(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	provider := ""
	for _, line := range strings.Split(string(b), "\n") {
		var e struct {
			Provider string `json:"provider"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Provider != "" {
			provider = e.Provider
		}
	}
	return provider
}

// auditRun is the `agent-sm diagnose` child: one auditor session with the profile it was given.
func auditRun(dir, repo, exit string) (*Diagnosis, error) {
	c := &Ctx{}
	if b, err := os.ReadFile(filepath.Join(dir, "ctx.json")); err == nil {
		json.Unmarshal(b, c)
	}
	if fi, err := os.Stat(c.Repo); err != nil || !fi.IsDir() { // the run's worktree may be gone
		c.Repo = repo
	}
	c.Scratch = dir
	c.RunBudget, c.Cost, c.CostUnknown = 0, 0, 0 // the run's spent budget must not stop its audit; SessionLimit still applies
	events, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	prompt := fmt.Sprintf("Issue #%d: %s\nExit: %s\nScratch directory: %s (run.log, events.jsonl, ctx.json, transcripts/)\nRepository: %s\n\nLast feedback:\n%s\n\nLast verdict:\n%s\n\nEvent log (tail):\n%s",
		c.Issue, c.Title, exit, dir, c.Repo, tail(c.Feedback, 4000), tail(c.Verdict, 2000), tail(string(events), 20000))
	var d Diagnosis
	if o := agent(c, Diagnose, prompt, &d); o != Ok {
		return nil, fmt.Errorf("the auditor session ended %s", o)
	}
	d.Exit = exit
	b, _ := json.MarshalIndent(d, "", "  ")
	return &d, os.WriteFile(filepath.Join(dir, "diagnosis.json"), b, 0o644)
}

var fileDiagnosisMu sync.Mutex // one filing at a time, so a double click files one issue

// fileDiagnosis creates the issue a diagnosis proposes and records its URL in diagnosis.json.
// A diagnosis already filed returns its issue again.
func fileDiagnosis(dir, repo string, issue int) (string, error) {
	fileDiagnosisMu.Lock()
	defer fileDiagnosisMu.Unlock()
	path := filepath.Join(dir, "diagnosis.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("this run has no diagnosis")
	}
	var d Diagnosis
	if err := json.Unmarshal(b, &d); err != nil || d.Title == "" {
		return "", fmt.Errorf("diagnosis.json is invalid")
	}
	if d.Issue != "" {
		return d.Issue, nil
	}
	body := fmt.Sprintf("%s\n\nFound by the agent-sm auditor in the failed run of #%d (exit: %s).", d.Body, issue, d.Exit)
	args := []string{"issue", "create", "--title", d.Title, "--body", body}
	out, err := sh(repo, "gh", append(args, "--label", d.Kind)...)
	if err != nil { // the repo may have no such label
		out, err = sh(repo, "gh", args...)
	}
	if err != nil {
		return "", fmt.Errorf("gh issue create: %s", tail(out, 300))
	}
	d.Issue = strings.TrimSpace(out)
	if d.Kind == "follow-up" { // a sub-issue of the run's issue, as followUp files them
		num := d.Issue[strings.LastIndex(d.Issue, "/")+1:]
		if id, err := sh(repo, "gh", "api", "repos/{owner}/{repo}/issues/"+num, "--jq", ".id"); err == nil {
			sh(repo, "gh", "api", "-X", "POST", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/sub_issues", issue), "-F", "sub_issue_id="+strings.TrimSpace(id))
		}
	}
	b, _ = json.MarshalIndent(d, "", "  ")
	return d.Issue, os.WriteFile(path, b, 0o644)
}

// fixesBaseMarker in an issue body skips the base check in prepare: that issue is the fix.
const fixesBaseMarker = "agent-sm: fixes-base"

// baseBug files (or reuses) one bug issue for a base that fails its checks, and links the
// blocked issue to it, so the break gets fixed by an issue allowed to touch that code.
func baseBug(c *Ctx, failed string) {
	if c.Level == levelNone {
		return
	}
	if settings.Management.Mode == "service" { // service mode creates tracker issues only through its journal
		logEvent(map[string]any{"event": "base-bug-skipped", "why": "management service mode files issues itself"})
		return
	}
	first, _, _ := strings.Cut(failed, "\n")
	title := "Base " + c.Base + " " + first // e.g. "Base origin/main check failed: cargo clippy ..."
	url, ok := gh(c, "issue", "list", "--state", "open", "--search", strconv.Quote(title)+" in:title", "--json", "url,title",
		"--jq", "map(select(.title == "+strconv.Quote(title)+"))[0].url // empty")
	if !ok {
		return
	}
	if url != "" {
		gh(c, "issue", "comment", url, "--body", fmt.Sprintf("Also blocks #%d.", c.Issue))
	} else {
		body := fmt.Sprintf("The checks fail on `%s` before any change, so agent-sm stops every issue run on this base.\n\nBlocks: #%d\n\n```\n%s\n```\n\n%s\n",
			c.Base, c.Issue, tail(failed, 6000), fixesBaseMarker)
		if url, ok = gh(c, "issue", "create", "--title", title, "--body", body, "--label", "bug"); !ok { // the repo may have no bug label
			url, ok = gh(c, "issue", "create", "--title", title, "--body", body)
		}
		if !ok {
			return
		}
	}
	logEvent(map[string]any{"event": "base-bug", "url": url})
	gh(c, "issue", "comment", strconv.Itoa(c.Issue), "--body", "Blocked: the base fails its checks before any change. Tracked in "+url+".")
}

// stopGhidra ends any bridge on the run's project copy (the old end-of-epic check).
func stopGhidra(c *Ctx) {
	if settings.Legacy && c.Ghidra != "" && c.Ghidra != "none" {
		sh(c.Repo, "ghidra", "--project", c.Ghidra, "--program", "arm9.bin", "stop")
	}
}
