//go:build !windows && !linux

package transcode

import "syscall"

func armParentDeath(*syscall.SysProcAttr) {}
