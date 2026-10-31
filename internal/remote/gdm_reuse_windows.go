//go:build windows

package remote

import "syscall"

func setGDMReuseAddr(fd uintptr) {
	_ = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
