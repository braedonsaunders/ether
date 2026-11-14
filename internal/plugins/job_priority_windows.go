package plugins

import "golang.org/x/sys/windows"

func lowerJobPriority() {
	// THREAD_MODE_BACKGROUND_BEGIN applies CPU, IO and memory scheduling to this
	// thread only. The locked thread exits when its job goroutine returns.
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadPriority")
	_, _, _ = proc.Call(^uintptr(1), 0x00010000) // GetCurrentThread pseudo-handle -2
}
