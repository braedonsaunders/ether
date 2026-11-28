package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestEncoderLifecycleHelper(t *testing.T) {
	mode := os.Getenv("ETHER_TEST_ENCODER_MODE")
	switch mode {
	case "parent":
		os.Setenv("ETHER_TEST_ENCODER_MODE", "child")
		p, err := (ExecRunner{}).Start(Command{Path: os.Args[0], Args: []string{"-test.run=^TestEncoderLifecycleHelper$"}})
		if err != nil {
			os.Exit(10)
		}
		// Establish that the child is actually running before the parent exits.
		ready := false
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			if b, err := os.ReadFile(os.Getenv("ETHER_TEST_ENCODER_HEARTBEAT")); err == nil && len(b) > 0 {
				ready = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !ready {
			p.Stop()
			os.Exit(11)
		}
		// Deliberately bypass defer/shutdown, as an abrupt Ether exit does.
		os.WriteFile(os.Getenv("ETHER_TEST_ENCODER_PID"), []byte(strconv.Itoa(p.(*execProcess).PID())), 0600)
		os.Exit(0)
	case "child":
		// One appended line per beat, so a kill can never erase beats that
		// already landed (os.WriteFile truncates first: a kill between its
		// truncate and write left an empty file). Each beat holds the file
		// open before writing, so the guardian's kill often lands mid-write
		// and the death check exercises exactly that boundary.
		path := os.Getenv("ETHER_TEST_ENCODER_HEARTBEAT")
		for i := 0; i < 300; i++ {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err != nil {
				os.Exit(12)
			}
			time.Sleep(18 * time.Millisecond)
			fmt.Fprintf(f, "%d\n", i)
			f.Close()
			time.Sleep(2 * time.Millisecond)
		}
		os.Exit(0)
	case "output":
		os.Stdout.Write([]byte("encoder output"))
		os.Exit(0)
	case "wait":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		t.Skip("subprocess helper")
	}
}

func TestEncoderGuardianOutputAndCancellation(t *testing.T) {
	t.Setenv("ETHER_TEST_ENCODER_MODE", "output")
	var out bytes.Buffer
	if err := (ExecRunner{}).Run(context.Background(), Command{Path: os.Args[0], Args: []string{"-test.run=^TestEncoderLifecycleHelper$"}, Stdout: &out}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "encoder output" {
		t.Fatalf("handshake leaked into output: %q", out.String())
	}
	t.Setenv("ETHER_TEST_ENCODER_MODE", "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := (ExecRunner{}).Run(ctx, Command{Path: os.Args[0], Args: []string{"-test.run=^TestEncoderLifecycleHelper$"}}); err != context.DeadlineExceeded {
		t.Fatalf("cancel: %v", err)
	}
}

// A session/cleanup lock must not be required to stop an owned process. Use the
// existing helper executable, keeping ordinary tests independent of FFmpeg.
func TestEncoderLifetimeFencesProcessOutsideCleanupQueue(t *testing.T) {
	t.Setenv("ETHER_TEST_ENCODER_MODE", "wait")
	life := &encoderLifetime{cancel: make(chan struct{})}
	for _, late := range []bool{false, true} {
		p, err := (ExecRunner{}).Start(Command{Path: os.Args[0], Args: []string{"-test.run=^TestEncoderLifecycleHelper$"}})
		if err != nil {
			t.Fatal(err)
		}
		life.mu.Lock() // cleanup and session-state progress deliberately unavailable
		life.fenceProcess(p, nil)
		if !late {
			close(life.cancel)
		}
		select {
		case <-p.Done():
		case <-time.After(5 * time.Second):
			life.mu.Unlock()
			p.Stop()
			t.Fatal("lease cancellation did not terminate owned process")
		}
		life.mu.Unlock()
	}
}

func TestEncoderDiesAfterParentExit(t *testing.T) {
	dir := t.TempDir()
	heartbeat, pidFile := filepath.Join(dir, "heartbeat"), filepath.Join(dir, "pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestEncoderLifecycleHelper$")
	cmd.Env = append(os.Environ(), "ETHER_TEST_ENCODER_MODE=parent", "ETHER_TEST_ENCODER_PID="+pidFile, "ETHER_TEST_ENCODER_HEARTBEAT="+heartbeat)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup owns only the child created by this test if a regression keeps it alive.
	defer func() {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
			p.Release()
		}
	}()
	read := func() []byte {
		b, err := os.ReadFile(heartbeat)
		if err != nil {
			t.Fatalf("read heartbeat: %v", err)
		}
		return b
	}
	time.Sleep(150 * time.Millisecond)
	first := read()
	time.Sleep(150 * time.Millisecond)
	second := read()
	// The parent only exits once it has seen a beat, and beats are append-only,
	// so an empty file means the protocol lost a completed beat.
	if len(first) == 0 {
		t.Fatal("heartbeat file is empty although the parent saw the encoder start")
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("encoder survived parent exit: heartbeat grew from %d to %d beats",
			bytes.Count(first, []byte("\n")), bytes.Count(second, []byte("\n")))
	}
}

func TestPauseResumeNoOpAfterProcessDone(t *testing.T) {
	t.Setenv("ETHER_TEST_ENCODER_MODE", "output")
	p, err := (ExecRunner{}).Start(Command{Path: os.Args[0], Args: []string{"-test.run=^TestEncoderLifecycleHelper$"}})
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if err := p.Pause(); err != nil {
		t.Fatalf("Pause after exit returned error: %v", err)
	}
	if err := p.Resume(); err != nil {
		t.Fatalf("Resume after exit returned error: %v", err)
	}

	// Simulate PID reuse after exit: a live unrelated process now owns p.pid
	// while p.done is already closed. Neither Pause nor Resume may signal it.
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat")
	victim := exec.Command(os.Args[0], "-test.run=^TestEncoderLifecycleHelper$")
	victim.Env = append(os.Environ(), "ETHER_TEST_ENCODER_MODE=child", "ETHER_TEST_ENCODER_HEARTBEAT="+heartbeat)
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(heartbeat); err == nil && len(b) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	done := make(chan struct{})
	close(done)
	reused := &execProcess{pid: victim.Process.Pid, done: done}
	if err := reused.Pause(); err != nil {
		t.Fatalf("Pause on exited process with reused pid: %v", err)
	}
	before, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Fatalf("Pause signaled reused pid %d and froze live process (%d -> %d bytes)", victim.Process.Pid, len(before), len(after))
	}
	reused.paused = true
	if err := reused.Resume(); err != nil {
		t.Fatalf("Resume on exited process with reused pid: %v", err)
	}
}
