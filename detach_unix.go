//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the carrier in its own session so it survives the shell, the
// terminal and the parent agent that armed it.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 tests existence without delivering anything.
	return syscall.Kill(pid, 0) == nil
}

func killPID(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

func shell() string     { return "/bin/sh" }
func shellFlag() string { return "-c" }
