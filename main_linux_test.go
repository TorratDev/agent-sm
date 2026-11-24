package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A stopped (or timed-out) session takes its child processes with it.
func TestAgentKillsProcessGroup(t *testing.T) {
	useProfile(t, defaultProfile("pi"))
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// Write then rename, so the watcher below never reads a half-written pid file.
	os.WriteFile(filepath.Join(dir, "pi"), []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nwait\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	ctx, cancel := context.WithCancel(context.Background())
	runCtx = ctx
	t.Cleanup(func() { runCtx = context.Background() })
	go func() {
		for {
			if _, err := os.Stat(pidFile); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	c := &Ctx{Repo: dir, Scratch: dir}
	var r struct{}
	if o := agent(c, Review, "x", &r); o == Ok {
		t.Fatalf("a killed session returned %v", o)
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("no child pid recorded")
	}
	// The signal is asynchronous: give the child up to 5s to die instead of a fixed sleep.
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("child process %d outlived the killed session", pid)
			break
		}
	}
	if c.CostUnknown != 1 {
		t.Errorf("CostUnknown = %d, want 1 for a killed session", c.CostUnknown)
	}
}

func TestPiAuth(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	fake := func(check string, bearerExit int) {
		script := "#!/bin/sh\ncase \"$2\" in check) echo '" + check + "';; print-bearer-token) exit " + strconv.Itoa(bearerExit) + ";; esac\n"
		if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		check  string
		bearer int
		want   string
	}{
		{`{"status":"ready","provider":"opencode-go","authType":"api_key"}`, 1, ""},
		{`{"status":"ready","provider":"anthropic","authType":"oauth"}`, 0, ""},
		{`{"status":"ready","provider":"anthropic","authType":"oauth"}`, 1, "pi auth login anthropic"},
		{`{"status":"invalid","provider":"anthropic","authType":"oauth"}`, 0, "are invalid; run `pi auth login anthropic`"},
		{`not json`, 0, "unknown (not json)"},
	} {
		fake(tc.check, tc.bearer)
		err := piAuth(context.Background(), "m", 60)
		if (err == nil) != (tc.want == "") || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("check %s, bearer exit %d: got %v, want %q", tc.check, tc.bearer, err, tc.want)
		}
	}
}
