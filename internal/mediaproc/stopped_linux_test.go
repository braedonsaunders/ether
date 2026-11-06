package mediaproc

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func assertStopped(t *testing.T, pid int) {
	t.Helper()
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return
		}
		// A killed child may briefly await reaping by the host's init process.
		if err == nil {
			_, state, _ := strings.Cut(string(b), ") ")
			if strings.HasPrefix(state, "Z ") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("probe child %d still running after cancellation", pid)
}
