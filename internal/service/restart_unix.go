//go:build !windows

package service

import "syscall"

// restartProcess replaces this process with exe (same PID, so a container
// or terminal session carries on). File descriptors are close-on-exec, so
// the listener and the data-directory lock go with the old image.
func restartProcess(exe string, args, env []string) error {
	return syscall.Exec(exe, args, env)
}
