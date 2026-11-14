package plugins

import "syscall"

// This is called only on a locked, disposable background-job OS thread.
func lowerJobPriority() {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, syscall.Gettid(), 10)
	_, _, _ = syscall.Syscall(syscall.SYS_IOPRIO_SET, 1, uintptr(syscall.Gettid()), 3<<13)
}
