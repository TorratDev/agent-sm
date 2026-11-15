package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// A "Refs" PR must never put close/fix/resolve next to an issue number, even negated:
// GitHub would close the issue on merge (prompts/original/executor.md, step 6).
var closeWord = regexp.MustCompile(`(?i)\b(close[sd]?|fix(es|ed)?|resolve[sd]?)\b[^\n]{0,40}#\d+|#\d+[^\n]{0,40}\b(close[sd]?|fix(es|ed)?|resolve[sd]?)\b`)

const footer = "🤖 Generated with [Claude Code](https://claude.com/claude-code)"

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
	n := strconv.Itoa(c.Issue)
	branch := "sm/issue-" + n
	if _, clean := sh(c.Repo, "git", "diff", "--cached", "--quiet"); clean == nil {
		logEvent(map[string]any{"event": "publish-error", "err": "nothing staged"})
		return Fail
	}
	model := c.Model
	if model == "" {
		model = "claude"
	}
	msg := fmt.Sprintf("%s (#%d)\n\nCo-Authored-By: Claude %s <noreply@anthropic.com>", c.Title, c.Issue, model)
	if _, ok := git(c, "commit", "-q", "-m", msg); !ok {
		return Fail
	}

	// Rebase onto the latest base branch and rerun the checks before pushing.
	base := baseBranch()
	if _, ok := git(c, "fetch", "origin", strings.TrimPrefix(base, "origin/")); !ok {
		return Fail
	}
	before, _ := git(c, "rev-parse", "HEAD")
	if _, ok := git(c, "rebase", base); !ok {
		git(c, "rebase", "--abort")
		return Fail
	}
	if after, _ := git(c, "rev-parse", "HEAD"); after != before && !runChecks(c) {
		logEvent(map[string]any{"event": "publish-error", "err": "checks failed after rebase", "feedback": tail(c.Feedback, 500)})
		return Fail
	}
	if _, ok := git(c, "push", "-u", "origin", branch); !ok {
		return Fail
	}

	link, body := "Closes", c.Summary
	if !c.ClosesIssue {
		// Refs PR: fixed text only, so no free text can carry a closing word.
		link, body = "Refs", "Covers only some points of the issue; the open items stay in the issue."
	}
	prBody := fmt.Sprintf("%s #%d\n\n%s\n\n%s", link, c.Issue, body, footer)
	if link == "Refs" && (closeWord.MatchString(c.Title) || closeWord.MatchString(prBody)) {
		logEvent(map[string]any{"event": "publish-error", "err": "closing word next to an issue number in a Refs PR", "title": c.Title})
		return Fail
	}
	url, ok := gh(c, "pr", "create", "--draft", "--head", branch, "--title", c.Title, "--body", prBody)
	if !ok {
		return Fail
	}
	pr := url[strings.LastIndex(url, "/")+1:]
	logEvent(map[string]any{"event": "pr-opened", "url": url})

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
	if !c.ClosesIssue { // a Refs merge must leave the issue open
		if st, _ := gh(c, "issue", "view", n, "--json", "state", "--jq", ".state"); st == "CLOSED" {
			gh(c, "issue", "reopen", n)
		}
	}
	c.Merged = true
	logEvent(map[string]any{"event": "merged", "pr": pr, "note": "the worktree is left in place: remove it with git worktree remove"})
	return Ok
}

// followUp files the follow-up issue the orchestrator created after 3 REVISE rounds or a split.
// It needs SM_EPIC (the epic the follow-up joins) and a publish level.
func followUp(c *Ctx, exit Exit) {
	if c.Level == levelNone || !(exit == SplitExit || (exit == GaveUp && c.Rounds >= maxExecute)) {
		return
	}
	epic := os.Getenv("SM_EPIC")
	if epic == "" {
		logEvent(map[string]any{"event": "follow-up-skipped", "why": "set SM_EPIC to create it"})
		return
	}
	reason := "work does not fit one PR"
	if exit == GaveUp {
		reason = fmt.Sprintf("%d REVISE rounds without a PASS", maxExecute)
	}
	body := fmt.Sprintf("Done so far: %s\n\nRemaining work: %s\n\nReason for split: %s\n\nParent: #%d\nEpic: #%s\nDependencies: #%d",
		c.Summary, c.Feedback+c.Evidence, reason, c.Issue, epic, c.Issue)
	if url, ok := gh(c, "issue", "create", "--title", c.Title+" (follow-up)", "--body", body, "--label", "follow-up"); ok {
		logEvent(map[string]any{"event": "follow-up", "url": url})
	}
}

// stopGhidra ends any bridge on the run's project copy (the old end-of-epic check).
func stopGhidra(c *Ctx) {
	if c.Ghidra != "none" {
		sh(c.Repo, "ghidra", "--project", c.Ghidra, "--program", "arm9.bin", "stop")
	}
}
