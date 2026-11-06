//go:build !linux && !windows

package mediaproc

import (
	"syscall"
	"testing"
	"time"
)

func assertStopped(t *testing.T, pid int) {
	t.Helper()
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("probe child %d still exists after cancellation", pid)
}
