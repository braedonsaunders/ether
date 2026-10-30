package transcode

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Thumbnails builds sparse BIF preview indexes for releases while they
// stream. Extraction only ever reads a source handed to EnsureTrickplay
// (the loopback input of an active playback or warmup session): one
// fast-seek ffmpeg run per frame decodes a single (key)frame, so bytes
// are read around the requested frame only — never the whole file ahead
// of playback. Jobs run at most one per release under a small global
// cap, fill in bounded batches with pauses, and stop when the source
// stops answering (the session ended) or Close is called.
//
// It implements core.TrickplayProvider.
type Thumbnails struct {
	runner Runner
	ffmpeg string
	log    *slog.Logger

	interval     time.Duration
	width        int
	quality      int
	frameTimeout time.Duration
	batchFrames  int
	batchPause   time.Duration
	maxBytes     int64
	maxReleases  int
	retryBackoff time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	entries map[string]*thumbEntry
	running map[string]context.CancelFunc
	seq     uint64
	closed  bool
}

var _ core.TrickplayProvider = (*Thumbnails)(nil)

const thumbReadBehind = 30 * time.Second

// ThumbOptions tunes a Thumbnails store. Runner and FFmpeg are required;
// zero limits take the defaults documented on each field.
type ThumbOptions struct {
	Runner Runner
	FFmpeg string
	Logger *slog.Logger

	Interval      time.Duration // frame spacing (default 10s)
	Width         int           // thumbnail width px (default 320, Plex "sd")
	Quality       int           // ffmpeg -q:v (default 7)
	FrameTimeout  time.Duration // per-frame extraction cap (default 25s)
	BatchFrames   int           // frames per batch before a pause (default 12)
	BatchPause    time.Duration // pause between batches (default 4s)
	MaxConcurrent int           // global extraction jobs (default 2)
	MaxBytes      int64         // total stored JPEG bytes, LRU-evicted (default 256MiB)
	MaxReleases   int           // releases kept, LRU-evicted (default 32)
	RetryBackoff  time.Duration // pause before retrying a failed release (default 2m)
}

type thumbEntry struct {
	frames         map[time.Duration][]byte
	fallback       []byte
	duration       time.Duration
	availableUntil time.Duration // latest playback position reported (it moves back on a seek)
	interval       time.Duration
	complete       bool
	dead           bool // generation failed with nothing to show; don't advertise
	retryAt        time.Time
	bytes          int64
	seq            uint64
}

// NewThumbnails builds an empty thumbnail store. Call Close on shutdown.
func NewThumbnails(o ThumbOptions) *Thumbnails {
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = 10 * time.Second
	}
	if o.Width <= 0 {
		o.Width = 320
	}
	if o.Quality <= 0 {
		o.Quality = 7
	}
	if o.FrameTimeout <= 0 {
		o.FrameTimeout = 25 * time.Second
	}
	if o.BatchFrames <= 0 {
		o.BatchFrames = 12
	}
	if o.BatchPause < 0 {
		o.BatchPause = 0
	} else if o.BatchPause == 0 {
		o.BatchPause = 4 * time.Second
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 2
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 256 << 20
	}
	if o.MaxReleases <= 0 {
		o.MaxReleases = 32
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Thumbnails{
		runner: o.Runner, ffmpeg: o.FFmpeg, log: o.Logger.With("component", "trickplay"),
		interval: o.Interval, width: o.Width, quality: o.Quality,
		frameTimeout: o.FrameTimeout, batchFrames: o.BatchFrames, batchPause: o.BatchPause,
		maxBytes: o.MaxBytes, maxReleases: o.MaxReleases, retryBackoff: o.RetryBackoff,
		ctx: ctx, cancel: cancel, sem: make(chan struct{}, o.MaxConcurrent),
		entries: map[string]*thumbEntry{}, running: map[string]context.CancelFunc{},
	}
}

// Close stops all generation and waits for jobs to finish.
func (t *Thumbnails) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.cancel()
	t.wg.Wait()
	return nil
}

// PreviewInterval and PreviewWidth describe the grid the shared provider
// generates. Callers compose sheets from these values instead of assuming a
// second thumbnail cache.
func (t *Thumbnails) PreviewInterval() time.Duration {
	if t == nil || t.interval <= 0 {
		return 0
	}
	return t.interval
}

func (t *Thumbnails) PreviewWidth() int {
	if t == nil || t.width <= 0 {
		return 0
	}
	return t.width
}

// EnsureTrickplay implements core.TrickplayProvider.
func (t *Thumbnails) EnsureTrickplay(_ context.Context, src core.TrickplaySource) {
	if src.ReleaseID == "" || src.InputURL == "" || t.ffmpeg == "" {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	e := t.entryLocked(src.ReleaseID)
	if src.Info.Duration > 0 {
		e.duration = src.Info.Duration
	}
	// Follow the latest position, backwards too: after a seek back the
	// frames there are read again, and those it skipped are still missing.
	if src.AvailableUntil > 0 {
		e.availableUntil = src.AvailableUntil
	}
	if t.running[src.ReleaseID] != nil || e.complete || time.Now().Before(e.retryAt) {
		t.mu.Unlock()
		return
	}
	e.dead = false
	jobCtx, cancel := context.WithCancel(t.ctx)
	t.running[src.ReleaseID] = cancel
	t.wg.Add(1)
	t.mu.Unlock()
	go t.generate(jobCtx, src)
}

// CancelTrickplay stops the active extraction for a release. Playback calls
// this after the last viewer session for that release ends.
func (t *Thumbnails) CancelTrickplay(releaseID string) {
	t.mu.Lock()
	cancel := t.running[releaseID]
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// TrickplayAvailable implements core.TrickplayProvider.
func (t *Thumbnails) TrickplayAvailable(releaseID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[releaseID]
	return ok && !e.dead
}

// TrickplayBIF implements core.TrickplayProvider.
func (t *Thumbnails) TrickplayBIF(releaseID string) ([]byte, bool) {
	t.mu.Lock()
	e, ok := t.entries[releaseID]
	if !ok {
		t.mu.Unlock()
		return nil, false
	}
	t.touchLocked(e)
	frames := e.snapshotLocked()
	t.mu.Unlock()
	if len(frames) == 0 {
		return nil, false
	}
	data, err := MarshalBIF(frames)
	if err != nil {
		return nil, false
	}
	return data, true
}

// TrickplayFrame implements core.TrickplayProvider.
func (t *Thumbnails) TrickplayFrame(releaseID string, at time.Duration) ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[releaseID]
	if !ok {
		return nil, false
	}
	t.touchLocked(e)
	var best, first time.Duration
	found, haveFirst := false, false
	for tm := range e.frames {
		if !haveFirst || tm < first {
			first, haveFirst = tm, true
		}
		if tm <= at && (!found || tm > best) {
			best, found = tm, true
		}
	}
	switch {
	case found:
		return append([]byte(nil), e.frames[best]...), true
	case len(e.fallback) > 0:
		return append([]byte(nil), e.fallback...), true
	case haveFirst:
		return append([]byte(nil), e.frames[first]...), true
	}
	return nil, false
}

// SetTrickplayFallback implements core.TrickplayProvider.
func (t *Thumbnails) SetTrickplayFallback(releaseID string, jpeg []byte) {
	if releaseID == "" || !IsJPEG(jpeg) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	e := t.entryLocked(releaseID)
	e.bytes -= int64(len(e.fallback))
	e.fallback = append([]byte(nil), jpeg...)
	e.bytes += int64(len(jpeg))
	e.dead = false
	t.evictLocked(releaseID)
}

// entryLocked returns (creating if needed) the entry for a release.
func (t *Thumbnails) entryLocked(releaseID string) *thumbEntry {
	e, ok := t.entries[releaseID]
	if !ok {
		e = &thumbEntry{frames: map[time.Duration][]byte{}, interval: t.interval}
		t.entries[releaseID] = e
	}
	t.touchLocked(e)
	return e
}

func (t *Thumbnails) touchLocked(e *thumbEntry) {
	t.seq++
	e.seq = t.seq
}

// snapshotLocked lists the frames for a BIF build: real frames plus, when
// a fallback still and a duration are known, fallback frames filling the
// not-yet-generated interval slots so the index spans the title.
func (e *thumbEntry) snapshotLocked() []BIFFrame {
	frames := make([]BIFFrame, 0, len(e.frames))
	for tm, jpeg := range e.frames {
		frames = append(frames, BIFFrame{Time: tm, JPEG: jpeg})
	}
	if len(e.fallback) > 0 && e.duration > 0 && e.interval > 0 {
		for _, slot := range intervalSlots(e.duration, e.interval) {
			if _, ok := e.frames[slot]; ok {
				continue
			}
			covered := false
			for _, f := range frames {
				if f.Time > slot-e.interval/2 && f.Time <= slot+e.interval/2 {
					covered = true
					break
				}
			}
			if !covered {
				frames = append(frames, BIFFrame{Time: slot, JPEG: e.fallback})
			}
		}
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].Time < frames[j].Time })
	return frames
}

// evictLocked enforces the byte and release-count budgets, dropping the
// least recently used entries that are not generating and not keep.
func (t *Thumbnails) evictLocked(keep string) {
	for {
		var total int64
		for _, e := range t.entries {
			total += e.bytes
		}
		if total <= t.maxBytes && len(t.entries) <= t.maxReleases {
			return
		}
		victim, victimSeq := "", uint64(0)
		for id, e := range t.entries {
			if id == keep || t.running[id] != nil {
				continue
			}
			if victim == "" || e.seq < victimSeq {
				victim, victimSeq = id, e.seq
			}
		}
		if victim == "" {
			return
		}
		delete(t.entries, victim)
	}
}

// intervalSlots is the frame grid for a duration: 0, interval, 2*interval…
func intervalSlots(duration, interval time.Duration) []time.Duration {
	if interval <= 0 {
		return nil
	}
	var out []time.Duration
	for at := time.Duration(0); at < duration; at += interval {
		out = append(out, at)
	}
	if duration > 0 && len(out) == 0 {
		out = append(out, 0)
	}
	return out
}

// planFrames picks the extraction time for each interval slot. With a
// container keyframe index it uses the last keyframe at or before each
// slot boundary, keeping extraction behind the already-read playback
// position. Without keyframes it uses the interval grid.
func planFrames(duration, interval time.Duration, keyframes []core.KeyframePoint) []time.Duration {
	kfs := make([]core.KeyframePoint, 0, len(keyframes))
	for _, kf := range keyframes {
		if kf.Time >= 0 {
			kfs = append(kfs, kf)
		}
	}
	sort.Slice(kfs, func(i, j int) bool { return kfs[i].Time < kfs[j].Time })
	if duration <= 0 && len(kfs) > 0 {
		duration = kfs[len(kfs)-1].Time + interval
	}
	slots := intervalSlots(duration, interval)
	if len(kfs) == 0 {
		return slots
	}
	out := make([]time.Duration, 0, len(slots))
	ki := 0
	for _, slot := range slots {
		for ki+1 < len(kfs) && kfs[ki+1].Time <= slot {
			ki++
		}
		if kfs[ki].Time <= slot && (len(out) == 0 || kfs[ki].Time > out[len(out)-1]) {
			out = append(out, kfs[ki].Time)
		}
	}
	return out
}

// thumbArgs builds the ffmpeg invocation for one frame: an input-side
// seek (keyframe seek over the range-capable loopback source), a single
// decoded frame, scaled to the BIF width on one thread, MJPEG to stdout.
func thumbArgs(input string, at time.Duration, width, quality int) []string {
	ss := strconv.FormatFloat(at.Seconds(), 'f', 3, 64)
	vf := fmt.Sprintf("scale=%d:-2,format=yuvj420p", width)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1"}
	args = append(args, inputSafetyArgs(input, false)...)
	return append(args,
		"-ss", ss, "-i", input,
		"-frames:v", "1", "-vf", vf,
		"-q:v", strconv.Itoa(quality),
		"-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1",
	)
}

// extractFrame decodes one thumbnail at at. An empty result with a nil
// error means the input ended before at (no frame decoded).
func (t *Thumbnails) extractFrame(ctx context.Context, input string, at time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, t.frameTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := t.runner.Run(ctx, Command{
		Path:   t.ffmpeg,
		Args:   thumbArgs(input, at, t.width, t.quality),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[len(msg)-200:]
		}
		return nil, fmt.Errorf("ffmpeg thumbnail at %s: %w: %s", at, err, msg)
	}
	if stdout.Len() == 0 {
		return nil, nil
	}
	if !IsJPEG(stdout.Bytes()) {
		return nil, fmt.Errorf("ffmpeg thumbnail at %s: not a JPEG (%d bytes)", at, stdout.Len())
	}
	return stdout.Bytes(), nil
}

// generate fills one release's frames in batches until the plan is
// complete, the source stops answering, or the store closes.
func (t *Thumbnails) generate(ctx context.Context, src core.TrickplaySource) {
	defer t.wg.Done()
	defer func() {
		t.mu.Lock()
		if cancel := t.running[src.ReleaseID]; cancel != nil {
			cancel()
		}
		delete(t.running, src.ReleaseID)
		t.mu.Unlock()
	}()
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return
	}
	plan := planFrames(src.Info.Duration, t.interval, src.Info.Keyframes)
	failures := 0
	for {
		// Next missing batch.
		t.mu.Lock()
		e, ok := t.entries[src.ReleaseID]
		if !ok {
			t.mu.Unlock()
			return
		}
		availableUntil := e.availableUntil
		oldestAllowed := availableUntil - thumbReadBehind
		if oldestAllowed < 0 {
			oldestAllowed = 0
		}
		var batch []time.Duration
		for _, slot := range plan {
			if slot > availableUntil || slot < oldestAllowed {
				continue
			}
			if _, have := e.frames[slot]; !have {
				batch = append(batch, slot)
				if len(batch) >= t.batchFrames {
					break
				}
			}
		}
		t.mu.Unlock()
		if len(batch) == 0 {
			// Complete only when every planned frame exists: frames skipped
			// outside the read-behind window (a resume, a seek, a wait for a
			// job slot) are made when playback reads there again.
			t.mu.Lock()
			e, ok := t.entries[src.ReleaseID]
			complete := ok && e.duration > 0 && missingFrames(e, plan, e.duration) == 0
			t.mu.Unlock()
			t.finish(src.ReleaseID, complete, "")
			return
		}
		for _, slot := range batch {
			if ctx.Err() != nil {
				return
			}
			jpeg, err := t.extractFrame(ctx, backgroundReadURL(src.InputURL), slot)
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return
				}
				failures++
				t.log.Debug("thumbnail extraction failed", "release", src.ReleaseID, "at", slot, "err", err)
				if failures >= 3 {
					t.finish(src.ReleaseID, false, "extraction failing")
					return
				}
				continue
			case len(jpeg) == 0:
				// End of input: nothing at or after this slot exists. The
				// release is complete if every earlier frame was made.
				t.mu.Lock()
				e, ok := t.entries[src.ReleaseID]
				complete := ok && missingFrames(e, plan, slot) == 0
				t.mu.Unlock()
				t.finish(src.ReleaseID, complete, "")
				return
			}
			failures = 0
			t.mu.Lock()
			if e, ok := t.entries[src.ReleaseID]; ok {
				if old, have := e.frames[slot]; have {
					e.bytes -= int64(len(old))
				}
				e.frames[slot] = jpeg
				e.bytes += int64(len(jpeg))
				t.touchLocked(e)
				t.evictLocked(src.ReleaseID)
			}
			t.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.batchPause):
		}
	}
}

// missingFrames counts planned slots before end that have no frame.
func missingFrames(e *thumbEntry, plan []time.Duration, end time.Duration) int {
	n := 0
	for _, slot := range plan {
		if slot >= end {
			break
		}
		if _, ok := e.frames[slot]; !ok {
			n++
		}
	}
	return n
}

// backgroundReadURL selects the loopback handler's prefetch-priority path;
// thumbnail misses must never compete with live playback article reads.
// The query is loopback protocol only: a local file path must reach ffmpeg
// untouched, or it stops naming a file on disk.
func backgroundReadURL(input string) string {
	if !isNetworkURL(input) {
		return input
	}
	sep := "?"
	if strings.Contains(input, "?") {
		sep = "&"
	}
	return input + sep + "background=1"
}

// finish marks a release complete, or failed (with a retry backoff; a
// release with no frames and no fallback stops being advertised).
func (t *Thumbnails) finish(releaseID string, complete bool, why string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[releaseID]
	if !ok {
		return
	}
	if complete {
		e.complete = true
		e.retryAt = time.Time{}
		t.log.Debug("thumbnails complete", "release", releaseID, "frames", len(e.frames))
		return
	}
	if why == "" {
		// This is a progressive stop at the current playback read frontier,
		// not an extraction failure. A later timeline update can extend it.
		e.retryAt = time.Time{}
		return
	}
	e.retryAt = time.Now().Add(t.retryBackoff)
	if len(e.frames) == 0 && len(e.fallback) == 0 {
		e.dead = true
	}
	t.log.Debug("thumbnail generation paused", "release", releaseID, "why", why, "frames", len(e.frames))
}
