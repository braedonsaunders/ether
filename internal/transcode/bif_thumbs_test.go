package transcode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// thumbFake installs a runFunc on fr that answers thumbnail extractions:
// it records every -ss value and writes a fake JPEG tagged with the seek
// second. endAt >= 0 makes seeks at/after it produce no frame (EOF).
type thumbFake struct {
	mu      sync.Mutex
	seeks   []float64
	inputs  []string
	endAt   float64
	fail    bool
	active  atomic.Int32
	maxSeen atomic.Int32
	block   chan struct{} // when non-nil, extractions wait on it (or ctx)
}

func (f *thumbFake) run(ctx context.Context, cmd Command) error {
	if argValue(cmd.Args, "-f") != "image2pipe" {
		return errUnhandled
	}
	cur := f.active.Add(1)
	for {
		old := f.maxSeen.Load()
		if cur <= old || f.maxSeen.CompareAndSwap(old, cur) {
			break
		}
	}
	defer f.active.Add(-1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ss, _ := strconv.ParseFloat(argValue(cmd.Args, "-ss"), 64)
	f.mu.Lock()
	f.seeks = append(f.seeks, ss)
	f.inputs = append(f.inputs, argValue(cmd.Args, "-i"))
	fail, endAt := f.fail, f.endAt
	f.mu.Unlock()
	if fail {
		return errors.New("exit status 1")
	}
	if endAt >= 0 && ss >= endAt {
		return nil // no frame decoded past end of input
	}
	_, err := cmd.Stdout.Write(fakeJPEG(byte(int(ss)%250+1), 64))
	return err
}

func (f *thumbFake) inputList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inputs...)
}

func (f *thumbFake) seekList() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.seeks...)
}

func newTestThumbs(t *testing.T, tf *thumbFake, mutate func(*ThumbOptions)) *Thumbnails {
	t.Helper()
	fr := newFakeRunner()
	fr.runFunc = tf.run
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ff, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := ThumbOptions{
		Runner: fr, FFmpeg: ff, Logger: testLogger(t),
		Interval: 10 * time.Second, BatchFrames: 3, BatchPause: time.Millisecond,
		FrameTimeout: 5 * time.Second,
	}
	if mutate != nil {
		mutate(&o)
	}
	th := NewThumbnails(o)
	t.Cleanup(func() { _ = th.Close() })
	return th
}

func waitThumbFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (t *Thumbnails) frameCount(releaseID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[releaseID]; ok {
		return len(e.frames)
	}
	return -1
}

// isIdle reports that no extraction runs for a release.
func (t *Thumbnails) isIdle(releaseID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.entries[releaseID]
	return ok && t.running[releaseID] == nil
}

func (t *Thumbnails) isComplete(releaseID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[releaseID]
	return ok && e.complete
}

func TestThumbArgs(t *testing.T) {
	args := thumbArgs("http://127.0.0.1:9/src", 65*time.Second+250*time.Millisecond, 320, 7)
	// -ss must precede -i (input seeking), one frame, one thread.
	ss, in := -1, -1
	for i, a := range args {
		switch a {
		case "-ss":
			ss = i
		case "-i":
			in = i
		}
	}
	if ss < 0 || in < 0 || ss > in {
		t.Fatalf("seek order wrong: %v", args)
	}
	if got := argValue(args, "-ss"); got != "65.250" {
		t.Errorf("-ss = %q", got)
	}
	if got := argValue(args, "-threads"); got != "1" {
		t.Errorf("-threads = %q", got)
	}
	if got := argValue(args, "-frames:v"); got != "1" {
		t.Errorf("-frames:v = %q", got)
	}
	if got := argValue(args, "-vf"); got != "scale=320:-2,format=yuvj420p" {
		t.Errorf("-vf = %q", got)
	}
}

func TestPlanFramesKeyframes(t *testing.T) {
	var kfs []core.KeyframePoint
	for s := 0; s <= 60; s += 4 {
		kfs = append(kfs, core.KeyframePoint{Offset: int64(s * 1000), Time: time.Duration(s) * time.Second})
	}
	plan := planFrames(55*time.Second, 10*time.Second, kfs)
	// Last keyframe at/before 0,10,20,30,40,50 → 0,8,20,28,40,48.
	want := []time.Duration{0, 8, 20, 28, 40, 48}
	if len(plan) != len(want) {
		t.Fatalf("plan = %v, want %v", plan, want)
	}
	for i := range want {
		if plan[i] != want[i]*time.Second {
			t.Errorf("plan[%d] = %s, want %s", i, plan[i], want[i]*time.Second)
		}
	}
	// No keyframes: plain grid. No duration or keyframes: empty.
	if got := planFrames(25*time.Second, 10*time.Second, nil); len(got) != 3 {
		t.Errorf("grid plan = %v", got)
	}
	if got := planFrames(0, 10*time.Second, nil); got != nil {
		t.Errorf("unknown duration plan = %v", got)
	}
	// Duration inferred from last keyframe.
	if got := planFrames(0, 10*time.Second, kfs); len(got) == 0 {
		t.Error("keyframe-inferred plan is empty")
	}
}

func TestThumbnailsGenerateAndServe(t *testing.T) {
	tf := &thumbFake{endAt: -1}
	th := newTestThumbs(t, tf, nil)
	src := core.TrickplaySource{
		ReleaseID: "rel-1", InputURL: "http://127.0.0.1:9/src",
		Info: core.MediaInfo{Duration: 65 * time.Second}, AvailableUntil: 65 * time.Second,
	}
	th.EnsureTrickplay(context.Background(), src)
	if !th.TrickplayAvailable("rel-1") {
		t.Fatal("not available while generating")
	}
	waitThumbFor(t, "generation stopped", func() bool { return th.isIdle("rel-1") && th.frameCount("rel-1") == 3 })
	// The bounded look-behind keeps only frames near the current playhead;
	// the earlier ones are still missing, so the release is not complete.
	if th.isComplete("rel-1") {
		t.Error("complete with frames 0-30s never made")
	}
	if got := th.frameCount("rel-1"); got != 3 {
		t.Fatalf("frames = %d", got)
	}
	seeks := tf.seekList()
	if len(seeks) != 3 {
		t.Fatalf("seeks = %v", seeks)
	}
	for i, s := range seeks {
		if s != float64(40+i*10) {
			t.Errorf("seek %d = %v", i, s)
		}
	}
	for _, input := range tf.inputList() {
		if input != "http://127.0.0.1:9/src?background=1" {
			t.Fatalf("thumbnail input %q does not select background read priority", input)
		}
	}
	// BIF snapshot parses and carries the frames.
	data, ok := th.TrickplayBIF("rel-1")
	if !ok {
		t.Fatal("no BIF")
	}
	b, err := ParseBIF(data)
	if err != nil {
		t.Fatal(err)
	}
	if b.Len() != 3 || b.Time(2) != 60*time.Second {
		t.Errorf("BIF: len=%d last=%s", b.Len(), b.Time(6))
	}
	// Single frame: latest at or before.
	fr, ok := th.TrickplayFrame("rel-1", 47*time.Second)
	if !ok || !IsJPEG(fr) || fr[3] != byte(40+1) {
		t.Errorf("frame at 47s: ok=%v tag=%v", ok, fr)
	}
	// A second Ensure after completion starts nothing new.
	th.EnsureTrickplay(context.Background(), src)
	time.Sleep(20 * time.Millisecond)
	if got := len(tf.seekList()); got != 3 {
		t.Errorf("seeks after re-Ensure = %d", got)
	}
	if _, ok := th.TrickplayBIF("nope"); ok {
		t.Error("unknown release has a BIF")
	}
}

// A local file input (the spike playback path) must reach ffmpeg as a
// plain path: "?background=1" is loopback query protocol, and appended to
// a filename it makes the file not exist.
func TestThumbnailsLocalInputHasNoBackgroundQuery(t *testing.T) {
	if got := backgroundReadURL("/x/spike.mp4"); got != "/x/spike.mp4" {
		t.Errorf("backgroundReadURL(local) = %q", got)
	}
	if got := backgroundReadURL("http://127.0.0.1:9/src"); got != "http://127.0.0.1:9/src?background=1" {
		t.Errorf("backgroundReadURL(http) = %q", got)
	}
	if got := backgroundReadURL("https://127.0.0.1:9/src?x=1"); got != "https://127.0.0.1:9/src?x=1&background=1" {
		t.Errorf("backgroundReadURL(https+query) = %q", got)
	}

	tf := &thumbFake{endAt: -1}
	th := newTestThumbs(t, tf, nil)
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "rel-local", InputURL: "/x/spike.mp4",
		Info: core.MediaInfo{Duration: 65 * time.Second}, AvailableUntil: 65 * time.Second,
	})
	waitThumbFor(t, "local generation stopped", func() bool { return th.isIdle("rel-local") && th.frameCount("rel-local") == 3 })
	for _, input := range tf.inputList() {
		if input != "/x/spike.mp4" {
			t.Fatalf("thumbnail input %q is not the plain local path", input)
		}
	}
}

func TestThumbnailsKeyframeSeeks(t *testing.T) {
	tf := &thumbFake{endAt: -1}
	th := newTestThumbs(t, tf, nil)
	var kfs []core.KeyframePoint
	for s := 0; s <= 60; s += 4 {
		kfs = append(kfs, core.KeyframePoint{Offset: int64(s), Time: time.Duration(s) * time.Second})
	}
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "rel-kf", InputURL: "http://x/",
		Info: core.MediaInfo{Duration: 55 * time.Second, Keyframes: kfs}, AvailableUntil: 55 * time.Second,
	})
	waitThumbFor(t, "stopped", func() bool { return th.isIdle("rel-kf") && th.frameCount("rel-kf") > 0 })
	for _, s := range tf.seekList() {
		if int(s)%4 != 0 {
			t.Errorf("seek %v not on a keyframe", s)
		}
	}
}

func TestThumbnailsOnlyAdvanceThroughPlaybackFrontier(t *testing.T) {
	tf := &thumbFake{endAt: -1}
	th := newTestThumbs(t, tf, nil)
	src := core.TrickplaySource{
		ReleaseID: "progressive", InputURL: "http://x/",
		Info: core.MediaInfo{Duration: 60 * time.Second}, AvailableUntil: 20 * time.Second,
	}
	th.EnsureTrickplay(context.Background(), src)
	waitThumbFor(t, "first frontier", func() bool {
		th.mu.Lock()
		defer th.mu.Unlock()
		return th.running["progressive"] == nil && len(th.entries["progressive"].frames) == 3
	})
	if got := tf.seekList(); len(got) != 3 || got[0] != 0 || got[1] != 10 || got[2] != 20 {
		t.Fatalf("initial seeks=%v; must stop at current playback position", got)
	}
	src.AvailableUntil = 40 * time.Second
	th.EnsureTrickplay(context.Background(), src)
	waitThumbFor(t, "extended frontier", func() bool {
		th.mu.Lock()
		defer th.mu.Unlock()
		return th.running["progressive"] == nil && len(th.entries["progressive"].frames) == 5
	})
	if got := tf.seekList(); len(got) != 5 || got[3] != 30 || got[4] != 40 {
		t.Fatalf("extended seeks=%v; expected only newly available intervals", got)
	}
}

func TestThumbnailsSingleJobPerReleaseAndGlobalCap(t *testing.T) {
	tf := &thumbFake{endAt: -1, block: make(chan struct{})}
	th := newTestThumbs(t, tf, func(o *ThumbOptions) { o.MaxConcurrent = 1 })
	src := func(id string) core.TrickplaySource {
		return core.TrickplaySource{ReleaseID: id, InputURL: "http://x/", Info: core.MediaInfo{Duration: 30 * time.Second}, AvailableUntil: 30 * time.Second}
	}
	th.EnsureTrickplay(context.Background(), src("a"))
	th.EnsureTrickplay(context.Background(), src("a")) // duplicate: no-op
	th.EnsureTrickplay(context.Background(), src("b"))
	th.EnsureTrickplay(context.Background(), src("c"))
	waitThumbFor(t, "first extraction active", func() bool { return tf.active.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if got := tf.maxSeen.Load(); got != 1 {
		t.Fatalf("concurrent extractions = %d, want cap 1", got)
	}
	close(tf.block)
	for _, id := range []string{"a", "b", "c"} {
		id := id
		waitThumbFor(t, "complete "+id, func() bool { return th.isComplete(id) })
	}
	// 3 releases × 3 frames (0,10,20), no duplicates from the re-Ensure.
	if got := len(tf.seekList()); got != 9 {
		t.Errorf("total seeks = %d, want 9", got)
	}
}

func TestThumbnailsFailureBackoffAndEOF(t *testing.T) {
	// Failing source: job gives up, release stops being advertised.
	tf := &thumbFake{endAt: -1, fail: true}
	th := newTestThumbs(t, tf, func(o *ThumbOptions) { o.RetryBackoff = time.Hour })
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "bad", InputURL: "http://x/", Info: core.MediaInfo{Duration: time.Minute}, AvailableUntil: time.Minute,
	})
	waitThumbFor(t, "job to stop", func() bool {
		th.mu.Lock()
		defer th.mu.Unlock()
		return th.running["bad"] == nil
	})
	if th.TrickplayAvailable("bad") {
		t.Error("failed release still advertised")
	}
	seeks := len(tf.seekList())
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "bad", InputURL: "http://x/", Info: core.MediaInfo{Duration: time.Minute}, AvailableUntil: time.Minute,
	})
	time.Sleep(20 * time.Millisecond)
	if got := len(tf.seekList()); got != seeks {
		t.Errorf("retried during backoff: %d -> %d", seeks, got)
	}

	// EOF before the nominal duration ends generation as complete.
	tf2 := &thumbFake{endAt: 30}
	th2 := newTestThumbs(t, tf2, nil)
	th2.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "short", InputURL: "http://x/", Info: core.MediaInfo{Duration: 95 * time.Second}, AvailableUntil: 30 * time.Second,
	})
	waitThumbFor(t, "eof complete", func() bool { return th2.isComplete("short") })
	if got := th2.frameCount("short"); got != 3 { // 0,10,20 (30 is EOF)
		t.Errorf("frames before EOF = %d", got)
	}
}

func TestThumbnailsFallbackAndEviction(t *testing.T) {
	tf := &thumbFake{endAt: -1}
	th := newTestThumbs(t, tf, func(o *ThumbOptions) { o.MaxReleases = 2 })
	ctx := context.Background()
	// Fallback only: BIF spans the duration once a session is ensured.
	th.SetTrickplayFallback("fb", fakeJPEG(77, 48))
	if _, ok := th.TrickplayBIF("fb"); ok {
		t.Fatal("fallback without duration should not make a BIF yet")
	}
	th.EnsureTrickplay(ctx, core.TrickplaySource{
		ReleaseID: "fb", InputURL: "http://x/", Info: core.MediaInfo{Duration: 35 * time.Second}, AvailableUntil: 30 * time.Second,
	})
	waitThumbFor(t, "fb complete", func() bool { return th.isComplete("fb") })
	data, ok := th.TrickplayBIF("fb")
	if !ok {
		t.Fatal("no BIF")
	}
	b, err := ParseBIF(data)
	if err != nil {
		t.Fatal(err)
	}
	if b.Len() != 4 { // real frames at 0,10,20,30 cover every slot
		t.Errorf("BIF len = %d", b.Len())
	}
	// Partial real coverage: remove middle frames and confirm the
	// fallback fills the gaps in the snapshot.
	th.mu.Lock()
	e := th.entries["fb"]
	delete(e.frames, 10*time.Second)
	delete(e.frames, 20*time.Second)
	th.mu.Unlock()
	data, _ = th.TrickplayBIF("fb")
	b, _ = ParseBIF(data)
	if b.Len() != 4 {
		t.Fatalf("filled BIF len = %d", b.Len())
	}
	if b.Frame(1)[3] != 77 || b.Frame(2)[3] != 77 {
		t.Error("gap frames are not the fallback")
	}
	// Frame endpoint falls back before the first real frame time... real
	// frame at 0 exists, so 5s serves it; unknown release misses.
	if _, ok := th.TrickplayFrame("gone", 0); ok {
		t.Error("unknown release frame")
	}
	// Eviction: two more releases push the LRU one out (cap 2).
	for _, id := range []string{"e1", "e2"} {
		th.EnsureTrickplay(ctx, core.TrickplaySource{
			ReleaseID: id, InputURL: "http://x/", Info: core.MediaInfo{Duration: 5 * time.Second}, AvailableUntil: 5 * time.Second,
		})
		waitThumbFor(t, id+" complete", func() bool { return th.isComplete(id) })
	}
	th.mu.Lock()
	n := len(th.entries)
	_, fbLeft := th.entries["fb"]
	th.mu.Unlock()
	if n > 2 {
		t.Errorf("entries = %d, want <= 2", n)
	}
	if fbLeft {
		t.Error("LRU entry fb was not evicted")
	}
}

func TestThumbnailsCloseCancelsJobs(t *testing.T) {
	tf := &thumbFake{endAt: -1, block: make(chan struct{})}
	fr := newFakeRunner()
	fr.runFunc = tf.run
	th := NewThumbnails(ThumbOptions{
		Runner: fr, FFmpeg: "ffmpeg", Logger: testLogger(t),
		BatchPause: time.Millisecond, FrameTimeout: time.Minute,
	})
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "x", InputURL: "http://x/", Info: core.MediaInfo{Duration: time.Hour}, AvailableUntil: time.Hour,
	})
	waitThumbFor(t, "extraction blocked", func() bool { return tf.active.Load() == 1 })
	done := make(chan struct{})
	go func() { _ = th.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the blocked job")
	}
	// Ensure after Close is a no-op (no panic, no new job).
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "y", InputURL: "http://x/", Info: core.MediaInfo{Duration: time.Minute},
	})
	if th.TrickplayAvailable("y") {
		t.Error("entry created after Close")
	}
}

func TestThumbnailsCancelReleaseStopsActiveExtraction(t *testing.T) {
	tf := &thumbFake{endAt: -1, block: make(chan struct{})}
	th := newTestThumbs(t, tf, nil)
	th.EnsureTrickplay(context.Background(), core.TrickplaySource{
		ReleaseID: "stop-me", InputURL: "http://x/",
		Info: core.MediaInfo{Duration: time.Minute}, AvailableUntil: time.Minute,
	})
	waitThumbFor(t, "blocked extraction", func() bool { return tf.active.Load() == 1 })
	th.CancelTrickplay("stop-me")
	waitThumbFor(t, "release cancellation", func() bool {
		th.mu.Lock()
		defer th.mu.Unlock()
		return th.running["stop-me"] == nil
	})
}

// A seek back (or a resume late in the title) leaves frames before the
// read-behind window unmade: they are made when playback reads there, and
// only then is the release complete.
func TestThumbnailsFillAfterSeekBack(t *testing.T) {
	tf := &thumbFake{endAt: -1}
	th := newTestThumbs(t, tf, nil)
	src := core.TrickplaySource{ReleaseID: "resume", InputURL: "http://x/",
		Info: core.MediaInfo{Duration: 65 * time.Second}, AvailableUntil: 65 * time.Second}
	th.EnsureTrickplay(context.Background(), src)
	waitThumbFor(t, "late frames", func() bool { return th.isIdle("resume") && th.frameCount("resume") == 3 })
	src.AvailableUntil = 25 * time.Second // rewatch from the start
	th.EnsureTrickplay(context.Background(), src)
	waitThumbFor(t, "early frames", func() bool { return th.isIdle("resume") && th.frameCount("resume") == 6 })
	if th.isComplete("resume") {
		t.Fatal("complete with the 30s frame missing")
	}
	src.AvailableUntil = 35 * time.Second
	th.EnsureTrickplay(context.Background(), src)
	waitThumbFor(t, "complete", func() bool { return th.isComplete("resume") })
	if got := th.frameCount("resume"); got != 7 {
		t.Errorf("frames = %d", got)
	}
}
