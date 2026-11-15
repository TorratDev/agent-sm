package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// subIssue is what the queue needs to know about one GitHub sub-issue.
type subIssue struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Body    string `json:"body"`
	Blocked int    `json:"blocked"` // open issues GitHub lists as blocking this one
	Subs    int    `json:"subs"`    // its own sub-issues
}

// pickSubIssues turns one selected issue into the issues to run. An issue without sub-issues
// runs itself. An issue with sub-issues is never run: its open sub-issues run in its place,
// except those that depend on an open issue (GitHub's "blocked by", or the body's Dependencies
// section, as prepare reads it). notes says what was left out and why.
func pickSubIssues(parent int, subs []subIssue, isOpen func(int) bool) (run []int, notes []string) {
	if len(subs) == 0 {
		return []int{parent}, nil
	}
	for _, s := range subs {
		why := ""
		switch {
		case s.State != "open":
			continue
		case s.Subs > 0:
			// shortcut: one level only; select a nested parent on its own to run its sub-issues.
			why = "has its own sub-issues"
		case s.Blocked > 0:
			why = fmt.Sprintf("blocked by %d open issue(s)", s.Blocked)
		default:
			for _, d := range dependencies(s.Body) {
				if isOpen(d) {
					why = fmt.Sprintf("depends on #%d, which is open", d)
					break
				}
			}
		}
		if why != "" {
			notes = append(notes, fmt.Sprintf("#%d skipped: %s", s.Number, why))
			continue
		}
		run = append(run, s.Number)
	}
	if len(run) == 0 {
		notes = append(notes, fmt.Sprintf("#%d: no sub-issue can run now", parent))
	}
	return run, notes
}

// expand asks GitHub for an issue's sub-issues and applies pickSubIssues.
// shortcut: the first 100 sub-issues only; page the request if an epic ever has more.
func expand(repo string, n int) ([]int, []string) {
	out, err := sh(repo, "gh", "api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/sub_issues?per_page=100", n), "--jq",
		`[.[] | {number, state, body: (.body // ""), blocked: (.issue_dependencies_summary.blocked_by // 0), subs: (.sub_issues_summary.total // 0)}]`)
	var subs []subIssue
	if err != nil || json.Unmarshal([]byte(out), &subs) != nil {
		return nil, []string{fmt.Sprintf("#%d not queued: cannot read its sub-issues: %s", n, tail(strings.TrimSpace(out), 200))}
	}
	return pickSubIssues(n, subs, func(d int) bool {
		st, _ := sh(repo, "gh", "issue", "view", strconv.Itoa(d), "--json", "state", "--jq", ".state")
		return strings.TrimSpace(st) == "OPEN"
	})
}
