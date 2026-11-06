package transcode

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

func TestRecoveryBufferStopsAtHolesAndRequiresAudio(t *testing.T) {
	s := &Session{segDur: 2 * time.Second, proto: core.ProtocolDASH, total: 20}
	s.lastReq = 2
	s.dash = &dashState{reps: []dashRep{{Kind: "video"}, {Kind: "audio"}, {Kind: "video"}}}
	s.done = []map[int]bool{{2: true, 3: true, 4: true, 6: true}, {2: true, 3: true, 6: true}, {}}
	b := s.RecoveryBuffer()
	if b.Position != 4*time.Second || b.ReadyUntil != 8*time.Second {
		t.Fatal(b)
	}
	s.proto = core.ProtocolHLS
	b = s.RecoveryBuffer()
	if b.ReadyUntil != 10*time.Second {
		t.Fatal("hole counted as playable", b)
	}
	s.lastReq = 6
	b = s.RecoveryBuffer()
	if b.Position != 12*time.Second || b.ReadyUntil != 14*time.Second {
		t.Fatal("seek reused old buffer", b)
	}
}

func TestBufferDistinguishesPublishedFromDelivered(t *testing.T) {
	s := &Session{segDur: 4 * time.Second, total: 30, proto: core.ProtocolHLS}
	s.done = []map[int]bool{{}}
	for n := 0; n < 12; n++ {
		s.done[0][n] = true
	}
	s.completedDeliveries = map[int]map[int]bool{0: {0: true, 1: true, 3: true}}
	b := s.RecoveryBuffer()
	if b.ReadyUntil != 48*time.Second || b.DeliveredUntil != 8*time.Second {
		t.Fatalf("server buffer confused with delivery: %+v", b)
	}
	if defaultOptions().maxAhead < 40*time.Second {
		t.Fatal("default encoder cannot fill requested reserve")
	}
}

func TestReplacementOutputWaitsForContiguousPublishedMedia(t *testing.T) {
	s := &Session{segDur: 2 * time.Second, total: 20, proto: core.ProtocolHLS}
	s.input = "replacement"
	s.swapBoundary = 3
	s.done = []map[int]bool{{0: true, 1: true, 3: true}}
	s.changed = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.WaitForReplacementOutput(ctx, "replacement"); err != context.Canceled {
		t.Fatalf("hole did not prevent readiness: %v", err)
	}
	s.mu.Lock()
	s.done[0][2] = true
	s.broadcastLocked()
	s.mu.Unlock()
	if err := s.WaitForReplacementOutput(t.Context(), "replacement"); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementOutputWaitRejectsAnAbandonedSeek(t *testing.T) {
	s := &Session{segDur: 2 * time.Second, total: 20, proto: core.ProtocolHLS}
	s.input = "replacement"
	s.swapBoundary = 3
	s.done = []map[int]bool{{0: true, 1: true, 2: true, 3: true}}
	s.changed = make(chan struct{})
	s.recoverySeekAt = time.Unix(1, 0)
	if err := s.WaitForReplacementOutput(t.Context(), "replacement"); err != core.ErrReplacementSuperseded {
		t.Fatalf("seek returned %v", err)
	}
}

func TestBufferedSwapPreservesPublishedMediaAndViewerPosition(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 20 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{}, WithBuffering(16*time.Second, 8*time.Second, 8*time.Second))
	req := remuxRequest(t, "buffer-swap", time.Hour)
	s := startSession(t, m, req)
	original := get(t, s, 0).Body.Bytes()
	waitFor(t, "prepared buffer", func() bool { return s.RecoveryBuffer().ReadyUntil >= 8*time.Second })
	if err := s.SwapInputBuffered(context.Background(), "http://127.0.0.1:1/source/b", req.Info, 0); err != nil {
		t.Fatal(err)
	}
	if s.RequestedPosition() != 0 {
		t.Fatal("swap moved viewer ahead", s.RequestedPosition())
	}
	if got := get(t, s, 0).Body.Bytes(); !bytes.Equal(got, original) {
		t.Fatal("buffered original replaced")
	}
	if argValue(fr.lastStart(), "-ss") == "0.000" || argValue(fr.lastStart(), "-ss") == "" {
		t.Fatal("alternate restarted before buffer boundary")
	}
}

// Pace only encoding/remux runs, not probes or fixture generation. Without
// this, a fast machine can finish the entire fixture before the first poll and
// the test only decodes the old buffer, never exercising the handoff.
type repairHandoffRunner struct{ ExecRunner }

func (r repairHandoffRunner) Start(cmd Command) (Process, error) {
	for i, arg := range cmd.Args {
		if arg == "-i" {
			args := append([]string(nil), cmd.Args[:i]...)
			args = append(args, "-readrate", "8")
			cmd.Args = append(args, cmd.Args[i:]...)
			break
		}
	}
	return r.ExecRunner.Start(cmd)
}

// Real ffmpeg must decode the preserved prefix and replacement suffix without
// fetching old input or restarting the viewer at the handoff.
func TestIntegrationBufferedRepairHandoff(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithRunner(repairHandoffRunner{}), WithBuffering(8*time.Second, 4*time.Second, 4*time.Second))
	e.gen(t, "buffer-a.mkv", 32, "-c:v", "libx264", "-preset", "ultrafast", "-threads", "2", "-g", "48", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("buffer-a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	hs, err := e.m.StartHLS(t.Context(), core.HLSRequest{SessionID: "buffered-repair", InputURL: e.url("buffer-a.mkv"), Info: *info, Decision: e.m.Decide(*info, plexWeb, allow()), SegmentDuration: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s := hs.(*Session)
	first := fetch(t, hs, 0)
	waitFor(t, "real prepared buffer", func() bool { return s.RecoveryBuffer().ReadyUntil >= 6*time.Second })
	if err := s.SwapInputBuffered(t.Context(), e.url("buffer-a.mkv")+"?replacement=1", *info, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReplacementOutput(t.Context(), e.url("buffer-a.mkv")+"?replacement=1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	handoffFrame := s.cur.startSeg * 2 * 24
	s.mu.Unlock()
	if handoffFrame <= 0 || handoffFrame >= 480 {
		t.Fatalf("test did not place the handoff inside delivered media: frame %d", handoffFrame)
	}
	if s.RequestedPosition() != 0 || !bytes.Equal(fetch(t, hs, 0), first) {
		t.Fatal("handoff discarded original buffer")
	}
	// Consume beyond the retained buffer and decode the assembled TS stream.
	var media []byte
	for n := 0; n < 10; n++ {
		media = append(media, fetch(t, hs, n)...)
	}
	path := e.writeTemp(t, "repair-handoff.ts", media)
	// Check the delivered packets before decoding; an output muxer's guessed
	// frame-rate timebase must not hide a discontinuity at the handoff.
	packets := e.videoPackets(t, path)
	if len(packets) != 480 {
		t.Fatalf("handoff has %d video packets, want 480", len(packets))
	}
	for i := 1; i < len(packets); i++ {
		d := packets[i].pts - packets[i-1].pts
		// TS may absorb the source's AAC priming offset at a restart (21 ms in
		// this fixture). Allow a shorter positive boundary step, never duplicate
		// timestamps, a backwards step, or a missing frame anywhere.
		minStep := 1.0/24 - 0.0015
		if i == handoffFrame {
			minStep = 0
		}
		if d <= minStep || d > 1.0/24+0.0015 {
			t.Fatalf("video packet %d has invalid presentation step %.6f (handoff=%d)", i, d, handoffFrame)
		}
		if packets[i].dts <= packets[i-1].dts {
			t.Fatalf("video DTS regressed at packet %d", i)
		}
	}
	// The Windows null muxer can guess a coarse frame-rate timebase and round
	// distinct input timestamps to the same output tick. Preserve TS precision
	// and every decoded frame instead of testing that unrelated conversion.
	out, err := mediaproc.CommandContext(t.Context(), 2*time.Minute, e.ffmpeg, "-v", "error", "-xerror", "-i", path, "-fps_mode", "passthrough", "-enc_time_base:v", "1:90000", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("handoff decode: %v: %s", err, out)
	}
}

func TestBufferedReplacementStartsWhileOldMediaRemainsPlayable(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 10 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{}, WithBuffering(8*time.Second, 48*time.Second, 8*time.Second))
	req := remuxRequest(t, "slow-buffered-replacement", time.Hour)
	s := startSession(t, m, req)
	original := get(t, s, 0).Body.Bytes()
	waitFor(t, "forty seconds encoded", func() bool { return s.RecoveryBuffer().ReadyUntil >= 40*time.Second })
	// Stop the sole producer before changing its fake rate. The replacement
	// takes longer than serving retained segments: it must not block them.
	s.mu.Lock()
	s.stopRunLocked()
	s.mu.Unlock()
	fr.segDelay = 300 * time.Millisecond
	replacement := req.Info
	replacement.Duration -= time.Second // harmless trailing duration difference
	if err := s.SwapInputBuffered(t.Context(), "http://127.0.0.1:1/source/replacement", replacement, 0); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	boundary := s.cur.startSeg
	s.mu.Unlock()
	if boundary <= 0 || s.RequestedPosition() != 0 {
		t.Fatalf("lost reserve at %d", boundary)
	}
	started := time.Now()
	if got := get(t, s, 0).Body.Bytes(); !bytes.Equal(got, original) {
		t.Fatal("lost old media")
	}
	if time.Since(started) >= 200*time.Millisecond {
		t.Fatal("old segment waited for replacement encoding")
	}
	// Production must progress without the player first requesting the suffix.
	waitFor(t, "replacement produced ahead of viewer", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.done[0][boundary] })
	if s.RequestedPosition() != 0 {
		t.Fatal("preparation advanced viewer")
	}
}
