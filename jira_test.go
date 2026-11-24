package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssueMetadata(t *testing.T) {
	body := "## Jira\n- **Jira issue:** [PUSH-42](https://example.atlassian.net/browse/PUSH-42)\n- **Jira Epic:** https://example.atlassian.net/browse/PUSH-1\n- **Base branch:** `feature/integration`\n"
	got, err := issueMetadata(body)
	if err != nil || got.JiraKey != "PUSH-42" || got.Base != "origin/feature/integration" {
		t.Fatalf("%+v %v", got, err)
	}
	for _, line := range []string{
		"- **Base branch**: `feature/integration` (parent Story #12).",
		"- **Base branch**: `origin/feature/integration` (parent Story #12)",
	} {
		got, err := issueMetadata(line)
		if err != nil || got.Base != "origin/feature/integration" {
			t.Errorf("%q: %+v %v", line, got, err)
		}
	}
	for _, invalid := range []string{
		body + "- Jira issue: https://example.atlassian.net/browse/PUSH-43",
		body + "- Base branch: main",
		"Base branch: --upload-pack=bad",
		"Base branch: feature/../main",
		"Base branch: `--upload-pack=bad` (parent Story #12).",
		"Base branch: `main` (or `release`)",
		"Base branch: `main` (parent Story #12).\nBase branch: main",
		"## Jira\nJira issue: not-a-url",
		"Base branch: origin/--upload-pack",
		"Base branch: main/.hidden",
		"## Jira\nJira issue: https://example.atlassian.net/browse/PUSH-42suffix",
		"## Jira\nJira issue: https://example.atlassian.net/browse/PUSH-42 https://other.example/browse/PUSH-43",
	} {
		if _, err := issueMetadata(invalid); err == nil {
			t.Errorf("accepted ambiguous/invalid metadata %s", invalid)
		}
	}
	got, err = issueMetadata("## Jira\nJira Epic: https://example.atlassian.net/browse/PUSH-1")
	if err != nil || got.JiraKey != "" {
		t.Fatal("Epic became this issue's counterpart")
	}
}

func TestJiraLifecycle(t *testing.T) {
	j := JiraConfig{InProgress: "In Progress", InReview: "In Code Review", Delivered: "Ready for Deploy"}
	issueURL := "https://github.com/example/repo/issues/1"
	candidate := linkedPR{Title: "PUSH-42 implement task", State: "OPEN", BaseRefName: "main", URL: "https://github.com/example/repo/pull/2"}
	candidate.ClosingIssuesReferences = append(candidate.ClosingIssuesReferences, struct {
		URL    string
		Number int
	}{issueURL, 1})
	for _, tc := range []struct {
		name     string
		pr       linkedPR
		target   string
		conflict bool
	}{
		{"review", candidate, j.InReview, false},
		{"draft", func() linkedPR { p := candidate; p.IsDraft = true; return p }(), j.InProgress, false},
		{"partial", func() linkedPR { p := candidate; p.ClosingIssuesReferences = nil; return p }(), j.InProgress, false},
		{"merged", func() linkedPR { p := candidate; p.State = "MERGED"; return p }(), j.Delivered, false},
		{"closed", func() linkedPR { p := candidate; p.State = "CLOSED"; return p }(), j.InProgress, false},
		{"wrong base", func() linkedPR { p := candidate; p.BaseRefName = "release"; return p }(), "", true},
		{"wrong key", func() linkedPR { p := candidate; p.Title = "PUSH-420 task"; return p }(), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := jiraPhase(issueURL, "PUSH-42", "origin/main", true, []linkedPR{tc.pr}, j)
			if (err != nil) != tc.conflict || got != tc.target {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
	parent := "https://github.com/example/repo/issues/9"
	if got, err := jiraPhase(parent, "PUSH-99", "origin/main", false, []linkedPR{candidate}, j); err != nil || got != "" {
		t.Fatal("child completion advanced its parent")
	}
}

func TestJiraBacklink(t *testing.T) {
	link := "https://github.com/example/repo/issues/1"
	for _, raw := range []string{`{"content":[{"attrs":{"href":"` + link + `"}}]}`, `"Related: ` + link + `."`} {
		if !hasBacklink(json.RawMessage(raw), link) {
			t.Errorf("missed backlink %s", raw)
		}
	}
	for _, raw := range []string{`"` + link + `0"`, `"` + link + `/comments"`, `"` + link + `?wrong=true"`, `"different issue"`} {
		if hasBacklink(json.RawMessage(raw), link) {
			t.Errorf("accepted wrong backlink %s", raw)
		}
	}
}

func TestJiraReconciliationReadOnly(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("reconciliation attempted a write")
		}
		user, token, ok := r.BasicAuth()
		if !ok || user != "test@example.invalid" || token != "test-token" {
			t.Error("missing configured authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"PUSH-42","fields":{"status":{"name":"In Progress"},"description":{"content":[{"attrs":{"href":"https://github.com/example/repo/issues/1"}}]}}}`))
	}))
	defer srv.Close()
	// Use a server with a locally trusted test certificate without changing production transport.
	oldTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
case "$1 $2" in
  "issue view") printf '%s\n' '{"url":"https://github.com/example/repo/issues/1","body":"## Jira\nJira issue: https://example.atlassian.net/browse/PUSH-42\nBase branch: main","state":"OPEN","assignees":[{"login":"example"}]}';;
  "pr list") echo '[]';;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JIRA_USER_EMAIL", "test@example.invalid")
	t.Setenv("JIRA_API_TOKEN", "test-token")
	p := defaultProfile("pi")
	p.Jira = JiraConfig{Mode: "actions", Site: "https://example.atlassian.net", APIBase: srv.URL, InProgress: "In Progress", InReview: "In Code Review", Delivered: "Ready for Deploy"}
	useProfile(t, p)
	if err := p.Jira.validate(); err != nil {
		t.Fatal(err)
	}
	got := reconcileJira(&Ctx{Repo: dir, Issue: 1})
	if got.Status != "already aligned" || got.Actual != "In Progress" {
		t.Fatalf("%+v", got)
	}
	t.Setenv("JIRA_API_TOKEN", "")
	got = reconcileJira(&Ctx{Repo: dir, Issue: 1})
	if got.Status != "skipped-unavailable" {
		t.Fatalf("unavailable integration claimed success: %+v", got)
	}
	settings.Jira.Site = "https://different.atlassian.net"
	got = reconcileJira(&Ctx{Repo: dir, Issue: 1})
	if got.Status != "conflict" || !strings.Contains(got.Reason, "site") {
		t.Fatalf("%+v", got)
	}
}
