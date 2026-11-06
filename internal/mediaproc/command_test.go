package mediaproc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestFakeFFprobe(t *testing.T) {
	switch os.Getenv("ETHER_FAKE_PROBE") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestFakeFFprobe$")
		child.Env = append(os.Environ(), "ETHER_FAKE_PROBE=child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("ETHER_PROBE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600)
		for {
			time.Sleep(time.Hour)
		}
	case "child":
		for {
			_ = os.WriteFile(os.Getenv("ETHER_PROBE_BEAT"), []byte(time.Now().String()), 0600)
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// Deadline and explicit cancellation both kill a hung probe and its child,
// including a child holding stdout/stderr open after its parent is terminated.
func TestProbeDeadlineKillsTree(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			beat := filepath.Join(dir, "beat")
			pid := filepath.Join(dir, "pid")
			t.Setenv("ETHER_FAKE_PROBE", "parent")
			t.Setenv("ETHER_PROBE_PID", pid)
			t.Setenv("ETHER_PROBE_BEAT", beat)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			timeout := 500 * time.Millisecond
			if mode == "cancel" {
				timeout = 30 * time.Second
			}
			cmd := CommandContext(ctx, timeout, os.Args[0], "-test.run=^TestFakeFFprobe$")
			if mode == "cancel" {
				go func() { time.Sleep(500 * time.Millisecond); cancel() }()
			}
			started := time.Now()
			_, err := cmd.Output()
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				t.Fatalf("deadline: %v", err)
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("probe retained its pipes past deadline")
			}
			b, err := os.ReadFile(beat)
			if err != nil {
				t.Fatalf("child never started: %v", err)
			}
			childPIDBytes, e := os.ReadFile(pid)
			if e != nil {
				t.Fatal(e)
			}
			childPID, e := strconv.Atoi(string(childPIDBytes))
			if e != nil {
				t.Fatal(e)
			}
			assertStopped(t, childPID)
			time.Sleep(80 * time.Millisecond)
			after, _ := os.ReadFile(beat)
			if string(b) != string(after) {
				t.Fatal("probe child survived")
			}
		})
	}
}
