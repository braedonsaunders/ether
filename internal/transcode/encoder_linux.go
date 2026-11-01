//go:build linux

package transcode

import "syscall"

// armParentDeath kills ffmpeg when the guardian dies, including SIGKILL.
// The guardian's stdin watch cannot run after an uncatchable signal, and the
// encoder is in its own process group so it would otherwise keep running.
func armParentDeath(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
