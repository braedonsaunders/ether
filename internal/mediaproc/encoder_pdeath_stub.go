//go:build !windows && !linux

package mediaproc

import "syscall"

func armParentDeath(*syscall.SysProcAttr) {}
