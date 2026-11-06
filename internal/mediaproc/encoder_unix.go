//go:build !windows

package mediaproc

import (
	"os/exec"
	"syscall"
)

func Prepare(cmd *exec.Cmd) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	cmd.SysProcAttr = attr
}
func Own(*exec.Cmd) (func(), error) { return func() {}, nil }
func Stop(cmd *exec.Cmd)            { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

// PrepareGuardian additionally ties Linux parent death to the guardian's locked OS thread.
// Ordinary Go callers must not use Pdeathsig: scheduler threads can retire.
func PrepareGuardian(cmd *exec.Cmd) { Prepare(cmd); armParentDeath(cmd.SysProcAttr) }
