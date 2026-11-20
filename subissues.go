package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
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
func expand(repo string, n int) ([]int, []string) {
	out, err := sh(repo, "gh", "api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/sub_issues?per_page=100", n), "--paginate", "--jq",
		`[.[] | {number, state, body: (.body // ""), blocked: (.issue_dependencies_summary.blocked_by // 0), subs: (.sub_issues_summary.total // 0)}]`)
	if err != nil {
		detail, _, _ := strings.Cut(strings.TrimSpace(out), "\n\n")
		if detail == "" {
			detail = err.Error()
		}
		return nil, []string{fmt.Sprintf("#%d not queued: cannot read its sub-issues: %s", n, tail(detail, 200))}
	}
	subs, err := decodeGitHubPages[subIssue](out)
	if err != nil {
		return nil, []string{fmt.Sprintf("#%d not queued: cannot read its sub-issues: %v", n, err)}
	}
	if settings.Management.Mode == "service" {
		if len(subs) == 0 {
			if err := managedDependencies(runCtx, repo, n); err != nil {
				return nil, []string{fmt.Sprintf("#%d not queued: %v", n, err)}
			}
		}
		var filtered []subIssue
		var notes []string
		for _, s := range subs {
			if s.State != "open" {
				continue
			}
			if err := managedDependencies(runCtx, repo, s.Number); err != nil {
				notes = append(notes, fmt.Sprintf("#%d not queued: %v", s.Number, err))
			} else {
				filtered = append(filtered, s)
			}
		}
		if len(subs) > 0 && len(filtered) == 0 {
			return nil, append(notes, fmt.Sprintf("#%d: no sub-issue can run now", n))
		}
		subs = filtered
		run, why := pickSubIssues(n, subs, func(d int) bool {
			st, err := sh(repo, "gh", "issue", "view", strconv.Itoa(d), "--json", "state", "--jq", ".state")
			return err != nil || strings.TrimSpace(st) != "CLOSED"
		})
		return run, append(notes, why...)
	}
	// All sub-issues closed: the work is done, so the issue is closed too.
	// shortcut: not in service mode, where the manager owns tracker writes; add a managed operation there if it is needed.
	if len(subs) > 0 && !slices.ContainsFunc(subs, func(s subIssue) bool { return s.State == "open" }) {
		if out, err := sh(repo, "gh", "issue", "close", strconv.Itoa(n), "--reason", "completed", "--comment", "Closed by agent-sm: all sub-issues are closed."); err != nil {
			return nil, []string{fmt.Sprintf("#%d: all its sub-issues are closed, but closing it failed: %s", n, tail(strings.TrimSpace(out), 200))}
		}
		return nil, []string{fmt.Sprintf("#%d closed: all its sub-issues are closed", n)}
	}
	return pickSubIssues(n, subs, func(d int) bool {
		st, err := sh(repo, "gh", "issue", "view", strconv.Itoa(d), "--json", "state", "--jq", ".state")
		return err != nil || strings.TrimSpace(st) != "CLOSED"
	})
}

func decodeGitHubPages[T any](out string) ([]T, error) {
	var values []T
	decoder := json.NewDecoder(strings.NewReader(out))
	for pageNumber := 0; ; pageNumber++ {
		var page []T
		err := decoder.Decode(&page)
		if err == io.EOF && pageNumber > 0 {
			break
		}
		if err != nil || page == nil {
			if err == nil {
				err = fmt.Errorf("expected a JSON array")
			}
			return nil, err
		}
		values = append(values, page...)
	}
	return values, nil
}

func githubDependencies(repo string, n int) error {
	out, err := sh(repo, "gh", "api", fmt.Sprintf("repos/{owner}/{repo}/issues/%d/dependencies/blocked_by?per_page=100", n), "--paginate")
	if err != nil {
		return fmt.Errorf("cannot read GitHub blockers: %w: %s", err, tail(out, 300))
	}
	blockers, err := decodeGitHubPages[subIssue](out)
	if err != nil {
		return fmt.Errorf("invalid GitHub blockers: %w", err)
	}
	for _, blocker := range blockers {
		if blocker.Number <= 0 || (blocker.State != "open" && blocker.State != "closed") {
			return fmt.Errorf("invalid GitHub blocker identity or state")
		}
		if blocker.State == "open" {
			return fmt.Errorf("blocked by open GitHub dependency #%d", blocker.Number)
		}
	}
	return nil
}

func managedDependencies(ctx context.Context, repo string, n int) error {
	m, err := newManager(ctx, repo, settings)
	if err != nil {
		return err
	}
	blockers, err := ghPages[issueSnapshot](ctx, m, m.endpoint(n, "/dependencies/blocked_by"))
	if err != nil {
		return fmt.Errorf("dependency state unavailable: %w", err)
	}
	for _, blocker := range blockers {
		if blocker.State != "closed" {
			return fmt.Errorf("blocked by open or unknown dependency %s", blocker.URL)
		}
	}
	return nil
}
