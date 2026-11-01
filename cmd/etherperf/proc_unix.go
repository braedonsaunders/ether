//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// The server runs in its own process group so the sandbox wrapper and the
// server stop together.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func interruptGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGINT) }

func killGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }
