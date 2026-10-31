//go:build !windows

package remote

import "syscall"

func setGDMReuseAddr(fd uintptr) {
	_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
