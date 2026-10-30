package playback

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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
	observation  core.PlaybackSourceObservation
	reads        pendingSourceReads
	standby      standbyState
	recovery     recoveryState
	damage       damageState
	sess         *session
	n            int
	token        string // loopback path component; unguessable
	src          core.MediaSource
	finishSource func(error)
	res          *core.Resolution
	info         core.MediaInfo
	tmap         TimeMap

	// indexReady is closed once the container keyframe index has been
	// read (or given up on); the prewarm path waits for it, Play does not.
	// ctx is cancelled on retire so a background index fill stops with
	// the generation instead of holding its source open.
	indexReady chan struct{}
	indexOnce  sync.Once
	ctx        context.Context
	cancel     context.CancelFunc

	// articleMissing records that a read hit core.ErrArticleMissing.
	articleMissing     atomic.Bool
	backgroundFailover atomic.Bool // at most one recovery worker for this source
	readFailure        error       // protected by mu; distinguishes transient failover from bad data

	mu      sync.Mutex
	refs    int
	retired bool
	closed  bool
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
	if g.src != nil {
		err := g.src.Close()
		if g.finishSource != nil {
			g.finishSource(err)
		}
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
	info := g.info
	info.Keyframes = append([]core.KeyframePoint(nil), g.info.Keyframes...)
	return info
}

// timeMapSnapshot copies the generation's TimeMap under its lock.
func (g *generation) timeMapSnapshot() TimeMap {
	g.mu.Lock()
	defer g.mu.Unlock()
	tm := g.tmap
	tm.Points = append([]IndexPoint(nil), g.tmap.Points...)
	return tm
}

func (g *generation) releaseID() string {
	if g.res == nil {
		return ""
	}
	return g.res.Release.ID
}

// session is the internal, mutable form of a core.PlaySession.
type session struct {
	sources        *sourceLifetime
	reserve        reserveState
	viewerPosition time.Duration // guarded by mu, from authenticated Plex timeline
	viewerAt       time.Time
	viewerState    string
	warm           warmState // the encoder input range being prefetched
	playbackGroup  string
	id             string
	ident          identity
	user           core.User
	caps           core.ClientCaps
	title          *core.Title
	titleName      string
	profileKey     string // profileKey of the resolving profile
	startedAt      time.Time

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
	stopCtx    context.Context
	stopCancel context.CancelFunc
	failSem    chan struct{} // serialises failovers
	bytes      atomic.Int64
	rate       rateMeter

	mu             sync.Mutex
	preparedOnly   bool
	sourcePins     int
	cur            *generation
	virtualGen     *generation // generation whose layout virtual mirrors
	gens           int
	tried          []string
	alternates     []core.Release
	failovers      int
	downFailovers  int
	requiresRemux  bool
	prefer         string // release ID the viewer picked (PrepareVariant); read-only after creation
	exact          bool   // exact selection must never switch releases
	lastActive     time.Time
	stopAt         time.Time // set by a "stopped" timeline; expiry after a grace
	decision       core.DecisionKind
	decisionReason string
	explicit       bool // decision set by SetDecision
	closed         bool
	registered     bool

	// Speed-coupled down-failover state (guarded by speedMu).
	speedMu    sync.Mutex
	speed      speedTracker
	genSince   time.Time // when the current generation started serving
	graceUntil time.Time // slow-source checks are ignored before this
	slowChecks int       // consecutive slow evaluations
	lastSrcPos int64     // latest source read position on the current generation
	expectPos  int64     // next sequential read offset (-1 = unknown)
}

func newSession(id string, ident identity, user *core.User, caps core.ClientCaps, title *core.Title, profileKey string, now time.Time) *session {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	return &session{
		id:         id,
		sources:    newSourceLifetime(user.ID, string(caps.Protocol), caps.ClientID),
		stopCtx:    stopCtx,
		stopCancel: stopCancel,
		ident:      ident,
		user:       *user,
		caps:       caps,
		title:      title,
		titleName:  displayTitle(title),
		profileKey: profileKey,
		startedAt:  now,
		lastActive: now,
		failSem:    make(chan struct{}, 1),
		rate:       rateMeter{},
		genSince:   now,
		graceUntil: now,
		expectPos:  -1,
	}
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
	s.contentType = contentTypeFor(info.Container, s.name)
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

func (s *session) touch(now time.Time) {
	s.mu.Lock()
	s.lastActive = now
	s.stopAt = time.Time{}
	s.mu.Unlock()
}

func (s *session) markDirectPlay() {
	s.mu.Lock()
	if !s.explicit {
		s.decision = core.DecisionDirectPlay
	}
	s.mu.Unlock()
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
	ps := &core.PlaySession{
		PreparedOnly:   s.preparedOnly,
		ID:             s.id,
		UserID:         s.ident.userID,
		ClientID:       s.ident.clientID,
		PlaybackGroup:  s.playbackGroup,
		Key:            s.ident.key,
		StartedAt:      s.startedAt,
		TitleName:      s.titleName,
		Username:       s.user.Username,
		Client:         s.caps,
		Decision:       s.decision,
		DecisionReason: s.decisionReason,
		BytesServed:    s.bytes.Load(),
		SpeedBps:       s.rate.rate(now),
		LastActive:     s.lastActive,
		Failovers:      s.failovers,
		DownFailovers:  s.downFailovers,
		RequiresRemux:  s.requiresRemux,
	}
	if !s.viewerAt.IsZero() {
		offset := s.viewerPosition.Milliseconds()
		ps.ViewOffset = &offset
	}
	if s.user.Username == "" {
		ps.Username = s.user.Title
	}
	if g := s.cur; g != nil {
		res := *g.res
		ps.Resolution = &res
		ps.Source = g.src
		ps.Info = g.infoSnapshot()
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
	if g.readFailure == nil {
		g.readFailure = err
	}
}
func (g *generation) sourceFailure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.readFailure
}
