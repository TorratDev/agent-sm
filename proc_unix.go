//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup runs cmd in its own process group so the whole tree can be
// signalled together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends sig to every process in pid's group.
func killProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// agentCommand returns name and args unchanged: only Windows wraps agent CLIs in batch shims.
func agentCommand(name string, args []string) (string, []string) { return name, args }
