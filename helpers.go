package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	sepRe     = regexp.MustCompile(`&&|\|\||;|\||\n`)
	wordRe    = regexp.MustCompile(`^[a-z./~][\w./~+-]*( [\w-]+)?$`) // a command starts lowercase, so hex addresses and prose do not count
	heredocRe = regexp.MustCompile(`<<-?\s*['"]?(\w+)['"]?`)
	quotedRe  = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	shKeyword = map[string]bool{"for": true, "do": true, "done": true, "if": true, "then": true, "fi": true, "else": true, "while": true, "case": true, "esac": true}
	twoWord   = map[string]bool{"git": true, "gh": true, "cargo": true, "rtk": true, "ghidra": true}
)

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
		l = append(l, "Bash("+o+":*)", "Bash(rtk "+o+":*)") // the RTK hook may prefix commands
	}
	return strings.Join(l, ",")
}()

// neverSuggest: a command key that must not be proposed for an allow list.
func neverSuggest(key string) bool {
	k := strings.TrimPrefix(key, "rtk ")
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

// allowFor returns the per-state allow list from SM_ALLOW_FILE (JSON {"state": "tools"}) if it
// names this state, else def.
func allowFor(s State, def string) string {
	b, err := os.ReadFile(os.Getenv("SM_ALLOW_FILE"))
	if err != nil {
		return def
	}
	var m map[string]string
	if json.Unmarshal(b, &m) == nil && m[s.String()] != "" {
		return m[s.String()]
	}
	return def
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
	if b := os.Getenv("SM_BASE"); b != "" {
		return b
	}
	return "origin/main"
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
	headRe    = regexp.MustCompile(`(?m)^#{1,6}\s`)
	issueRe   = regexp.MustCompile(`#(\d+)`)
)

// dependencies lists the issues a body says it depends on: every #N of a "Dependencies" section
// (except Parent, Epic and Related lines), or the #N on "depends on"/"blocked by" lines of a
// "Depends on ..." section. A "Parent:" line is never a dependency.
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
		l := strings.ToLower(line)
		if strings.Contains(l, "parent") || strings.Contains(l, "epic") || strings.Contains(l, "related") {
			continue
		}
		onlyDeps := strings.Contains(head, "dependencies")
		if !onlyDeps && !strings.Contains(l, "depends on") && !strings.Contains(l, "blocked by") {
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
	sh(main, "git", "branch", "-D", "sm/issue-"+strconv.Itoa(c.Issue)) // already merged
	logEvent(map[string]any{"event": "worktree-removed", "path": repo})
}
