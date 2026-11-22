//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An npm .cmd shim is run through node, so cmd.exe never sees (and cuts) a multi-line prompt.
func TestAgentCommandUnwrapsNpmShim(t *testing.T) {
	dir := t.TempDir()
	shim := "@ECHO off\r\nendLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & \"%_prog%\"  \"%dp0%\\node_modules\\@github\\copilot\\npm-loader.js\" %*\r\n"
	os.WriteFile(filepath.Join(dir, "fakeagent.cmd"), []byte(shim), 0o644)
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	name, args := agentCommand("fakeagent", []string{"-p", "line one\nline two"})
	want := filepath.Join(dir, `node_modules\@github\copilot\npm-loader.js`)
	if name != "node" || len(args) != 3 || args[0] != want || args[2] != "line one\nline two" {
		t.Fatalf("agentCommand = %q %q", name, args)
	}
	os.WriteFile(filepath.Join(dir, "node.exe"), nil, 0o644) // a node beside the shim wins, as in the shim
	if name, _ := agentCommand("fakeagent", nil); name != filepath.Join(dir, "node.exe") {
		t.Fatalf("agentCommand node = %q", name)
	}
	if name, args := agentCommand("missing", []string{"x"}); name != "missing" || args[0] != "x" {
		t.Fatalf("an unknown command changed: %q %q", name, args)
	}
}
