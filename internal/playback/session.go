package playback

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// identity is what makes two Prepare calls the same playback.
type identity struct {
	userID   int64
	clientID string
	key      core.RatingKey
	surface  string
	variant  string
	exact    bool
}

// generation is one opened source of a session. A failover creates a new
// generation; the old one is retired and closed once in-flight reads end.
type generation struct {
	warmReads            *core.WarmReadWindow
	prewarmReads         *core.WarmReadWindow
	backgroundReadBytes  atomic.Int64
	backgroundReadErrors atomic.Int64
	backgroundRequests   atomic.Int64
	indexedMu            sync.Mutex
	indexedHeader        []byte
	observation          core.PlaybackSourceObservation
	reads                pendingSourceReads
	standby              standbyState
	recovery             recoveryState
	damage               damageState
	sess                 *session
	n                    int
	token                string // loopback path component; unguessable
	src                  core.MediaSource
	share                core.MediaSource // refcount wrapper; nil when src is the backend
	sharedClose          func() error
	finishSource         func(error)
	res                  *core.Resolution
	info                 core.MediaInfo
	tmap                 TimeMap

	// indexReady is closed once the container keyframe index has been
	// read (or given up on); the prewarm path waits for it, Play does not.
	// ctx is cancelled on retire so a background index fill stops with
	// the generation instead of holding its source open.
	indexReady chan struct{}
	indexOnce  sync.Once
	ctx        context.Context
	cancel     context.CancelFunc

	downloadFailure sync.Once // marks the release unhealthy once per generation
	// articleMissing records that a read hit core.ErrArticleMissing.
	articleMissing     atomic.Bool
	backgroundFailover atomic.Bool // at most one recovery worker for this source
	originalRejected   bool        // guarded by the failover semaphore
	readFailure        error       // protected by mu; distinguishes transient failover from bad data
	retryEpoch         uint64
	retryPending       bool

	mu          sync.Mutex
	needIndex   bool
	needTrailer bool
	refs        int
	retired     bool
	closed      bool
}

// acquire takes a read reference; false once the generation is retired.
func (g *generation) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return false
	}
	g.refs++
	return true
}

func (g *generation) release() {
	g.mu.Lock()
	g.refs--
	closeNow := g.retired && g.refs == 0 && !g.closed
	if closeNow {
		g.closed = true
	}
	g.mu.Unlock()
	if closeNow {
		g.closeSource()
	}
}

// retire stops new reads and closes the source when the last read ends.
func (g *generation) retire() {
	if g.cancel != nil {
		g.cancel()
	}
	g.mu.Lock()
	if g.retired {
		g.mu.Unlock()
		return
	}
	g.retired = true
	closeNow := g.refs == 0 && !g.closed
	if closeNow {
		g.closed = true
	}
	g.mu.Unlock()
	if closeNow {
		g.closeSource()
	}
}

func (g *generation) closeSource() {
	if g.src == nil && g.sharedClose == nil {
		return
	}
	var err error
	if g.sharedClose != nil {
		err = g.sharedClose()
	} else if g.src != nil {
		err = g.src.Close()
	}
	if g.finishSource != nil {
		g.finishSource(err)
	}
}

// markIndexReady records that no background index fill is outstanding.
func (g *generation) markIndexReady() {
	g.indexOnce.Do(func() {
		if g.indexReady != nil {
			close(g.indexReady)
		}
	})
}

// setIndex installs keyframe points read in the background, updating the
// generation's TimeMap and published MediaInfo atomically.
func (g *generation) setIndex(points []IndexPoint) {
	g.mu.Lock()
	g.tmap.Points = points
	g.info.Keyframes = keyframesOf(points)
	g.mu.Unlock()
}

// infoSnapshot copies the generation's info under its lock (the background
// index fill may still be updating Keyframes).
func (g *generation) infoSnapshot() core.MediaInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return core.CloneMediaInfo(g.info)
}

// publishInfo takes a detached copy before exposing probe/augmentation facts.
// The producer owns info exclusively during this call; retained inputs may be
// mutated after it returns. Later generation updates and snapshots use g.mu.
func (g *generation) publishInfo(info core.MediaInfo, observation core.PlaybackSourceObservation, tmap TimeMap) {
	info = core.CloneMediaInfo(info)
	if observation.HDR != nil {
		hdr := *observation.HDR
		observation.HDR = &hdr
	}
	tmap.Points = slices.Clone(tmap.Points)
	g.mu.Lock()
	g.info = info
	g.observation = observation
	g.tmap = tmap
	g.mu.Unlock()
}

// timeMapSnapshot copies the generation's TimeMap under its lock.
func (g *generation) timeMapSnapshot() TimeMap {
	g.mu.Lock()
	defer g.mu.Unlock()
	tm := g.tmap
	tm.Points = append([]IndexPoint(nil), g.tmap.Points...)
	return tm
}

// resetTriedForNewPlayLocked starts a new play's recovery chain after a pause
// or stop. A preferred release the session tried, or fell back from, stays
// tried, so the running session never returns to it. Callers hold s.mu.
func (s *session) resetTriedForNewPlayLocked() {
	if s.preference != "" && slices.Contains(s.tried, s.preference) {
		s.preferenceFailed = true
	}
	s.tried = nil
	if s.preferenceFailed && s.preference != "" {
		s.tried = []string{s.preference}
	}
}

func (g *generation) releaseID() string {
	if g.res == nil {
		return ""
	}
	return g.res.Release.ID
}

// session is the internal, mutable form of a core.PlaySession.
type session struct {
	offlineOriginal func(core.MediaInfo) bool
	// alias is the identity a promoted preparation also answers for in
	// byIdent (guarded by Manager.mu); ident itself never changes.
	alias                 identity
	hasAlias              bool
	sources               *sourceLifetime
	reserve               reserveState
	viewerPosition        time.Duration // guarded by mu, from authenticated Plex timeline
	viewerAt              time.Time
	viewerState           string
	choiceStartSet        bool            // guarded by mu; tracks real-play duration on an explicit version
	choiceLastPos         time.Duration   // guarded by mu
	choicePlayed          time.Duration   // guarded by mu
	choiceRecorded        bool            // guarded by mu
	exp                   experienceState // guarded by mu
	stallObserved         int             // guarded by mu; timeline stalls evaluated against delivery
	timelineEnabled       bool
	timelineStalls        int
	timelineDecided       bool
	timelineDecision      core.DecisionKind
	timelineReason        string
	timelineBitrate       int
	timelineHeight        int
	encoderRecoveryReason string
	encoderRunway         *time.Duration
	warm                  warmState // the encoder input range being prefetched
	playbackGroup         string
	id                    string
	ident                 identity
	user                  core.User
	caps                  core.ClientCaps
	noVideoTranscode      atomic.Int32 // 0=from caps, 1=off, 2=on
	title                 *core.Title
	titleName             string
	profileKey            string // profileKey of the resolving profile
	originalLanguage      string // catalogue language when this session was selected
	startedAt             time.Time
	// kindOpen opens a non-catalogue plugin item's bytes (spec §15.1). When
	// set, the session bypasses resolution/failover and opens this source
	// directly, then runs the normal probe/decision pipeline.
	kindOpen          func(ctx context.Context) (core.MediaSource, error)
	kindOpenCandidate func(ctx context.Context, exclude []string) (core.MediaSource, core.Release, error)
	kindSourceID      string

	// virtual is the byte/time layout of the first generation: the file the
	// client was told about. Direct-play byte offsets are always in these
	// coordinates, even after a failover swapped the underlying release.
	virtual     TimeMap
	etag        string
	modTime     time.Time
	contentType string
	container   string
	name        string

	// stopCtx cancels background recovery when playback ends. Unlike a
	// generation context, it survives retiring an input during a healthy swap.
	stopCtx        context.Context
	stopCancel     context.CancelFunc
	failSem        chan struct{} // serialises failovers
	failingOver    atomic.Bool   // true while failoverWith holds failSem
	bytes          atomic.Int64
	mediaBytes     atomic.Int64 // client-facing media bytes delivered (excludes ffmpeg loopback / background probes)
	directRangeEnd atomic.Int64 // highest delivered byte offset on direct-play media ranges (-1 = none yet)
	activeReads    atomic.Int32
	rate           rateMeter

	// claimed (guarded by Manager.mu): a caller took the session over
	// after its flight completed (see abandonFlightLocked).
	claimed bool

	mu               sync.Mutex
	preparedOnly     bool
	backgroundHold   *backgroundSourceHold
	analysisSource   bool
	stoppedBySurface bool
	sourcePins       int

	originalRecoveryTried bool
	originalRevision      string

	cur                *generation
	virtualGen         *generation // generation whose layout virtual mirrors
	gens               int
	tried              []string
	preference         string // owner's preferred release when the session opened
	preferenceFailed   bool   // the session tried the preference and fell back; survives unpause
	alternates         []core.Release
	altVersionSelector string // slot selector carried by alternates when a slot pick fails over
	altQualityFallback string // fallback note carried by alternates when a stepped-down pick fails over
	failovers          int
	downFailovers      int
	// downMisses counts speed-driven down-failovers that found no
	// lower-bitrate release. They count toward MaxDownFailovers so a
	// source that stays too slow cannot retry forever.
	downMisses int
	// stallContinues counts times a stall-triggered failover continued
	// this release because every alternate failed fast — the shape of a
	// stalled CDN edge rather than an unhealthy release (bounded by
	// maxStallContinues). The grace is never replenished: one continue
	// total per playback session, even after a fully recovered read or a
	// later generation swap, so a long session's second independent edge
	// glitch escalates as before. A per-outage reset would need sustained
	// healthy progress plus a cumulative starvation cap (issue #169), not
	// merely a new request. Guarded by mu; see takeStallContinue.
	stallContinues   int
	requiresRemux    bool
	prefer           string             // release ID the viewer picked (PrepareVariant); read-only after creation
	exact            bool               // exact selection must never switch releases
	binding          core.SourceBinding // why an exact session holds its release: which failures may still replace it
	lastActive       time.Time
	lastRead         time.Time
	lastMediaAt      time.Time // last client-facing media bytes / segment / direct redirect served (zero until media delivered)
	directFillUntil  time.Time // active initial/post-seek buffer-fill window for direct-play upload-budget metering
	openedAt         time.Time // last Prepare/MarkSurfaceOpened time for StartupGrace
	pausedAt         time.Time // set when viewerState enters "paused"
	stopAt           time.Time // set by a "stopped" timeline; expiry after a grace
	decision         core.DecisionKind
	decisionReason   string
	deliveryDecision *core.Decision
	// Whether the current decision copies each stream; a remux with
	// decisionAudioCopy false is transcoding only the audio.
	decisionVideoCopy bool
	decisionAudioCopy bool
	explicit          bool // decision set by SetDecision
	closed            bool
	registered        bool

	// Marker enter/exit tracking for playback.marker.* events (guarded by mu).
	lastProgressAt    time.Time
	markersLoaded     bool
	markersCache      []MarkerSpan
	markersDuration   time.Duration
	markersFirstCheck time.Time
	activeMarkerKind  string
	activeMarkerStart int64
	activeMarkerEnd   int64

	// Speed-coupled down-failover state (guarded by speedMu).
	speedMu           sync.Mutex
	speedSeekAt       time.Time
	speed             speedTracker
	delivery          deliveryTracker
	genSince          time.Time     // when the current generation started serving
	graceUntil        time.Time     // slow-source checks are ignored before this
	gracePeakBuffer   time.Duration // highest known buffer observed in current grace window
	slowChecks        int           // consecutive slow evaluations
	encoderSampleAt   time.Time
	encoderRunAt      time.Time
	encoderSeekAt     time.Time
	encoderSlowChecks int
	encoderRetryAt    time.Time
	encoderMisses     int
	lastSrcPos        int64 // latest source read position on the current generation
	expectPos         int64 // next sequential read offset (-1 = unknown)
}

func newSession(id string, ident identity, user *core.User, caps core.ClientCaps, title *core.Title, profileKey string, now time.Time) *session {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	s := &session{
		id:               id,
		sources:          newSourceLifetime(user.ID, string(caps.Protocol), caps.ClientID),
		stopCtx:          stopCtx,
		stopCancel:       stopCancel,
		ident:            ident,
		user:             *user,
		caps:             caps,
		title:            title,
		titleName:        displayTitle(title),
		profileKey:       profileKey,
		originalLanguage: titleLanguage(title),
		startedAt:        now,
		openedAt:         now,
		lastActive:       now,
		failSem:          make(chan struct{}, 1),
		rate:             rateMeter{},
		genSince:         now,
		graceUntil:       now,
		expectPos:        -1,
	}
	s.directRangeEnd.Store(-1)
	s.setNoVideoTranscode(caps.NoVideoTranscode)
	return s
}

func (s *session) noVideoTranscodeActive() bool {
	if s == nil {
		return false
	}
	switch s.noVideoTranscode.Load() {
	case 1:
		return false
	case 2:
		return true
	default:
		return s.caps.NoVideoTranscode
	}
}

func (s *session) setNoVideoTranscode(off bool) {
	if off {
		s.noVideoTranscode.Store(2)
	} else {
		s.noVideoTranscode.Store(1)
	}
}

func (s *session) isKind() bool {
	return s != nil && (s.kindOpen != nil || s.kindOpenCandidate != nil)
}

// adopt makes g the session's first generation and fixes the virtual file.
func (s *session) adopt(g *generation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = g
	s.virtualGen = g
	s.gens = g.n + 1
	s.alternates = g.res.Alternates
	s.virtual = g.timeMapSnapshot()
	info := g.infoSnapshot()
	s.container = info.Container
	s.name = g.src.Name()
	s.contentType = contentTypeForInfo(info, s.name, sourceMIME(g.src))
	sum := sha1.Sum(fmt.Appendf(nil, "%s:%d", g.releaseID(), s.virtual.Size))
	s.etag = `"` + hex.EncodeToString(sum[:10]) + `"`
	s.modTime = g.res.Release.PubDate
	if s.modTime.IsZero() {
		s.modTime = s.startedAt
	}
	s.modTime = s.modTime.UTC().Truncate(time.Second)
}

func (s *session) current() *generation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *session) nextGen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.gens
	s.gens++
	return n
}

func (s *session) isRegistered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registered
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// invalidateDirectRetryLocked disarms a stalled range's watchdog when the
// viewer leaves playback. A pause is not evidence of a broken source.
// The caller holds s.mu (session locks precede generation locks).
func (s *session) invalidateDirectRetryLocked() {
	if s.cur != nil {
		s.cur.mu.Lock()
		s.cur.retryEpoch++
		s.cur.retryPending = false
		s.cur.mu.Unlock()
	}
}

func (s *session) setViewerStateLocked(state string, now time.Time) {
	if state == "paused" {
		if s.viewerState != "paused" || s.pausedAt.IsZero() {
			s.pausedAt = now
		}
	} else {
		s.pausedAt = time.Time{}
	}
	s.viewerState = state
}

func (s *session) touch(now time.Time) {
	s.mu.Lock()
	s.lastActive = now
	s.openedAt = now
	s.stopAt = time.Time{}
	s.stoppedBySurface = false
	if s.viewerState == "stopped" {
		s.setViewerStateLocked("", now)
	}
	s.mu.Unlock()
}

// touchMedia records client-facing media delivery. Unlike touch it must not
// clear a client stop: a player that already reported "stopped" can keep
// draining segments or an open range, and those reads are not the viewer
// coming back (#410). Only a new play (touch) or a playing/paused timeline
// (touchTimeline) cancels a stop.
func (s *session) touchMedia(now time.Time) {
	s.mu.Lock()
	s.lastActive = now
	s.lastRead = now
	s.lastMediaAt = now
	s.mu.Unlock()
}

func (s *session) recordMediaBytes(n int, now time.Time) {
	if n <= 0 {
		return
	}
	s.mediaBytes.Add(int64(n))
	s.touchMedia(now)
}

func (s *session) touchTimeline(now time.Time) {
	s.mu.Lock()
	s.lastActive = now
	s.lastProgressAt = now
	s.stopAt = time.Time{}
	if s.viewerState == "stopped" {
		s.setViewerStateLocked("", now)
	}
	s.mu.Unlock()
}

func (s *session) markDirectPlay() bool {
	s.mu.Lock()
	first := false
	if !s.explicit {
		hasVideo := s.cur == nil || len(s.cur.info.Streams) == 0 || s.cur.info.Video() != nil
		s.decision = core.DecisionDirectPlay
		s.decisionVideoCopy, s.decisionAudioCopy = hasVideo, true
		if !s.timelineDecided {
			s.timelineDecided = true
			s.timelineDecision = core.DecisionDirectPlay
			first = true
		}
	}
	s.mu.Unlock()
	return first
}

func (s *session) addBytes(n int, now time.Time) {
	if n <= 0 {
		return
	}
	s.bytes.Add(int64(n))
	s.rate.add(int64(n), now)
}

// toSource maps a virtual (client-facing) offset into generation g.
func (s *session) toSource(g *generation, v int64) int64 {
	s.mu.Lock()
	virtual := s.virtual
	s.mu.Unlock()
	if g.n == 0 {
		return v
	}
	return MapOffset(v, virtual, g.timeMapSnapshot())
}

func (s *session) snapshot(now time.Time) *core.PlaySession {
	s.mu.Lock()
	defer s.mu.Unlock()
	caps := s.caps
	caps.NoVideoTranscode = s.noVideoTranscodeActive()
	ps := &core.PlaySession{
		PreparedOnly:      s.preparedOnly,
		OriginalRevision:  s.originalRevision,
		ID:                s.id,
		UserID:            s.ident.userID,
		ClientID:          s.ident.clientID,
		PlaybackGroup:     s.playbackGroup,
		Key:               s.ident.key,
		StartedAt:         s.startedAt,
		TitleName:         s.titleName,
		Username:          s.user.Username,
		Client:            caps,
		Decision:          s.decision,
		DeliveryDecision:  s.deliveryDecision,
		DecisionReason:    s.decisionReason,
		DecisionVideoCopy: s.decisionVideoCopy,
		DecisionAudioCopy: s.decisionAudioCopy,
		BytesServed:       s.bytes.Load(),
		SpeedBps:          s.rate.rate(now),
		LastActive:        s.lastActive,
		ViewerState:       s.viewerState,
		Failovers:         s.failovers,
		DownFailovers:     s.downFailovers,
		RequiresRemux:     s.requiresRemux,
	}
	if !s.viewerAt.IsZero() {
		offset := s.viewerPosition.Milliseconds()
		ps.ViewOffset = &offset
	}
	if !s.caps.NoPlaybackTelemetry {
		ps.Experience = s.exp.snapshot(now)
	}
	if s.user.Username == "" {
		ps.Username = s.user.Title
	}
	if g := s.cur; g != nil {
		res := *g.res
		ps.Resolution = &res
		ps.QualityFallback = res.QualityFallback
		ps.Source = g.src
		ps.Info = g.infoSnapshot()
		ps.SegmentStart = segmentStart(s.title, res.Release, ps.Info.Duration)
		ps.Origin = sourceOrigin(g.src, res.Release.Protocol)
	}
	return ps
}

// sourceOrigin names the service a generation streams from.
func sourceOrigin(src core.MediaSource, protocol string) string {
	if o, ok := src.(core.OriginSource); ok {
		return o.Origin()
	}
	if protocol == core.ProtocolTorrent {
		return "Debrid"
	}
	if protocol == core.ProtocolHTTP {
		return "HTTP"
	}
	return "Usenet"
}

func displayTitle(t *core.Title) string {
	if t == nil {
		return ""
	}
	switch {
	case t.MediaRole == core.MediaRoleTrack || t.MBItemType == "Audio" && t.MediaClass == core.MediaClassMusic:
		artist := playbackCredit(t, "artist")
		if artist == "" {
			for _, parent := range t.MediaParents {
				if parent.Role == core.MediaRoleArtist && parent.Title != "" {
					artist = parent.Title
					break
				}
			}
		}
		if artist == "" && !t.SelfChild {
			artist = t.ShowTitle
		}
		if artist != "" && artist != t.Title {
			return artist + " – " + t.Title
		}
		return t.Title
	case t.MediaClass == core.MediaClassBook || t.MediaClass == core.MediaClassAudiobook || t.MBItemType == "Book" || t.MBItemType == "AudioBook":
		if t.MediaRole == core.MediaRolePart && t.ParentTitle != "" && t.ParentTitle != t.Title {
			return t.ParentTitle + " – " + t.Title
		}
		if author := playbackCredit(t, "author"); author != "" {
			return author + " – " + t.Title
		}
		return t.Title
	}
	if t.Type == core.TypeEpisode {
		show := t.ShowTitle
		if show == "" {
			show = "?"
		}
		return fmt.Sprintf("%s - S%02dE%02d - %s", show, t.Season, t.Episode, t.Title)
	}
	if t.Year > 0 {
		return fmt.Sprintf("%s (%d)", t.Title, t.Year)
	}
	return t.Title
}

func playbackCredit(t *core.Title, role string) string {
	var names []string
	for _, credit := range t.MediaCredits {
		if credit.Role == role && credit.Name != "" {
			names = append(names, credit.Name)
		}
	}
	return strings.Join(names, ", ")
}

// lock acquires a channel semaphore, honouring ctx.
func lock(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unlock(sem chan struct{}) { <-sem }

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("playback: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// rateMeter tracks bytes per second over a short sliding window.
type rateMeter struct {
	mu      sync.Mutex
	buckets [rateWindow + 1]int64
	secs    [rateWindow + 1]int64
}

const rateWindow = 5 // seconds

func (r *rateMeter) add(n int64, now time.Time) {
	sec := now.Unix()
	i := int(sec % int64(len(r.buckets)))
	r.mu.Lock()
	if r.secs[i] != sec {
		r.secs[i], r.buckets[i] = sec, 0
	}
	r.buckets[i] += n
	r.mu.Unlock()
}

// rate is the average over the last rateWindow complete seconds, in bytes
// per second.
func (r *rateMeter) rate(now time.Time) int64 {
	sec := now.Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	var sum int64
	for i := range r.buckets {
		if d := sec - r.secs[i]; d >= 1 && d <= rateWindow {
			sum += r.buckets[i]
		}
	}
	return sum / rateWindow
}

func (g *generation) noteReadFailure(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.readFailure == nil || errors.Is(err, core.ErrOriginalLost) {
		g.readFailure = err
	}
}

func (s *session) setAlternateMeta(versionSelector, qualityFallback string) {
	s.mu.Lock()
	s.altVersionSelector = versionSelector
	s.altQualityFallback = qualityFallback
	s.mu.Unlock()
}

func (s *session) alternateMeta() (string, string) {
	s.mu.Lock()
	sel, qf := s.altVersionSelector, s.altQualityFallback
	s.mu.Unlock()
	return sel, qf
}
func (g *generation) sourceFailure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readFailure
}
