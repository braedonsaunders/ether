package plugins

import "syscall"

// Darwin's thread-scoped background policy lowers CPU and disk IO priority.
func lowerJobPriority() { _ = syscall.Setpriority(3, 0, 0x1000) }
