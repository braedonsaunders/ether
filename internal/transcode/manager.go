// Package transcode wraps Ether's bundled ffmpeg: discovery, hardware
// encoder detection, probing, the direct play / remux / transcode decision
// and HLS and MPEG-DASH sessions. Manager implements core.Transcoder.
package transcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// ErrTooManyTranscodes is returned by StartHLS when config MaxTranscodes
// video transcodes are already running. Remuxes are not counted.
var ErrTooManyTranscodes = errors.New("transcode: too many concurrent transcodes")

// ErrImageSubtitle is returned when a bitmap subtitle is asked for as text.
var ErrImageSubtitle = errors.New("transcode: bitmap subtitles cannot be converted to text")

// Option tunes a Manager.
type Option func(*options)

type options struct {
	runtimeDir     string
	runner         Runner
	finder         *finder
	detectHW       bool
	idleTimeout    time.Duration
	segmentTimeout time.Duration
	restartAhead   time.Duration
	maxAhead       time.Duration
	keepBehind     time.Duration
	pollInterval   time.Duration
	segmentURI     func(n int, container string) string
	dashURLs       DASHURLs
	manifestWait   time.Duration
	onFailure      func(sessionID string, at time.Duration, err error)
	onInputFailure func(sessionID, inputURL string, at time.Duration, err error)
	onRunStart     func(inputURL string, from, to time.Duration)
	onEncoder      func(sessionID string, started bool, excerpt string)
	tracePlayback  bool
	// rungDwell is how long a run keeps producing its rung before a
	// request on another rung may replace it (see Session.rungDwellLocked).
	rungDwell time.Duration
	// audioMatchBudget bounds the best-effort loudness analysis before a
	// source swap (see Session.prepareAudioMatch).
	audioMatchBudget time.Duration
}

func defaultOptions() options {
	return options{
		runner:         ExecRunner{},
		detectHW:       true,
		idleTimeout:    2 * time.Minute,
		segmentTimeout: 45 * time.Second,
		restartAhead:   24 * time.Second,
		maxAhead:       2 * time.Minute,
		keepBehind:     3 * time.Minute,
		pollInterval:   150 * time.Millisecond,
		segmentURI:     SegmentName,
		dashURLs:       PlexDASHURLs,
		manifestWait:   15 * time.Second,
		rungDwell:      8 * time.Second,

		audioMatchBudget: defaultAudioMatchBudget,
	}
}

// WithRuntimeDir enables managed optional tool installation in this node-local
// directory. No downloads occur while RTX Video is off. Embedded/test managers
// that omit this option never install software.
func WithRuntimeDir(dir string) Option { return func(o *options) { o.runtimeDir = dir } }

// WithAudioMatchBudget bounds the best-effort loudness analysis run before a
// source swap (default 1.5s). Integration tests on slow or race-instrumented
// machines raise it; production keeps the short budget so a swap never waits.
func WithAudioMatchBudget(d time.Duration) Option {
	return func(o *options) { o.audioMatchBudget = d }
}

// WithRunner replaces the process runner (tests).
func WithRunner(r Runner) Option { return func(o *options) { o.runner = r } }

// WithoutHWDetection skips the startup hardware probe and encodes on CPU.
func WithoutHWDetection() Option { return func(o *options) { o.detectHW = false } }

// WithIdleTimeout sets how long an unrequested session lives (default 2m).
func WithIdleTimeout(d time.Duration) Option { return func(o *options) { o.idleTimeout = d } }

// WithSegmentTimeout bounds how long ServeSegment waits for ffmpeg
// (default 45s).
func WithSegmentTimeout(d time.Duration) Option { return func(o *options) { o.segmentTimeout = d } }

// withRungDwell sets how long one rung's run keeps the shared encoder
// before a request on another rung may restart it (default 8s; 0 = switch
// at once). Tests use it; the default suits real players.
func withRungDwell(d time.Duration) Option { return func(o *options) { o.rungDwell = d } }

// WithBuffering tunes seeking and disk use: a request more than
// restartAhead past the encoder restarts ffmpeg there (default 24s); ffmpeg
// is suspended when maxAhead in front of the player (default 2m, 0 = never);
// segments more than keepBehind behind the player are deleted (default 3m,
// 0 = never).
func WithBuffering(restartAhead, maxAhead, keepBehind time.Duration) Option {
	return func(o *options) {
		o.restartAhead, o.maxAhead, o.keepBehind = restartAhead, maxAhead, keepBehind
	}
}

// WithSegmentURI sets the segment URIs written into media playlists. The
// default is SegmentName ("00042.ts", relative to index.m3u8).
func WithSegmentURI(f func(n int, container string) string) Option {
	return func(o *options) {
		if f != nil {
			o.segmentURI = f
		}
	}
}

// WithDASHURLs sets the segment URL templates written into DASH MPDs. The
// default is PlexDASHURLs, the layout of real PMS.
func WithDASHURLs(f DASHURLs) Option {
	return func(o *options) {
		if f != nil {
			o.dashURLs = f
		}
	}
}

// WithManifestWait bounds how long a DASH session's first MPD request waits
// for ffmpeg's init segments to learn the exact codecs (default 15s); after
// that the MPD is served with codecs estimated from the probe.
func WithManifestWait(d time.Duration) Option { return func(o *options) { o.manifestWait = d } }

// WithFailureHandler is called (in its own goroutine) when ffmpeg fails
// mid-session, e.g. because the source lost articles; at is the media time
// playback had reached. Playback typically answers with SwapInput.
func WithFailureHandler(f func(sessionID string, at time.Duration, err error)) Option {
	return func(o *options) { o.onFailure = f }
}

// WithInputFailureHandler includes the failed input generation, so delayed
// encoder exits cannot fail over a healthy replacement that is already playing.
func WithInputFailureHandler(f func(sessionID, inputURL string, at time.Duration, err error)) Option {
	return func(o *options) { o.onInputFailure = f }
}

// WithRunStartHandler is told, before ffmpeg is spawned, which part of the
// input a new run will read first, so the source can fetch it in parallel
// while the encoder starts and locates it.
func WithRunStartHandler(f func(inputURL string, from, to time.Duration)) Option {
	return func(o *options) { o.onRunStart = f }
}

// WithEncoderObserver is told when ffmpeg starts or fails to start.
// excerpt is already redacted argument text and must not be required for playback.
func WithEncoderObserver(f func(sessionID string, started bool, excerpt string)) Option {
	return func(o *options) { o.onEncoder = f }
}

// WithTracePlayback logs Plex HLS playhead decisions and ffmpeg run
// start/stop reasons. It does not change encoding or delivery.
func WithTracePlayback(on bool) Option { return func(o *options) { o.tracePlayback = on } }

func withFinder(f finder) Option { return func(o *options) { o.finder = &f } }

// Manager runs ffmpeg for Ether. It implements core.Transcoder.
type Manager struct {
	subtitles      subtitleCache
	rtxSetup       *rtxSetupWorker
	audioMatchBusy atomic.Bool
	rtx            rtxBackendState
	encoders       map[*encoderLifetime]struct{}
	encoderIDs     map[string]*encoderLifetime
	encoderLimit   int
	encoderTimeout time.Duration
	encoderRemove  func(string) error
	reaperDone     chan struct{}

	log     *slog.Logger
	opts    options
	runner  Runner
	ffmpeg  string
	ffprobe string
	version string
	hw      core.HWAccel
	feat    features
	tempDir string
	// graphErr is the latest owner-visible reason a graph could not be built.
	graphErr string

	mu       sync.Mutex
	cfg      config.Transcode
	sessions map[string]*Session
	closed   bool

	stop chan struct{}
	wg   sync.WaitGroup
}

var _ core.Transcoder = (*Manager)(nil)
var _ core.HLSSession = (*Session)(nil)
var _ core.DASHSession = (*Session)(nil)

// New finds ffmpeg/ffprobe, detects hardware encoders and starts the idle
// session reaper. It fails with ErrFFmpegNotFound when no ffmpeg exists.
func New(cfg config.Transcode, logger *slog.Logger, opts ...Option) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}
	m := &Manager{
		log:        logger.With("component", "transcode"),
		opts:       o,
		runner:     o.runner,
		cfg:        cfg,
		sessions:   map[string]*Session{},
		stop:       make(chan struct{}),
		reaperDone: make(chan struct{}),
	}
	f := defaultFinder()
	if o.finder != nil {
		f = *o.finder
	}
	var err error
	m.ffmpeg, m.ffprobe, err = f.discover(cfg.FFmpegPath, cfg.FFprobePath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if m.version, err = toolVersion(ctx, m.runner, m.ffmpeg); err != nil {
		return nil, err
	}
	if m.feat, err = probeFeatures(ctx, m.runner, m.ffmpeg); err != nil {
		return nil, err
	}
	m.feat.jellyfinSegmentClock = strings.Contains(strings.ToLower(m.version), "jellyfin")
	if !m.feat.hasEncoder("libx264") {
		m.log.Warn("ffmpeg has no libx264; CPU transcoding unavailable", "ffmpeg", m.ffmpeg)
	}
	mode := cfg.HWAccel
	if !o.detectHW {
		mode = AccelNone
	}
	m.hw = hwDetector{runner: m.runner, ffmpeg: m.ffmpeg, goos: f.goos, log: m.log, statDevice: deviceExists}.
		detect(ctx, mode, cfg.VAAPIDevice, m.feat)

	if o.runtimeDir == "" {
		m.detectRTX(ctx, f.goos)
	}
	m.tempDir = cfg.TempDir
	if m.tempDir == "" {
		m.tempDir = filepath.Join(os.TempDir(), "ether-transcode")
	}
	if err := os.MkdirAll(m.tempDir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: temp dir: %w", err)
	}
	m.cleanStale()
	m.log.Info("ffmpeg features", "ffmpeg", m.ffmpeg, "version", m.version,
		"filters", featureLine(m.feat), "encoders", encoderLine(m.feat),
		"filtersProbed", m.feat.filtersProbed, "probe", probeState(m.feat))
	if notice := m.limitation(); notice != "" {
		m.log.Warn("transcoder limitation", "notice", notice)
	}
	m.log.Info("transcoder ready", "ffmpeg", m.ffmpeg, "ffprobe", m.ffprobe, "version", m.version,
		"hwaccel", m.hw.Name, "encoders", m.hw.Encoders, "tempDir", m.tempDir)

	m.wg.Add(1)
	go m.reaper()
	m.startRTXSetup(f.goos)
	return m, nil
}

// staleDownloadAge is how old a remux download file must be before the
// single stale-download policy may remove it. Who deletes what, with no
// overlap and no contradiction:
//
//   - cleanStaleDownloads (this file) is the ONLY boot-time deleter of
//     downloads/: it removes only files matching isRemuxDownloadFile
//     (download.go: dl-*.mp4 and their .part partials), and only once
//     older than staleDownloadAge. It is applied by both cleanStale
//     (boot, via New) and NewDownloadRemuxer (download.go).
//   - NewDownloadRemuxer ADOPTS the fresh dl-* orphans that policy keeps:
//     it registers them in DownloadRemuxer.files/used (real size) with
//     their real mtime, so the disk budget accounts for them and they
//     expire at runtime exactly like adopted files do (see
//     expireStaleAdopted in download.go). It never deletes a fresh dl-*.
//   - At runtime only DownloadRemuxer deletes download files:
//     ReleaseDownload for files a caller still owns, and
//     expireStaleAdopted for adopted orphans that crossed
//     staleDownloadAge. Files this process remuxed itself never expire
//     by age — only ReleaseDownload frees them.
//
// An active remux writes its .part continuously, so its mtime keeps
// advancing; a crash orphan stops advancing and crosses this age. A
// fresh dl-* may therefore still be live (notably one owned by another
// process, or left by a recent restart) and is kept — and adopted, not
// ignored — by both boot callers.
const staleDownloadAge = time.Hour

// cleanStale removes artifacts left by a previous process. It runs only
// from New, before this process has created any session. Only names
// Ether itself creates under tempDir are touched; anything else in a
// shared temp dir is kept:
//
//   - s-* session dirs, including everything nested in them: run-* DASH
//     run dirs, c-* chunk dirs, rung-* rendition dirs and dash chunk-*
//     files. These are removed unconditionally (pre-existing behaviour).
//     That is only sound assuming a single process owns tempDir:
//     cleanStale cannot tell another process's live s-* session sharing
//     the same tempDir from an orphan, so cluster/multi-instance
//     deployments must give each instance a distinct TempDir.
//   - downloads/ offline-download files, via the single stale-download
//     policy in cleanStaleDownloads (same policy NewDownloadRemuxer
//     applies; fresh dl-* files are kept there and adopted by it).
func (m *Manager) cleanStale() {
	entries, err := os.ReadDir(m.tempDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		switch {
		case e.IsDir() && strings.HasPrefix(e.Name(), "s-"):
			path := filepath.Join(m.tempDir, e.Name())
			if _, protected := encoderCleanup.dirs.Load(path); !protected {
				_ = os.RemoveAll(path)
			}
		case e.IsDir() && e.Name() == "downloads":
			cleanStaleDownloads(filepath.Join(m.tempDir, e.Name()))
		}
	}
}

// cleanStaleDownloads is the single stale-download cleanup policy,
// shared by cleanStale (boot, via New) and NewDownloadRemuxer
// (download.go): it removes only the files DownloadRemuxer creates in
// its dir (isRemuxDownloadFile: dl-*.mp4 and their .part partials —
// never e.g. dl-notas.txt), and only once they are older than
// staleDownloadAge; unrelated files sharing the dir, and fresh dl-*
// files, are kept. The fresh dl-* files it keeps are adopted by
// NewDownloadRemuxer (see staleDownloadAge for who deletes what).
func cleanStaleDownloads(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleDownloadAge)
	for _, e := range entries {
		if e.IsDir() || !isRemuxDownloadFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// Version is the ffmpeg version string.
func (m *Manager) Version() string { return m.version }

// FFmpegPath is the ffmpeg executable in use.
func (m *Manager) FFmpegPath() string { return m.ffmpeg }

// FFprobePath is the ffprobe executable in use.
func (m *Manager) FFprobePath() string { return m.ffprobe }

// Capability is the owner-visible transcoder health card: which colour and
// tone-map paths were probed, plus the latest graph failure.
type Capability struct {
	Notice        string   `json:"notice,omitempty"`
	PlaybackError string   `json:"playbackError,omitempty"`
	FiltersProbed bool     `json:"filtersProbed"`
	ToneMap       []string `json:"toneMap,omitempty"`
	Colour        []string `json:"colour,omitempty"`
}

// Capability reports probed filters and any owner-visible limitation.
func (m *Manager) Capability() Capability {
	if m == nil {
		return Capability{}
	}
	m.mu.Lock()
	playback := m.graphErr
	m.mu.Unlock()
	return Capability{
		Notice:        m.limitation(),
		PlaybackError: playback,
		FiltersProbed: m.feat.filtersProbed,
		ToneMap:       toneMapPaths(m.feat, m.hw),
		Colour:        colourPaths(m.feat),
	}
}

func (m *Manager) noteGraphError(err error) {
	if m == nil || err == nil {
		return
	}
	m.mu.Lock()
	m.graphErr = err.Error()
	m.mu.Unlock()
	m.log.Warn("transcode graph unavailable", "err", err)
}

func (m *Manager) limitation() string {
	if m == nil {
		return ""
	}
	var parts []string
	if !m.feat.filtersProbed {
		detail := "FFmpeg's filter list could not be read, so optional filters such as zscale are treated as unavailable."
		if m.feat.probeDetail != "" {
			detail += " " + m.feat.probeDetail
		}
		parts = append(parts, detail)
	}
	if len(toneMapPaths(m.feat, m.hw)) == 0 {
		parts = append(parts, toneMapUnavailableMessage())
	}
	if !m.feat.hasFilter("zscale") && !m.feat.hasFilter("colorspace") {
		parts = append(parts, colourUnavailableMessage())
	}
	return strings.Join(parts, " ")
}

func featureLine(f features) string {
	names := []string{"zscale", "colorspace", "scale", "libplacebo", "tonemap", "tonemap_opencl", "tonemap_vaapi", "tonemap_videotoolbox", "scale_vt", "tonemap_cuda"}
	parts := make([]string, len(names))
	for i, name := range names {
		bit := "0"
		if f.hasFilter(name) {
			bit = "1"
		}
		parts[i] = name + "=" + bit
	}
	return strings.Join(parts, " ")
}

func encoderLine(f features) string {
	names := []string{"libx264", "libx265", "h264_videotoolbox", "hevc_videotoolbox", "h264_nvenc", "hevc_nvenc", "h264_vaapi", "hevc_vaapi", "h264_qsv", "hevc_qsv"}
	var parts []string
	for _, name := range names {
		if f.hasEncoder(name) {
			parts = append(parts, name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func probeState(f features) string {
	if f.filtersProbed && f.probeDetail == "" {
		return "ok"
	}
	if f.probeDetail != "" {
		return f.probeDetail
	}
	return "filters unread"
}

// HWAccel implements core.Transcoder.
func (m *Manager) HWAccel() core.HWAccel {
	hw := m.hw
	hw.Encoders = append([]string(nil), m.hw.Encoders...)
	if len(m.hw.VAAPIQualityRC) > 0 {
		hw.VAAPIQualityRC = make(map[string]string, len(m.hw.VAAPIQualityRC))
		for k, v := range m.hw.VAAPIQualityRC {
			hw.VAAPIQualityRC[k] = v
		}
	}
	if len(m.hw.VAAPIBitrateRC) > 0 {
		hw.VAAPIBitrateRC = make(map[string]string, len(m.hw.VAAPIBitrateRC))
		for k, v := range m.hw.VAAPIBitrateRC {
			hw.VAAPIBitrateRC[k] = v
		}
	}
	return hw
}

// UpdateConfig applies changed limits (MaxTranscodes,
// RemoteMaxBitrateKbps) and schedules optional RTX runtime setup. FFmpeg paths
// and the ordinary hardware encoder choice still need a restart.
func (m *Manager) UpdateConfig(cfg config.Transcode) {
	m.mu.Lock()
	previous := m.cfg.RTXVideo
	m.cfg = cfg
	if m.rtxSetup != nil && previous != cfg.RTXVideo && !m.closed {
		m.rtxSetup.changedLocked()
	}
	m.mu.Unlock()
}

// Policy is the server-side decision input derived from config and
// hardware.
func (m *Manager) Policy() Policy {
	m.mu.Lock()
	remote := m.cfg.RemoteMaxBitrateKbps
	m.mu.Unlock()
	hevcHW := false
	for _, e := range m.hw.Encoders {
		if strings.HasPrefix(e, "hevc_") {
			hevcHW = true
		}
	}
	// libx265 is too slow for real-time 4K/1080p transcodes on most CPUs,
	// so only a hardware HEVC encoder makes HEVC (and kept HDR) a target.
	return Policy{
		RemoteMaxBitrateKbps: remote,
		HEVCEncoder:          hevcHW,
		HEVCHardware:         hevcHW,
	}
}

// Probe implements core.Transcoder.
func (m *Manager) Probe(ctx context.Context, inputURL string) (*core.MediaInfo, error) {
	start := time.Now()
	info, err := probe(ctx, m.runner, m.ffprobe, inputURL)
	if err != nil {
		return nil, err
	}
	m.log.Debug("probed", "input", inputURL, "took", time.Since(start).Round(time.Millisecond),
		"container", info.Container, "duration", info.Duration, "streams", len(info.Streams))
	return info, nil
}

// Decide implements core.Transcoder.
func (m *Manager) Decide(info core.MediaInfo, caps core.ClientCaps, opts core.DecideOptions) core.Decision {
	return m.enhanceDecision(info, caps, opts, Decide(info, caps, opts, m.Policy()))
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// encodingLocked counts the sessions other than except that are running an
// encoder: transcodes, and copy sessions while their fast-start ramp, a
// governor clamp or a lower rung is being encoded. Caller holds m.mu.
func (m *Manager) encodingLocked(except string) int {
	n := 0
	for life := range m.encoders {
		if current := m.sessions[except]; except != "" && current != nil && current.encoder == life {
			continue // planning a replacement; admission still requires old Close
		}
		if life.encoding.Load() || life.runs.Load() > 0 {
			n++
		}
	}
	return n
}

// StartHLS implements core.Transcoder. A replacement cannot begin until the
// prior exact session has completed its process and directory cleanup.
func (m *Manager) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	life, err := m.reserveEncoder()
	if err != nil {
		return nil, err
	}
	return m.startHLS(ctx, req, life)
}

func (m *Manager) startHLS(ctx context.Context, req core.HLSRequest, life *encoderLifetime) (result core.HLSSession, resultErr error) {
	defer life.finishWork()
	defer func() {
		if result == nil {
			life.seal()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.InputURL == "" {
		return nil, errors.New("transcode: StartHLS: empty input URL")
	}
	if req.InputLease != nil && req.LiveInput {
		// Scoped VOD inputs cannot expand a playlist into provider requests
		// carrying the private header. Ordinary live input remains unchanged.
		return nil, core.ErrEncoderInputUnverified
	}
	inputHeaders, headerErr := validatedInputHeaders(req.InputHeaders)
	if headerErr != nil {
		return nil, headerErr
	}
	proto := req.Protocol
	switch proto {
	case "":
		proto = core.ProtocolHLS
	case core.ProtocolHLS, core.ProtocolDASH:
	default:
		return nil, fmt.Errorf("transcode: StartHLS: unknown protocol %q", req.Protocol)
	}
	d := req.Decision
	switch d.Kind {
	case core.DecisionRemux, core.DecisionTranscode:
	case core.DecisionDirectPlay:
		// Asked to stream something directly playable: remux it.
		d.Kind = core.DecisionRemux
		d.VideoCopy, d.AudioCopy = true, true
		d.Container = ""
	case "":
		return nil, errors.New("transcode: StartHLS: decision required")
	default:
		return nil, fmt.Errorf("transcode: StartHLS: unknown decision kind %q", d.Kind)
	}
	if proto == core.ProtocolDASH {
		d = dashDecision(req.Info, d)
	}
	v := req.Info.Video()
	if v == nil {
		d.VideoCopy = true // audio-only: nothing to encode
	}
	if d.VideoCopy && v != nil && d.VideoCodec == "" {
		d.VideoCodec = v.Codec
	}
	if d.AudioStream < 0 {
		if a := pickAudio(req.Info, -1); a != nil {
			d.AudioStream = a.Index
		}
	}
	if a := findStream(req.Info, d.AudioStream, "audio"); a != nil && d.AudioCopy && d.AudioCodec == "" {
		d.AudioCodec = a.Codec
	}
	if proto == core.ProtocolDASH {
		d.Container = ContainerFMP4
	} else if d.Container != ContainerFMP4 && d.Container != ContainerMPEGTS && d.Container != ContainerMatroska {
		d.Container = segmentContainer(d.VideoCodec, d.AudioCodec)
	}
	m.mu.Lock()
	rtxConfig, _ := normalizedRTX(m.cfg.RTXVideo)
	adapt := adaptiveFromConfig(m.cfg)
	if enhancementSelected(d) {
		// Native VSR keeps a steady clock. Lower rungs use ordinary FFmpeg
		// on that same clock; never change the timeline on a VSR fallback.
		adapt.FastStart = false
	}
	atCap := m.cfg.MaxTranscodes > 0 && m.encodingLocked(req.SessionID) >= m.cfg.MaxTranscodes
	m.mu.Unlock()
	if atCap && d.VideoCopy {
		// The encoder cap is reached. A copy session still plays, but
		// plainly: no encoded fast-start ramp, no governor clamps and no
		// lower rungs, each of which would run an encoder past the cap.
		adapt.FastStart, adapt.Governor, adapt.Ladder = false, false, false
		m.log.Debug("transcode cap reached: copy session without encoded ramp, governor or ladder", "session", req.SessionID)
	}
	rungs := buildRungs(req.Info, d, adapt)
	if proto == core.ProtocolHLS && d.Container == ContainerMatroska && len(rungs) > 1 {
		// Plex's native player probes every rendition's initialization and
		// early segments concurrently. These rungs share one encoder, so
		// those probes would repeatedly stop the active run. Advertise the
		// selected quality alone until rungs can produce independently.
		// Explicit Plex quality changes still start a separate decision.
		rungs = rungs[:1]
	}

	if proto == core.ProtocolDASH && len(rungs) > 1 && findStream(req.Info, d.AudioStream, "audio") == nil {
		// Ladder track IDs reserve track 1 for audio.
		rungs = rungs[:1]
	}

	segDur := req.SegmentDuration
	if segDur <= 0 {
		segDur = DefaultSegmentDuration
		if proto == core.ProtocolDASH {
			segDur = DefaultDASHSegmentDuration
			// A copy primary may advertise encoded lower rungs. They share
			// the primary's clock, so give the whole ladder the short grid.
			// Copy GOPs spanning slots keep using the existing aliases.
			if !d.VideoCopy || len(rungs) > 1 {
				segDur = encodedDASHSegmentDuration
			}
		}
	}
	offset := req.StartOffset
	if offset < 0 {
		offset = 0
	}
	if req.Info.Duration > 0 && offset >= req.Info.Duration {
		offset = req.Info.Duration - segDur
		if offset < 0 {
			offset = 0
		}
	}

	// HLS playlists start at the offset; a DASH MPD always spans the whole
	// title and the first run starts at the offset's segment.
	base, startSeg, total := offset, 0, 0
	tracks := 1
	var reps []dashRep
	var plan *Timeline
	if proto == core.ProtocolDASH {
		base = 0
		startSeg = int(offset / segDur)
		total = dashSegmentCount(req.Info.Duration, segDur)
		if total > 0 && startSeg >= total {
			startSeg = total - 1
		}
		reps = dashRepsLadder(req.Info, d, rungs)
		if len(reps) == 0 {
			return nil, errors.New("transcode: StartHLS: no video or audio stream for DASH")
		}
		tracks = len(reps)
	} else {
		// One segment plan for the whole session (all rungs share it).
		span := time.Duration(0)
		if req.Info.Duration > 0 {
			span = req.Info.Duration - base
		}
		steadyMode := ModeTranscode
		if d.VideoCopy {
			steadyMode = ModeCopy
		}
		// Pure copy output cannot force the short opening GOPs. Chaining a
		// run at the nominal ramp boundary can seek back to an earlier source
		// keyframe and replay it. TS ramps encode their opening GOPs; other
		// copy containers keep one continuous run.
		//
		// Remux + audio transcode is the exception: one continuous copy run
		// at FastStartSegment (default 2s) so the first muxed A+V segment
		// does not wait for 4s of EAC3. Playlist is uniformly shorter —
		// an explained Plex-visible difference vs a 4s remux. Pure copy
		// remux (audio copies too) stays on 4s so long-GOP MKV does not
		// restart ffmpeg and replay frames.
		if req.SegmentDuration <= 0 {
			if short := remuxOpenSegment(adapt, d, v); short > 0 {
				segDur = short
			}
		}
		fastStart := adapt.FastStart && v != nil && (!d.VideoCopy || d.Container == ContainerMPEGTS)
		plan = buildTimeline(span, segDur, timelineOptions{
			steadyMode: steadyMode,
			fastStart:  fastStart,
			rampSegs:   adapt.RampSegs,
			rampDur:    adapt.RampDur,
			keyframes:  req.Info.Keyframes,
			base:       base,
			snap:       d.VideoCopy && len(req.Info.Keyframes) > 0,
		})
		total = plan.Total()
		tracks = len(rungs) // one segment track per rung
	}
	id := req.SessionID
	if id == "" {
		id = randomHex(12)
	}

	if req.InputLease != nil {
		if err := life.installInput(req.InputLease); err != nil {
			return nil, err
		}
	}
	if err := m.claimEncoderID(ctx, id, life); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("transcode: manager closed")
	}
	if !d.VideoCopy && m.cfg.MaxTranscodes > 0 {
		if n := m.encodingLocked(""); n >= m.cfg.MaxTranscodes {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w (%d running)", ErrTooManyTranscodes, n)
		}
	}
	life.encoding.Store(!d.VideoCopy)
	m.mu.Unlock()

	safe := unsafeID.ReplaceAllString(id, "_")
	if len(safe) > 48 {
		safe = safe[:48]
	}
	dir := filepath.Join(m.tempDir, "s-"+safe+"-"+randomHex(4))
	s := &Session{
		encoder:                 life,
		id:                      id,
		m:                       m,
		dir:                     dir,
		segDur:                  segDur,
		base:                    base,
		total:                   total,
		container:               d.Container,
		proto:                   proto,
		requireIndependentProof: req.RequireIndependentProof,
		log:                     m.log.With("session", id),
		created:                 time.Now(),
		sessionInput: sessionInput{
			rtxTimeline:  enhancementSelected(d),
			rtxConfig:    rtxConfig,
			liveInput:    req.LiveInput,
			input:        req.InputURL,
			inputHeaders: inputHeaders,
			inputLease:   req.InputLease,
			info:         req.Info,
			decision:     d,
		},
		sessionEncoding: sessionEncoding{
			endSeg:   -1,
			resumeAt: -1,
		},
		sessionDelivery: sessionDelivery{
			done:    make([]map[int]bool, tracks),
			lastReq: startSeg,
			changed: make(chan struct{}),
		},
		sessionAdaptation: sessionAdaptation{
			plan:        plan,
			adapt:       adapt,
			rungs:       rungs,
			initFiles:   make([]string, tracks),
			seekRampSeg: -1,
			segSizes:    map[int]int64{},
			governorOn:  adapt.Governor && proto == core.ProtocolHLS && d.VideoCopy && v != nil,
		},
	}
	if !life.attach(s) {
		return nil, errors.New("transcode: session closed")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: session dir: %w", err)
	}
	for t := range s.done {
		s.done[t] = map[int]bool{}
	}
	if proto == core.ProtocolHLS && d.Container == ContainerMatroska {
		// Plex keeps the first EXT-X-MAP header for the whole session.
		if c, ok := contractOf(req.Info, d); ok {
			s.contract = c
			s.log.Info("static header output contract", "video", c.String())
		}
	}
	if proto == core.ProtocolDASH {
		s.dash = newDASHState(reps, m.opts.dashURLs, id)
	}
	s.Touch()
	s.log.Info("transcode session starting", "protocol", proto, "kind", d.Kind, "container", d.Container,
		"offset", offset, "segment", startSeg, "segments", s.total, "reason", d.Reason)

	s.mu.Lock()
	err := s.startRunForLocked(startSeg, false, "session start")
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	life.mu.Lock()
	m.mu.Lock()
	if m.closed || ctx.Err() != nil || life.sealed || m.encoderIDs[id] != life {
		m.mu.Unlock()
		life.mu.Unlock()
		return nil, errors.New("transcode: manager closed or start canceled")
	}
	m.sessions[id] = s
	m.mu.Unlock()
	life.mu.Unlock()
	return s, nil
}

// Session implements core.Transcoder.
func (m *Manager) Session(id string) (core.HLSSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	return s, true
}

// Sessions lists the live sessions (admin dashboard).
func (m *Manager) Sessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// Stop implements core.Transcoder.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// forget unregisters a closed session (if it is still the registered one).
func (m *Manager) forget(s *Session) {
	m.mu.Lock()
	if m.sessions[s.id] == s {
		delete(m.sessions, s.id)
	}
	m.mu.Unlock()
}

func (m *Manager) reaper() {
	defer m.wg.Done()
	defer close(m.reaperDone)
	every := m.opts.idleTimeout / 4
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	if every < 50*time.Millisecond {
		every = 50 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.reapIdle(time.Now())
		}
	}
}

func (m *Manager) reapIdle(now time.Time) {
	if m.opts.idleTimeout <= 0 {
		return
	}
	for _, s := range m.Sessions() {
		if idle := now.Sub(s.LastAccess()); idle > m.opts.idleTimeout && now.UnixNano() >= s.pauseUntil.Load() {
			s.log.Info("closing idle session", "idle", idle.Round(time.Second))
			_ = s.Close()
		}
	}
}

// Close stops every session and the reaper.
func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		if m.rtxSetup != nil {
			m.rtxSetup.cancel()
		}
		close(m.stop)
	}
	lives := make([]*encoderLifetime, 0, len(m.encoders))
	for life := range m.encoders {
		lives = append(lives, life)
	}
	reaperDone := m.reaperDone
	var setupDone <-chan struct{}
	if m.rtxSetup != nil {
		setupDone = m.rtxSetup.done
	}
	m.mu.Unlock()
	for _, life := range lives {
		life.seal()
	}
	timer := time.NewTimer(m.encoderWait())
	defer timer.Stop()
	var err error
	wait := func(done <-chan struct{}) bool {
		if done == nil {
			return true
		}
		select {
		case <-done:
			return true
		case <-timer.C:
			return false
		}
	}
	for _, life := range lives {
		if !wait(life.done) {
			return errors.Join(err, core.ErrEncoderPending)
		}
		err = errors.Join(err, life.legacyResult())
	}
	if !wait(reaperDone) || !wait(setupDone) {
		return errors.Join(err, core.ErrEncoderPending)
	}
	return err
}

// ExtractSubtitle writes an embedded text subtitle stream as WebVTT. It
// reads the whole input (subtitle packets are interleaved through it).
func (m *Manager) ExtractSubtitle(ctx context.Context, inputURL string, streamIndex int, codec string, w io.Writer) error {
	if codec != "" && IsImageSubtitle(codec) {
		return ErrImageSubtitle
	}
	if source, _ := ctx.Value(subtitleSourceKey{}).(string); source != "" {
		data, err := m.subtitles.extract(ctx, subtitleCacheKey(source, codec, streamIndex), func(ctx context.Context, buf *bytes.Buffer) error {
			return m.extractSubtitle(ctx, inputURL, streamIndex, buf)
		})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	return m.extractSubtitle(ctx, inputURL, streamIndex, w)
}

func (m *Manager) extractSubtitle(ctx context.Context, inputURL string, streamIndex int, w io.Writer) error {
	var stderr tailWriter
	err := m.runner.Run(ctx, Command{Path: m.ffmpeg, Args: subtitleArgs(inputURL, streamIndex), Stdout: w, Stderr: &stderr})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("transcode: extract subtitle %d: %w: %s", streamIndex, err, tail(stderr.String(), 300))
	}
	return nil
}

// tailWriter keeps the last 4 KiB written.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailWriter) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(b), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
