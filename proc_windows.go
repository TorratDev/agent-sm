//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const ctrlBreakEvent = 1 // CTRL_BREAK_EVENT; not exposed by the syscall package.

var procGenerateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// setProcessGroup runs cmd in its own process group (CREATE_NEW_PROCESS_GROUP
// is the Windows analogue of setpgid).
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessGroup terminates the tree rooted at pid. Windows has no POSIX
// signals: SIGKILL is approximated with a forced taskkill /T, other signals
// with a CTRL_BREAK_EVENT broadcast to the group (requires a shared console).
func killProcessGroup(pid int, sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	}
	r1, _, errno := procGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(pid))
	if r1 == 0 {
		return fmt.Errorf("console break to group %d: %w", pid, errno)
	}
	return nil
}

// npmShimScript finds the script an npm .cmd shim hands to node: "%dp0%\node_modules\...\x.js".
var npmShimScript = regexp.MustCompile(`"%dp0%\\([^"]+\.js)"`)

// agentCommand runs an npm-installed CLI through node rather than its .cmd shim:
// cmd.exe cuts a batch file's command line at the first newline, so a multi-line -p prompt
// and every flag after it (session id, tool limits) would never reach the agent.
func agentCommand(name string, args []string) (string, []string) {
	path, err := exec.LookPath(name)
	if err != nil || !strings.EqualFold(filepath.Ext(path), ".cmd") {
		return name, args
	}
	shim, err := os.ReadFile(path)
	if err != nil {
		return name, args
	}
	m := npmShimScript.FindSubmatch(shim)
	if m == nil {
		return name, args
	}
	dir := filepath.Dir(path)
	node := filepath.Join(dir, "node.exe") // the shim prefers a node beside it
	if _, err := os.Stat(node); err != nil {
		node = "node"
	}
	return node, append([]string{filepath.Join(dir, string(m[1]))}, args...)
}
