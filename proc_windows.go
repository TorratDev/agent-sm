//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
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
