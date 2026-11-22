//go:build unix

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
	useProfile(t, defaultProfile("claude"))
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// Write then rename, so the watcher below never reads a half-written pid file.
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nwait\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
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
