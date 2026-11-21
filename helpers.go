package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	sepRe         = regexp.MustCompile(`&&|\|\||;|\||\n`)
	wordRe        = regexp.MustCompile(`^[a-z./~][\w./~+-]*( [\w-]+)?$`) // a command starts lowercase, so hex addresses and prose do not count
	heredocRe     = regexp.MustCompile(`<<-?\s*['"]?(\w+)['"]?`)
	quotedRe      = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	shKeyword     = map[string]bool{"for": true, "do": true, "done": true, "if": true, "then": true, "fi": true, "else": true, "while": true, "case": true, "esac": true}
	twoWord       = map[string]bool{"git": true, "gh": true, "cargo": true, "rtk": true, "ghidra": true, "dotnet": true, "go": true}
	issueBranchRe = regexp.MustCompile(`^sm/issue-([1-9][0-9]*)(?:-[1-9][0-9]*)?$`)
)

// handledElsewhere says why this run has nothing left to do: the issue is closed, or another
// attempt's PR for it is open. "" means go on. A gh error fails open: the guard only saves work.
// A merged PR alone does not stop the run: a partial fix ("Refs #N") leaves the issue open on purpose.
func handledElsewhere(c *Ctx) string {
	n := strconv.Itoa(c.Issue)
	if out, err := sh(c.Repo, "gh", "issue", "view", n, "--json", "state", "--jq", ".state"); err == nil && strings.TrimSpace(out) == "CLOSED" {
		return "issue #" + n + " is closed"
	}
	// shortcut: scans the 100 newest open PRs only; fine while attempts of one issue are recent.
	out, err := sh(c.Repo, "gh", "pr", "list", "--state", "open", "--limit", "100", "--json", "headRefName,url")
	var prs []struct{ HeadRefName, URL string }
	if err != nil || json.Unmarshal([]byte(out), &prs) != nil {
		logEvent(map[string]any{"event": "handled-check", "err": "gh pr list failed", "out": tail(out, 300)})
		return ""
	}
	for _, p := range prs {
		if m := issueBranchRe.FindStringSubmatch(p.HeadRefName); m != nil && m[1] == n && p.HeadRefName != c.Branch {
			return "another attempt's PR is open: " + p.URL
		}
	}
	return ""
}

func issueBranchName(issue, attempt int) string {
	name := "sm/issue-" + strconv.Itoa(issue)
	if attempt > 0 {
		name += "-" + strconv.Itoa(attempt)
	}
	return name
}

// An empty worktreeRoot selects only a branch, for direct CLI runs.
func nextIssueBranch(repo string, issue int, worktreeRoot string) (string, string, error) {
	registered := map[string]bool{}
	if worktreeRoot != "" {
		out, err := sh(repo, "git", "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return "", "", fmt.Errorf("list worktrees: %w: %s", err, tail(out, 300))
		}
		for _, field := range strings.Split(out, "\x00") {
			if path, ok := strings.CutPrefix(field, "worktree "); ok {
				registered[filepath.Clean(path)] = true
			}
		}
	}
	for attempt := 0; ; attempt++ {
		branch := issueBranchName(issue, attempt)
		dir := ""
		if worktreeRoot != "" {
			suffix := strings.TrimPrefix(branch, issueBranchName(issue, 0))
			dir = filepath.Join(worktreeRoot, strconv.Itoa(issue)+"-sm"+suffix)
			if registered[dir] {
				continue
			}
			if _, err := os.Lstat(dir); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return "", "", fmt.Errorf("check worktree %s: %w", dir, err)
			}
		}
		out, err := sh(repo, "git", "for-each-ref", "--format=%(refname)", "refs/heads/"+branch)
		if err != nil {
			return "", "", fmt.Errorf("check local branch %s: %w: %s", branch, err, tail(out, 300))
		}
		localTaken := false
		for _, ref := range strings.Fields(out) {
			if ref == "refs/heads/"+branch || strings.HasPrefix(ref, "refs/heads/"+branch+"/") {
				localTaken = true
				break
			}
		}
		if localTaken {
			continue
		}
		out, err = sh(repo, "git", "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch, "refs/heads/"+branch+"/*")
		if err == nil {
			continue
		}
		if status, ok := err.(*exec.ExitError); !ok || status.ExitCode() != 2 {
			return "", "", fmt.Errorf("check remote branch %s: %w: %s", branch, err, tail(out, 300))
		}
		out, err = sh(repo, "gh", "pr", "list", "--head", branch, "--state", "all", "--limit", "1", "--json", "number")
		if err != nil {
			return "", "", fmt.Errorf("check PRs for %s: %w: %s", branch, err, tail(out, 300))
		}
		var prs []struct {
			Number int `json:"number"`
		}
		if err := json.Unmarshal([]byte(out), &prs); err != nil || prs == nil {
			return "", "", fmt.Errorf("invalid PR lookup response for %s", branch)
		}
		for _, pr := range prs {
			if pr.Number <= 0 {
				return "", "", fmt.Errorf("invalid PR identity for %s", branch)
			}
		}
		if len(prs) == 0 {
			return branch, dir, nil
		}
	}
}

func checkRunBranch(c *Ctx) error {
	match := issueBranchRe.FindStringSubmatch(c.Branch)
	if match == nil || match[1] != strconv.Itoa(c.Issue) {
		return fmt.Errorf("invalid run branch %q for issue %d", c.Branch, c.Issue)
	}
	out, err := sh(c.Repo, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("read run branch: %w: %s", err, tail(out, 300))
	}
	if strings.TrimSpace(out) != c.Branch {
		return fmt.Errorf("run branch changed: expected %s, got %s", c.Branch, strings.TrimSpace(out))
	}
	return nil
}

// stripHeredocs drops each heredoc body: the lines after a "<<WORD" marker up to the line holding WORD.
func stripHeredocs(cmd string) string {
	for {
		m := heredocRe.FindStringSubmatchIndex(cmd)
		if m == nil {
			return cmd
		}
		word := cmd[m[2]:m[3]]
		nl := strings.Index(cmd[m[1]:], "\n")
		if nl < 0 { // marker on the last line: no body
			return cmd[:m[0]] + cmd[m[1]:]
		}
		bodyStart := m[1] + nl + 1
		end := len(cmd)
		pos := bodyStart
		for _, line := range strings.SplitAfter(cmd[bodyStart:], "\n") {
			pos += len(line)
			if strings.TrimSpace(line) == word {
				end = pos
				break
			}
		}
		cmd = cmd[:m[0]] + cmd[m[1]:bodyStart] + cmd[end:]
	}
}

// cmdWords reduces a shell line to the command words it runs: "git log" for git/gh/cargo/rtk/ghidra
// (their subcommand matters for allow lists), the first word for anything else.
func cmdWords(cmd string) []string {
	var out []string
	cmd = quotedRe.ReplaceAllString(stripHeredocs(cmd), `""`) // heredoc bodies and quoted text are arguments, not commands
	for _, seg := range sepRe.Split(cmd, -1) {
		f := strings.Fields(seg)
		for len(f) > 0 && strings.Contains(f[0], "=") && !strings.HasPrefix(f[0], "-") {
			f = f[1:] // VAR=value prefix
		}
		for len(f) > 0 && (f[0] == "do" || f[0] == "then" || f[0] == "else") {
			f = f[1:] // keyword that precedes a command
		}
		if len(f) == 0 || shKeyword[f[0]] {
			continue
		}
		w := f[0]
		if twoWord[w] && len(f) > 1 && !strings.HasPrefix(f[1], "-") {
			w += " " + f[1]
		}
		if wordRe.MatchString(w) {
			out = append(out, w)
		}
	}
	return out
}

// Commands an audited agent may never run: they change git history, GitHub, or delete trees.
// Matching is by command prefix, so this is a best-effort guard, not a sandbox: run audits in a
// throwaway clone with no push access.
var outward = []string{"git push", "git commit", "git checkout", "git switch", "git reset", "git rebase",
	"git branch", "git merge", "git stash", "gh pr", "gh issue edit", "gh issue create", "gh issue comment",
	"gh issue close", "gh issue reopen", "gh api", "rm -rf", "rm -r", "sudo"}

var auditDeny = func() string {
	var l []string
	for _, o := range outward {
		l = append(l, "Bash("+o+":*)", "Bash(rtk "+o+":*)", "Bash(rtk proxy "+o+":*)")
	}
	return strings.Join(l, ",")
}()

// neverSuggest: a command key that must not be proposed for an allow list.
func neverSuggest(key string) bool {
	k := strings.TrimPrefix(strings.TrimPrefix(key, "rtk "), "proxy ")
	if k == "rm" || k == "sudo" || k == "rtk" {
		return true
	}
	for _, o := range outward {
		if k == o || strings.HasPrefix(k, o+" ") {
			return true
		}
	}
	return false
}

// withRTK adds, for every Bash(cmd:*) entry, the forms the RTK rule makes agents type:
// "rtk cmd" and "rtk proxy cmd". Never a bare Bash(rtk:*): rtk can run any command.
func withRTK(allow string) string {
	out := allow
	for _, a := range strings.Split(allow, ",") {
		if cmd, ok := strings.CutPrefix(strings.TrimSpace(a), "Bash("); ok && !strings.HasPrefix(cmd, "rtk ") {
			out += ",Bash(rtk " + cmd + ",Bash(rtk proxy " + cmd
		}
	}
	return out
}

// baseBranch is the branch runs start from and publish rebases onto (SM_BASE, default origin/main).
func baseBranch() string {
	return settings.Base
}

// suggestAllow builds an allow list from what a state really used, plus a small floor.
func suggestAllow(state string, called, bash map[string]int) string {
	set := map[string]bool{"Read": true, "Grep": true, "Glob": true}
	if state == "execute" {
		set["Edit"], set["Write"] = true, true
	}
	for t := range called {
		switch t {
		case "StructuredOutput", "Bash":
		case "Edit", "Write", "NotebookEdit":
			if state == "execute" {
				set[t] = true
			}
		default:
			set[t] = true
		}
	}
	for k := range bash {
		if !neverSuggest(k) {
			set["Bash("+k+":*)"] = true
		}
	}
	var l []string
	for t := range set {
		l = append(l, t)
	}
	sort.Strings(l)
	return strings.Join(l, ",")
}

var (
	depHeadRe = regexp.MustCompile(`(?im)^#{1,6}\s*(depends on|dependencies)\b.*$`)
	depListRe = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
	depLineRe = regexp.MustCompile(`^(?:depends on|blocked by)\b`)
	headRe    = regexp.MustCompile(`(?m)^#{1,6}\s`)
	issueRe   = regexp.MustCompile(`#(\d+)`)
)

// dependencies reads list entries under "Dependencies" and explicit "depends on"/"blocked by"
// lines under either dependency heading. Parent, Epic, Related and contextual prose are ignored.
func dependencies(body string) []int {
	loc := depHeadRe.FindStringIndex(body)
	if loc == nil {
		return nil
	}
	head := strings.ToLower(body[loc[0]:loc[1]])
	rest := body[loc[1]:]
	if m := headRe.FindStringIndex(rest); m != nil {
		rest = rest[:m[0]]
	}
	var deps []int
	for _, line := range strings.Split(rest, "\n") {
		l := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(line, "**", "")))
		if strings.Contains(l, "parent") || strings.Contains(l, "epic") || strings.Contains(l, "related") {
			continue
		}
		list := depListRe.FindStringIndex(l)
		if list != nil {
			l = l[list[1]:]
		}
		onlyDeps := strings.Contains(head, "dependencies")
		if !(onlyDeps && list != nil) && !depLineRe.MatchString(l) {
			continue
		}
		for _, m := range issueRe.FindAllStringSubmatch(line, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				deps = append(deps, n)
			}
		}
	}
	return deps
}

// cleanupWorktree removes the run's worktree and its merged local branch after a merge.
// The run lives inside the worktree, so the main checkout is found through the common git dir.
func cleanupWorktree(c *Ctx) {
	if !c.Merged {
		return
	}
	if err := checkRunBranch(c); err != nil {
		logEvent(map[string]any{"event": "cleanup-error", "err": err.Error()})
		return
	}
	common, err := sh(c.Repo, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo, _ := filepath.Abs(c.Repo)
	main := filepath.Dir(strings.TrimSpace(common))
	if err != nil || main == repo {
		logEvent(map[string]any{"event": "cleanup-skipped", "why": "not a linked worktree"})
		return
	}
	if out, err := sh(main, "git", "worktree", "remove", repo); err != nil {
		logEvent(map[string]any{"event": "cleanup-error", "out": tail(out, 300)})
		return
	}
	if out, err := sh(main, "git", "branch", "-D", "--", c.Branch); err != nil {
		logEvent(map[string]any{"event": "cleanup-error", "out": tail(out, 300)})
		return
	}
	logEvent(map[string]any{"event": "worktree-removed", "path": repo})
}
