package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A rebase conflict goes to the resolve agent; its merge is staged and the rebase finishes.
func TestResolveConflictsFinishesRebase(t *testing.T) {
	useProfile(t, defaultProfile("claude"))
	repo, bin := t.TempDir(), t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	doc := filepath.Join(repo, "doc.md")
	run("init", "-q", "-b", "main")
	writeFile(t, doc, "base\n", 0o644)
	run("add", ".")
	run("commit", "-qm", "base")
	run("checkout", "-qb", "work")
	writeFile(t, doc, "work\n", 0o644)
	run("commit", "-qam", "work")
	run("checkout", "-q", "main")
	writeFile(t, doc, "main\n", 0o644)
	run("commit", "-qam", "main")
	run("checkout", "-q", "work")
	cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@t", "rebase", "main")
	cmd.Dir = repo
	if cmd.Run() == nil {
		t.Fatal("setup: the rebase did not conflict")
	}
	// The fake agent merges both sides by hand, as the prompt asks.
	writeFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\nprintf 'main\\nwork\\n' > doc.md\n"+
		`echo '{"type":"result","is_error":false,"total_cost_usd":0.01,"structured_output":{"status":"resolved","summary":"kept both"}}'`+"\n", 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	c := &Ctx{Repo: repo, Scratch: t.TempDir(), Issue: 1, Title: "t"}
	if !resolveConflicts(c) {
		t.Fatal("resolveConflicts = false")
	}
	if b, _ := os.ReadFile(doc); string(b) != "main\nwork\n" {
		t.Fatalf("doc.md = %q", b)
	}
	if out, _ := sh(repo, "git", "status"); strings.Contains(out, "rebase in progress") {
		t.Fatalf("rebase still in progress:\n%s", out)
	}

	// An agent that leaves markers is refused, so publish aborts instead of committing them.
	run("checkout", "-qb", "work2", "HEAD~1")
	writeFile(t, doc, "other\n", 0o644)
	run("commit", "-qam", "other")
	cmd = exec.Command("git", "rebase", "work")
	cmd.Dir = repo
	if cmd.Run() == nil {
		t.Fatal("setup: the second rebase did not conflict")
	}
	writeFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\n"+
		`echo '{"type":"result","is_error":false,"total_cost_usd":0.01,"structured_output":{"status":"resolved","summary":"done"}}'`+"\n", 0o755)
	if resolveConflicts(c) {
		t.Fatal("conflict markers were accepted")
	}
}
