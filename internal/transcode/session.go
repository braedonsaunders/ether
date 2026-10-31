package transcode

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Session is one HLS or DASH remux or transcode. It implements
// core.HLSSession, and core.DASHSession for DASH sessions.
//
// Segment n always covers media time base+n*segDur (base is the request's
// StartOffset for HLS and 0 for DASH, whose MPD always spans the whole
// title). ffmpeg runs are started at a segment boundary with -ss and
// -copyts, so restarting at any segment (a seek far ahead or behind, a
// failover to another release) keeps numbering and timestamps continuous.
//
// An HLS session has one track of muxed segments. A DASH session has one
// track per representation (video, audio); ffmpeg's dash muxer writes short
// chunks that the session regroups into numbered segments by timestamp.
type Session struct {
	encoder                 *encoderLifetime
	id                      string
	m                       *Manager
	dir                     string
	watchers                sync.WaitGroup // running watch goroutines; Close waits for them
	removals                pendingRemovals
	segDur                  time.Duration
	base                    time.Duration
	total                   int // segments in the playlist; 0 = unknown duration
	container               string
	proto                   string // core.ProtocolHLS or core.ProtocolDASH
	requireIndependentProof bool
	// False until the packager can prove all segments across all rungs,
	// including copies, switches, and failovers, are independently decodable.
	segmentsIndependent bool
	log                 *slog.Logger
	created             time.Time
	lastAccess          atomic.Int64 // unix nanos
	pauseUntil          atomic.Int64 // bounded authenticated player pause
	mu                  sync.Mutex
	closed              bool
	// Coordinated state transitions use mu; blocking process reaping releases it.
	sessionInput
	sessionEncoding
	sessionDelivery
	sessionAdaptation
}

// IndependentSegments implements core.HLSIndependentSegments. It reports
// a proven all-renditions guarantee, not the legacy playlist convention.
func (s *Session) IndependentSegments() bool { return s.segmentsIndependent }

// sessionInput owns one aspect of the session. Fields are guarded by Session.mu.
type sessionInput struct {
	rtxConfig          config.RTXVideo
	rtxTimeline        bool
	rtxDiscontinuities map[int]bool
	liveInput          bool
	inputLease         core.EncoderInputLease
	inputHeaders       map[string]string
	input              string
	info               core.MediaInfo
	decision           core.Decision
	forceCPU           bool // hardware encoder failed; stay on CPU
	noPause            bool // platform cannot suspend ffmpeg
	inputFailed        bool // exhausted this input; HTTP retries cannot restart it
	// contract is the delivered video a static-header session keeps
	// across failovers; nil for every other session (contract.go).
	contract       *videoContract
	inputCrop      *pictureCrop
	pictureSamples map[string]*pictureCrop
	audioMatch     audioMatchState
}

// sessionEncoding owns one aspect of the session. Fields are guarded by Session.mu.
type sessionEncoding struct {
	starting <-chan struct{}
	reaping  <-chan struct{} // previous encoder exit barrier; waited without mu
	resumeAt int             // segment to restart at after a throttle stop; -1 none
	cur      *run
	runSeq   int
	endSeg   int // last segment of the input once a run hit EOF; -1 unknown
	failures int // consecutive runs that failed without output
	failErr  error
	// deniedFilters are names ffmpeg reported missing. The next graph is
	// built without them. Unknown filters are never assumed present.
	deniedFilters   map[string]bool
	filterFallbacks int
	graphReported   bool
	// hevcFallbackWarned latches the HEVC->libx264 fallback warning to
	// at most once per session (warned from startRunLocked, under mu).
	hevcFallbackWarned bool
}

// sessionDelivery owns one aspect of the session. Fields are guarded by Session.mu.
type sessionDelivery struct {
	completedDeliveries map[int]map[int]bool // bounded by the rolling segment window
	done                []map[int]bool       // finished segments per track
	inflight            map[int]int          // video segment -> in-flight GETs
	lastReq             int                  // playhead: oldest fetch still inside the restart window
	recoverySeekAt      time.Time
	mkvDelivery         map[int]mkvDelivery // last response per track, including retries of a seek
	// Published Matroska data segment path -> the video stream that
	// produced it: a copy of an input, or an encode run (chunks.go).
	mkvSources map[string]string
	// reqSeq numbers segment requests as they arrive; movedBy is the number
	// of the request that last moved the encoder. A request that arrived
	// before that move is one the player abandoned by seeking: it must not
	// drag the encoder back to its old position.
	reqSeq   uint64
	movedBy  uint64
	initFile string
	changed  chan struct{}
	playlist []byte
	listed   []int      // unknown duration: per-track listed prefix; never shrinks
	dash     *dashState // DASH sessions only
	// fmp4Edits is, per HLS fMP4 init scope, the edits of the first init
	// published there: the clock later runs are held to (fmp4_edit.go).
	fmp4Edits map[clockScope]map[string]trackEdit
}

// sessionAdaptation owns one aspect of the session. Fields are guarded by Session.mu.
type sessionAdaptation struct {
	// Adaptive HLS engine: the shared segment plan (HLS only), the config
	// snapshot, the ABR ladder and measured delivery goodput.
	plan           *Timeline
	adapt          adaptiveSettings
	rungs          []rung
	initFiles      []string // per HLS track: init path relative to dir
	wantTrack      int      // track the encoder should be producing (rung/rep)
	videoRequested bool     // a client has requested video media, not just initialization
	deliveredTrack int
	deliveredSeg   int
	deliveredCount int // sequential complete media deliveries on the active run
	goodput        goodputEstimator
	segSizes       map[int]int64 // primary-track segment sizes (spike governor)
	governorOn     bool          // plan may change ahead of the player
	lastGovSeg     int           // last segment the governor evaluated from
	govCooldown    time.Time     // no new clamp planning before this
	seekRampSeg    int           // segment a seek restarted at (preset ramp); -1 none
}

// mkvDelivery keeps a retried segment on the same representation: a seek
// may serve the GOP covering an empty cluster, while sequential playback
// needs the empty cluster to avoid repeating that GOP.
type mkvDelivery struct {
	segment int
	seek    bool
}

// run is one ffmpeg process producing segments from startSeg on.
type run struct {
	rtx          bool
	rtxConfirmed atomic.Bool

	seq        int
	startSeg   int
	spec       runSpec
	proc       Process
	watchWork  *encoderWork
	stopWork   *encoderWork
	hw         bool
	maxDone    int // last segment finished (DASH: on every track)
	produced   int // segments finished (DASH: on the slowest track)
	finished   bool
	stopped    bool // stopped deliberately, not a failure
	paused     bool
	pausedDone int // maxDone when Pause was applied; later growth means it did not hold
	err        error
	stderr     *tailBuffer
	started    time.Time
	mode       SegMode // plan mode this run produces
	planEnd    int     // first segment NOT produced by this run (mode change); -1 none
	track      int     // session track (HLS rung / DASH video rep) encoded

	progressMu sync.Mutex
	progress   Progress

	dash   *dashRun  // DASH runs only; owned by the run's watch goroutine
	chunks *chunkRun // chunked HLS runs only (chunks.go); likewise
	edits  *runEdits // HLS fMP4 runs: correction onto the retained clock (fmp4_edit.go)
}

func (r *run) next() int {
	if r.produced == 0 {
		return r.startSeg
	}
	return r.maxDone + 1
}

// ID implements core.HLSSession.
func (s *Session) ID() string { return s.id }

// LastAccess implements core.HLSSession.
func (s *Session) LastAccess() time.Time { return time.Unix(0, s.lastAccess.Load()) }

// Touch marks the session as in use (e.g. on a Plex timeline ping while the
// client is paused) so the idle reaper keeps it.
func (s *Session) Touch() { s.lastAccess.Store(time.Now().UnixNano()) }

var _ core.SessionActivity = (*Session)(nil)

func (s *Session) PlaybackState(state string) {
	switch state {
	case "paused":
		if s.pauseUntil.Swap(time.Now().Add(core.PausedSessionRetention).UnixNano()) == 0 {
			s.log.Info("player paused; retaining output", "retention", core.PausedSessionRetention)
		}
		s.Touch()
	case "playing":
		s.Touch()
		if s.pauseUntil.Swap(0) != 0 {
			s.log.Info("player resumed")
		}
	case "stopped":
		s.pauseUntil.Store(0)
	}
}

// Decision is the session's current decision (it can change on SwapInput).
func (s *Session) Decision() core.Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decision
}

// encoding reports whether the session counts against the transcode cap:
// it is a transcode by decision, or a copy session whose current run
// encodes (a fast-start ramp, a governor clamp or a lower rung).
func (s *Session) encoding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.decision.VideoCopy {
		return true
	}
	r := s.cur
	if r == nil || r.finished || r.stopped || r.err != nil {
		return false
	}
	return r.mode != ModeCopy || r.track != 0
}

// Info is the media info of the current input.
func (s *Session) Info() core.MediaInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// Progress is the current ffmpeg run's latest progress report.
func (s *Session) Progress() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.cur
	if r == nil {
		return Progress{}
	}
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	return r.progress
}

// HWAccelerated reports whether the current run encodes on hardware.
func (s *Session) HWAccelerated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && s.cur.hw
}

// SegmentDuration is the session's segment length.
func (s *Session) SegmentDuration() time.Duration { return s.segDur }

// RequestedPosition is the title position of the player's latest segment
// request. Source reads can be far ahead or at the container's tail index.
func (s *Session) RequestedPosition() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.segStart(s.lastReq)
}

// Container is "mpegts", "fmp4", or explicitly negotiated "mkv".
func (s *Session) Container() string { return s.container }

// Protocol is core.ProtocolHLS or core.ProtocolDASH.
func (s *Session) Protocol() string { return s.proto }

func (s *Session) isDASH() bool { return s.proto == core.ProtocolDASH }

func (s *Session) segName(n int) string { return s.m.opts.segmentURI(n, s.container) }

// segPath is the file of segment n of the first track.
func (s *Session) segPath(n int) string { return s.trackSegPath(0, n) }

// runDirLocked is where the wanted track's ffmpeg run writes segments:
// the session dir for the primary rung, a rung subdir for other rungs
// (segment N has different bytes per rung).
func (s *Session) runDirLocked() string {
	if s.isDASH() || s.wantTrack <= 0 || s.wantTrack >= len(s.rungs) {
		return s.dir
	}
	return filepath.Join(s.dir, "rung-"+s.rungs[s.wantTrack].id)
}

func (s *Session) trackSegPath(t, n int) string {
	if s.isDASH() {
		return filepath.Join(s.dir, fmt.Sprintf("seg-%d-%05d.m4s", t, n))
	}
	name := segmentFile(n, segmentExt(s.container))
	if t > 0 && t < len(s.rungs) {
		return filepath.Join(s.dir, "rung-"+s.rungs[t].id, name)
	}
	return filepath.Join(s.dir, name)
}

func (s *Session) segStart(n int) time.Duration {
	if s.plan != nil && !s.isDASH() {
		return s.base + s.plan.Start(n)
	}
	return s.base + time.Duration(n)*s.segDur
}

func (s *Session) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Close implements core.HLSSession: kill ffmpeg and delete the segments.
func (s *Session) Close() error {
	if err := s.RetireEncoder(); err != nil {
		return err
	}
	timer := time.NewTimer(s.m.encoderWait())
	defer timer.Stop()
	select {
	case <-s.encoder.done:
		return s.encoder.legacyResult()
	case <-timer.C:
		return core.ErrEncoderPending
	}
}
