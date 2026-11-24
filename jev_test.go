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

// fakeJev answers every choice question with choice/confidence and records the last request.
func fakeJev(t *testing.T, status int, choice string, confidence float64) *map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"duplicate": map[string]any{
			"type": "choice", "choice": choice, "confidence": confidence}}})
	}))
	t.Cleanup(srv.Close)
	old := jevURL
	jevURL = srv.URL
	t.Cleanup(func() { jevURL = old })
	oldEvents := eventsPath
	eventsPath = filepath.Join(t.TempDir(), "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })
	return &got
}

func TestPickDuplicate(t *testing.T) {
	open := []openIssue{
		{Number: 7, Title: "Add retries", Body: "Retry the upload step", URL: "https://github.com/o/r/issues/7"},
		{Number: 9, Title: "Add retries", Body: "Retry the download step", URL: "https://github.com/o/r/issues/9"},
	}
	for _, tc := range []struct {
		name       string
		status     int
		choice     string
		confidence float64
		want       string
	}{
		{"confident duplicate", 200, "9", 0.97, "https://github.com/o/r/issues/9"},
		{"unsure files a new issue", 200, "9", 0.6, ""},
		{"new work", 200, "new", 0.99, ""},
		{"unknown option", 200, "42", 0.99, ""},
		{"endpoint refuses (e.g. free model gone)", 403, "9", 0.99, ""},
	} {
		got := fakeJev(t, tc.status, tc.choice, tc.confidence)
		if url := pickDuplicate("k", "follow-up", "Add retries", "Retry the download step", open); url != tc.want {
			t.Fatalf("%s: url = %q, want %q", tc.name, url, tc.want)
		}
		q := (*got)["questions"].(map[string]any)["duplicate"].(map[string]any)
		criteria := q["criteria"].(map[string]any)
		if (*got)["model"] != jevModel || q["type"] != "choice" || len(criteria) != 3 ||
			!strings.Contains(criteria["9"].(string), "download") || criteria["new"] == nil {
			t.Fatalf("%s: request = %v", tc.name, *got)
		}
	}
	if url := pickDuplicate("k", "bug", "t", "b", nil); url != "" {
		t.Fatalf("no open issues: url = %q", url)
	}
}

func TestDuplicateIssueNeedsAKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	old := jevURL
	jevURL = srv.URL
	defer func() { jevURL = old }()
	if url := duplicateIssue(os.TempDir(), "bug", "t", "b"); url != "" || called {
		t.Fatalf("without a key: url = %q, Jev called = %v", url, called)
	}
}

func TestFollowUpCommentsOnADuplicate(t *testing.T) {
	useProfile(t, defaultProfile("pi"))
	dir := t.TempDir()
	log := filepath.Join(dir, "gh.log")
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
printf '%s|' "$@" >> "`+log+`"; echo >> "`+log+`"
case "$1 $2" in
  "issue list") echo '[{"number":77,"title":"t","body":"old","url":"https://github.com/example/repo/issues/77"}]';;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SM_EPIC", "")
	t.Setenv("OPENCODE_API_KEY", "k")
	fakeJev(t, 200, "77", 0.95)
	c := &Ctx{Repo: dir, Scratch: dir, Issue: 5, Title: "t", Level: levelPR, Partial: true, PR: "42", RunBudget: 1, CostUnknown: 1}
	followUp(c, Ready)
	b, _ := os.ReadFile(log)
	got := string(b)
	if strings.Contains(got, "issue|create") || !strings.Contains(got, "issue|comment|77|") ||
		!strings.Contains(got, "pr|comment|42|--body|This PR delivers part of #5. The remaining work is tracked in #77.") {
		t.Fatalf("a duplicate follow-up must comment on #77 and file nothing:\n%s", got)
	}
}
