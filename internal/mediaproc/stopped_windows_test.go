package mediaproc

import (
	"testing"

	"golang.org/x/sys/windows"
)

func assertStopped(t *testing.T, pid int) {
	t.Helper()
	p, e := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if e != nil {
		return
	}
	defer windows.CloseHandle(p)
	state, e := windows.WaitForSingleObject(p, 1000)
	if e != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("probe child %d still running: %v", pid, e)
	}
}
