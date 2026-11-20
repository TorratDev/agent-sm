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
	os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 300 &\necho $! > "+pidFile+"\nwait\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	ctx, cancel := context.WithCancel(context.Background())
	runCtx = ctx
	t.Cleanup(func() { runCtx = context.Background() })
	go func() {
		for {
			if _, err := os.Stat(pidFile); err == nil {
				time.Sleep(50 * time.Millisecond) // let the shell finish writing it
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
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("child process %d outlived the killed session", pid)
	}
	if c.CostUnknown != 1 {
		t.Errorf("CostUnknown = %d, want 1 for a killed session", c.CostUnknown)
	}
}
