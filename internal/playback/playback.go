// Package playback implements core.Playback: it binds a user, a client and a
// title to a resolved, open release, enforces stream limits, serves the
// source to clients (direct play) and to ffmpeg (a loopback HTTP server),
// fails over to another release mid-stream when articles go missing, and
// protects the releases of live and in-progress titles in the segment cache.
//
// Sessions are internal and mutable; every PlaySession handed out is a
// snapshot. Read media bytes through ServeRange or InputURL rather than a
// snapshot's Source, which is closed when a failover swaps it.
package playback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Deps are the components playback orchestrates. Resolver, Engine,
// Transcoder, Catalog and SelectProfile are required.
type Deps struct {
	AugmentMedia func(context.Context, *core.Title, *core.User, *core.Resolution, core.MediaSource, *core.MediaInfo) // synchronous enrichment before publishing a generation
	// NativeProbe, when set, reads stream parameters straight from the
	// source's container headers before falling back to Transcoder.Probe.
	// It must return an error unless every decision-relevant field is
	// exact. NativeTrailer later fills facts kept at the file tail.
	NativeProbe   func(context.Context, core.MediaSource) (*core.MediaInfo, error)
	NativeTrailer func(context.Context, core.MediaSource, *core.MediaInfo) bool
	Resolver      core.Resolver
	Engine        core.Engine
	Transcoder    core.Transcoder
	Catalog       core.Catalog
	// Cache, when set, receives pins for live and in-progress releases.
	Cache core.SegmentCache
	// State, when set, tells which titles are in progress (cache protection).
	State core.State
	// SelectProfile picks the quality profile for a client and user. It is
	// implemented by the resolver package.
	SelectProfile func(caps core.ClientCaps, user *core.User) core.QualityProfile
	// HLSLookup finds the HLS session fed by a playback session, for
	// mid-stream failover. Defaults to Transcoder.Session(sessionID).
	HLSLookup func(sessionID string) (core.HLSSession, bool)
	// FetchNZB optionally downloads an alternate's NZB so failover can try
	// Resolution.Alternates directly; without it failover asks the resolver
	// again with the tried releases excluded.
	FetchNZB func(ctx context.Context, r core.Release) ([]byte, error)
	// Aligner, when set, maps failover timestamps between releases by
	// dialogue timing; it gets a short timeout and any miss falls back to
	// duration-based AlignTime.
	Aligner core.TimelineAligner
	// DB holds the playback_resume table; nil disables resume records.
	DB     *sql.DB
	Logger *slog.Logger
	// Cluster, when set (clustered nodes only; requires DB), mirrors this
	// node's sessions into playback_live_sessions and applies stream limits
	// across the cluster. Nil keeps everything in this process.
	Cluster clusterbus.Node
	// DescribeGroup optionally describes a live TV viewing unit reserved
	// with BeginChannel for other nodes' status pages.
	DescribeGroup func(group string) *core.PlaySession
	// Availability, when set (clustered nodes), is the node's view of
	// database reachability: resume records the database cannot take are
	// held and written later, and admission skips the cluster-wide check
	// while the database is down (failing open) instead of waiting on it.
	Availability *store.Availability
}

// Options tune the manager. Zero values take the defaults noted.
type Options struct {
	// DataDir is where NZBs of resumable releases are kept (DataDir/playback/nzb).
	DataDir string
	// IdleTimeout ends sessions without viewer activity (default 2m).
	IdleTimeout time.Duration
	// WriteStallTimeout fails a direct-play write that makes no progress
	// for this long, so a client that stopped reading releases its source
	// (default IdleTimeout: a paused player is not cut off before its
	// session would be reaped anyway).
	WriteStallTimeout time.Duration
	// StoppedGrace ends a session this long after a "stopped" timeline (default 1m).
	StoppedGrace time.Duration
	// PrepareTimeout bounds resolve+open+probe (default 90s).
	PrepareTimeout time.Duration
	// ProbeTimeout bounds one Transcoder.Probe (default 30s).
	ProbeTimeout time.Duration
	// VerifyNativeProbe runs ffprobe in the background after every native
	// probe and logs disagreements (soak testing; off by default).
	VerifyNativeProbe bool
	// TracePlayback logs every loopback range request (soak testing).
	TracePlayback bool
	// FailoverTimeout bounds a background (loopback-triggered) failover (default 90s).
	FailoverTimeout time.Duration
	// ConnectionsPerStream derives the server-wide stream cap from the total
	// Usenet connections when Server.MaxStreams is 0 (default 8).
	ConnectionsPerStream int
	// MaxAttempts is how many releases one Prepare or failover tries (default 4).
	MaxAttempts int
	// MaxFailovers per session before giving up (default 6).
	MaxFailovers int
	// ChunkSize is the copy buffer size (default 1 MiB); ReadAhead the number
	// of chunks read ahead of the writer (default 4).
	ChunkSize int
	ReadAhead int
	// PinInterval is how often in-progress pins are reconciled (default 5m).
	PinInterval time.Duration
	// ResumeRetention drops resume records of titles no longer in progress
	// after this long (default 30 days).
	ResumeRetention time.Duration
	// IndexTimeout bounds the background container keyframe-index read
	// after a generation is published (default 60s). Play never waits on it.
	IndexTimeout time.Duration
	// SpeedCheckInterval is how often source fetch speed is compared with
	// media bitrate (default 2s). SpeedGrace after a start, seek or
	// failover is ignored (default 20s). A session whose fetch rate stays
	// below SpeedThreshold (default 0.8) of its media bitrate for
	// SpeedSlowChecks consecutive checks (default 3) down-fails over, at
	// most MaxDownFailovers times (default 2; negative disables).
	SpeedCheckInterval time.Duration
	SpeedGrace         time.Duration
	SpeedThreshold     float64
	SpeedSlowChecks    int
	MaxDownFailovers   int
	// LoopbackAddr is the source server's listen address (default 127.0.0.1:0).
	LoopbackAddr string
	// SessionHeartbeat is how often a clustered node refreshes its shared
	// session rows (default 10s); SessionTTL is how long a row counts
	// without a refresh (default 45s).
	SessionHeartbeat time.Duration
	SessionTTL       time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.IdleTimeout, 2*time.Minute)
	def(&o.WriteStallTimeout, o.IdleTimeout)
	def(&o.StoppedGrace, time.Minute)
	def(&o.PrepareTimeout, 90*time.Second)
	def(&o.ProbeTimeout, 30*time.Second)
	def(&o.FailoverTimeout, 90*time.Second)
	def(&o.PinInterval, 5*time.Minute)
	def(&o.ResumeRetention, 30*24*time.Hour)
	def(&o.IndexTimeout, 60*time.Second)
	def(&o.SpeedCheckInterval, 2*time.Second)
	def(&o.SpeedGrace, 20*time.Second)
	if o.SpeedThreshold <= 0 {
		o.SpeedThreshold = 0.8
	}
	if o.SpeedSlowChecks <= 0 {
		o.SpeedSlowChecks = 3
	}
	if o.MaxDownFailovers == 0 {
		o.MaxDownFailovers = 2
	}
	def(&o.SessionHeartbeat, 10*time.Second)
	def(&o.SessionTTL, 45*time.Second)
	if o.ConnectionsPerStream <= 0 {
		o.ConnectionsPerStream = 8
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 4
	}
	if o.MaxFailovers <= 0 {
		o.MaxFailovers = 6
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = 1 << 20
	}
	if o.ReadAhead <= 0 {
		o.ReadAhead = 4
	}
	if o.LoopbackAddr == "" {
		o.LoopbackAddr = "127.0.0.1:0"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Manager implements core.Playback.
type Manager struct {
	sourceAccounting sourceAccounting
	// sourceCleanupBudget, when set, is the shutdown wait for in-flight
	// source cleanup. Production leaves it nil and uses
	// sourceCleanupShutdownWait. Tests install an already-expired context
	// so a blocked backend Close is detected by ordering, not by a timer
	// that can lose to a loaded machine.
	sourceCleanupBudget context.Context
	events              eventBus
	deps             Deps
	opts             Options
	log              *slog.Logger
	now              func() time.Time
	resume           *resumeStore
	reg              *registry // nil on a standalone server
	// titleInval broadcasts title invalidations (nil standalone).
	titleInval *clusterbus.Invalidator
	bufs       sync.Pool

	cfgMu sync.RWMutex
	cfg   config.Config

	mu             sync.Mutex
	draining       bool // SetDraining: refuse new sessions
	closed         bool
	standbyActive  int
	reserveActive  int
	sessions       map[string]*session
	byIdent        map[identity]*session
	tokens         map[string]*generation
	encoderInputs  map[string]*encoderInputScope
	flights        map[flightKey]*flight
	pending        map[int64]int
	pendingN       int
	reservations   map[identity]struct{}
	channelGroups  map[identity]string
	externalGroups map[string]externalGroup
	lastCaps       map[int64]core.ClientCaps
	warm           map[identity]*warmEntry
	warmRequests   map[warmBudgetKey][]time.Time

	pinMu        sync.Mutex
	livePins     map[string]int
	progressPins map[string]bool
	pinsSwept    bool
	pinKick      chan struct{}

	ln      net.Listener
	srv     *http.Server
	baseURL string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var (
	_ core.Playback        = (*Manager)(nil)
	_ core.Prewarmer       = (*Manager)(nil)
	_ core.VariantPlayback = (*Manager)(nil)
	_ core.RemuxRequirer   = (*Manager)(nil)
)

// flightKey separates a plain Prepare from a PrepareVariant for a picked
// release: they must not share one resolution.
type flightKey struct {
	id                identity
	prefer            string
	exact             bool
	deferSupersession bool
}

type warmBudgetKey struct {
	userID int64
	reason string
}

// hasFlightLocked reports whether any Prepare for id is in flight.
func (m *Manager) hasFlightLocked(id identity) bool {
	for k := range m.flights {
		if k.id == id {
			return true
		}
	}
	return false
}

// flight is one in-progress Prepare shared by concurrent callers.
type flight struct {
	sources *sourceLifetime
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	sess    *session
	err     error
}

// Pin reasons used with core.SegmentCache.
const (
	PinReasonLive       = "playback:live"
	PinReasonInProgress = "playback:inprogress"
)

// New starts a Manager: the loopback source server and its background
// loops (idle expiry, pin reconciliation). Call Close to stop it.
func New(deps Deps, cfg config.Config, opts Options) (*Manager, error) {
	switch {
	case deps.Resolver == nil:
		return nil, errors.New("playback: Resolver is required")
	case deps.Engine == nil:
		return nil, errors.New("playback: Engine is required")
	case deps.Transcoder == nil:
		return nil, errors.New("playback: Transcoder is required")
	case deps.Catalog == nil:
		return nil, errors.New("playback: Catalog is required")
	case deps.SelectProfile == nil:
		return nil, errors.New("playback: SelectProfile is required")
	}
	opts.defaults()
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.HLSLookup == nil {
		tr := deps.Transcoder
		deps.HLSLookup = func(id string) (core.HLSSession, bool) { return tr.Session(id) }
	}
	m := &Manager{
		deps:     deps,
		opts:     opts,
		log:      deps.Logger.With("component", "playback"),
		now:      opts.Now,
		cfg:      cfg,
		sessions: map[string]*session{},
		byIdent:  map[identity]*session{},
		tokens:   map[string]*generation{},
		flights:  map[flightKey]*flight{},
		pending:  map[int64]int{},
		lastCaps: map[int64]core.ClientCaps{},
		warm:     map[identity]*warmEntry{}, warmRequests: map[warmBudgetKey][]time.Time{},
		livePins:     map[string]int{},
		progressPins: map[string]bool{},
		pinKick:      make(chan struct{}, 1),
	}
	chunk := opts.ChunkSize
	m.bufs.New = func() any { b := make([]byte, chunk); return &b }
	if deps.DB != nil {
		if opts.DataDir == "" {
			return nil, errors.New("playback: DataDir is required with DB")
		}
		rs, err := newResumeStore(deps.DB, opts.DataDir)
		if err != nil {
			return nil, err
		}
		if deps.Availability != nil {
			rs.hold(deps.Availability, m.log)
		}
		m.resume = rs
	}
	if deps.Cluster != nil {
		if deps.DB == nil {
			return nil, errors.New("playback: Cluster requires DB")
		}
		m.reg = newRegistry(m, deps.DB, deps.Cluster)
		m.startTitleInvalidation(deps.Cluster)
	}

	ln, err := net.Listen("tcp", opts.LoopbackAddr)
	if err != nil {
		if m.resume != nil {
			m.resume.close(context.Background())
		}
		m.titleInval.Close()
		return nil, fmt.Errorf("playback: loopback listen: %w", err)
	}
	m.ln = ln
	m.baseURL = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/src/{session}/{token}", m.serveLoopback)
	m.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(m.log.Handler(), slog.LevelDebug),
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())

	m.wg.Add(4)
	go func() {
		defer m.wg.Done()
		if err := m.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.log.Error("loopback server stopped", "err", err)
		}
	}()
	go func() { defer m.wg.Done(); m.janitor() }()
	go func() { defer m.wg.Done(); m.pinLoop() }()
	go func() { defer m.wg.Done(); m.speedLoop() }()
	if m.reg != nil {
		m.reg.start()
	}
	m.log.Info("loopback source server listening", "addr", ln.Addr().String())
	return m, nil
}

// Close stops every session, the loopback server and background loops.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	flights := make([]*flight, 0, len(m.flights))
	for _, f := range m.flights {
		flights = append(flights, f)
	}
	m.mu.Unlock()
	for _, f := range flights {
		f.cancel()
	}
	for _, id := range ids {
		m.Stop(id)
	}
	m.cancel()
	err := m.srv.Close()
	// Loops exit on cancellation. Source cleanup is a separate budget: it
	// must not be extended by pin reconcile, registry sync, or resume flush,
	// and it must not wait for a backend Close that is still blocked.
	m.wg.Wait()
	cleanupCtx, cleanupCancel := m.sourceCleanupContext()
	if cleanupErr := m.drainSourceCleanup(cleanupCtx); cleanupErr != nil {
		err = errors.Join(err, fmt.Errorf("playback: source cleanup unfinished: %w", cleanupErr))
	}
	cleanupCancel()
	if m.reg != nil {
		m.reg.close()
	}
	m.titleInval.Close()
	if m.resume != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if herr := m.resume.close(ctx); herr != nil {
			m.log.Warn("playback: resume records lost at shutdown", "err", herr)
			err = errors.Join(err, herr)
		}
		cancel()
	}
	m.sourceAccounting.mu.Lock()
	pending := m.sourceAccounting.owners
	m.sourceAccounting.mu.Unlock()
	if pending != 0 {
		err = errors.Join(err, fmt.Errorf("playback: %d source responsibilities remain protected", pending))
	}
	return err
}

// SetDraining stops (true) or resumes (false) admitting new playback
// sessions on this node. A draining node — about to restart for an update
// — keeps serving its existing sessions and their seeks, but a new Prepare
// or live TV tune fails with core.ErrStreamLimit so the client retries,
// reaching another node through another published connection.
func (m *Manager) SetDraining(draining bool) {
	m.mu.Lock()
	changed := m.draining != draining
	m.draining = draining
	m.mu.Unlock()
	if changed {
		if draining {
			m.log.Info("playback: draining: no new sessions on this node")
		} else {
			m.log.Info("playback: no longer draining")
		}
	}
}

// Draining reports whether SetDraining(true) is in effect.
func (m *Manager) Draining() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draining
}

// ResumeOutboxStatus reports resume records held through a database
// outage; ok is false when records are written synchronously.
func (m *Manager) ResumeOutboxStatus() (st store.OutboxStatus, ok bool) {
	if m.resume == nil || m.resume.held == nil {
		return st, false
	}
	return m.resume.held.Status(), true
}

// errDraining is returned for new sessions while the node drains.
var errDraining = fmt.Errorf("%w: this server is restarting for an update; try again", core.ErrStreamLimit)

// SetConfig applies a new config snapshot (stream limits).
func (m *Manager) SetConfig(cfg config.Config) {
	m.cfgMu.Lock()
	m.cfg = cfg
	m.cfgMu.Unlock()
	m.mu.Lock()
	m.warm = map[identity]*warmEntry{}
	m.mu.Unlock()
}

func (m *Manager) config() config.Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cfg
}

// UserStreamLimit is the number of concurrent streams a user may hold: the
// user's own limit, else the server default; 0 means unlimited.
func UserStreamLimit(cfg config.Config, u *core.User) int {
	switch {
	case u != nil && u.StreamLimit > 0:
		return u.StreamLimit
	case u != nil && u.StreamLimit < 0:
		return 0
	case cfg.Server.DefaultStreamLimit > 0:
		return cfg.Server.DefaultStreamLimit
	}
	return 0
}

// ServerStreamLimit is the server-wide stream cap: Server.MaxStreams, or the
// total connections of enabled primary (non-backfill) Usenet providers
// divided by connsPerStream, at least 1. 0 means unlimited (no providers).
func ServerStreamLimit(cfg config.Config, connsPerStream int) int {
	if cfg.Server.MaxStreams > 0 {
		return cfg.Server.MaxStreams
	}
	if connsPerStream <= 0 {
		connsPerStream = 8
	}
	total := 0
	for _, p := range cfg.Usenet.Providers {
		if p.Enabled && !p.Backfill && p.Connections > 0 {
			total += p.Connections
		}
	}
	if total == 0 {
		return 0
	}
	if n := total / connsPerStream; n > 1 {
		return n
	}
	return 1
}

// reserve claims a stream slot for a Prepare, enforcing per-user and
// server-wide limits. Sessions of the same user and client count as
// replaceable: a client plays one thing at a time. A clustered node then
// checks the same limits against the whole cluster (see registry.admit).
func (m *Manager) reserve(ctx context.Context, user *core.User, id identity) error {
	cfg := m.config()
	userLimit := UserStreamLimit(cfg, user)
	serverLimit := ServerStreamLimit(cfg, m.opts.ConnectionsPerStream)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("playback: shutting down")
	}
	if m.draining {
		m.mu.Unlock()
		return errDraining
	}
	userN, total, children := m.reservationCountsLocked(id)
	group := m.channelGroups[id]
	if group != "" && children >= 3 {
		m.mu.Unlock()
		return fmt.Errorf("%w: channel source overlap limit", core.ErrStreamLimit)
	}
	if userLimit > 0 && userN >= userLimit {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s is using %d of %d streams", core.ErrStreamLimit, userLabel(user), userN, userLimit)
	}
	if serverLimit > 0 && total >= serverLimit {
		m.mu.Unlock()
		return fmt.Errorf("%w: server is at its limit of %d streams", core.ErrStreamLimit, serverLimit)
	}
	if m.reservations == nil {
		m.reservations = map[identity]struct{}{}
	}
	m.reservations[id] = struct{}{}
	m.pending[user.ID]++
	m.pendingN++
	m.mu.Unlock()
	if m.reg == nil {
		return nil
	}
	// A source joining this node's own channel group adds no unit.
	if group != "" && children > 0 {
		m.reg.poke()
		return nil
	}
	claim := unitRow{id: pendingID(m.reg.self, id), user: id.userID, client: clusterClientID(id), grp: group, kind: unitPending}
	if err := m.reg.admit(ctx, user, id, claim, userN, total, userLimit, serverLimit); err != nil {
		m.mu.Lock()
		m.unreserveLocked(id)
		m.mu.Unlock()
		m.reg.poke()
		return err
	}
	return nil
}

func (m *Manager) unreserveLocked(id identity) {
	userID := id.userID
	delete(m.reservations, id)
	if m.pending[userID]--; m.pending[userID] <= 0 {
		delete(m.pending, userID)
	}
	m.pendingN--
}

func userLabel(u *core.User) string {
	if u.Username != "" {
		return u.Username
	}
	if u.Title != "" {
		return u.Title
	}
	return fmt.Sprintf("user %d", u.ID)
}

// Prepare resolves and opens a source for the user and client, enforcing
// stream limits, or returns the existing session for the same user, client
// and title. Concurrent calls for the same session share one resolution.
func (m *Manager) Prepare(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	return m.prepareEntry(ctx, user, key, caps, "", false, false)
}

// PrepareSurface keeps a prepared source until its owning session opens or expires.
func (m *Manager) PrepareSurface(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, releaseID string, exact bool) (*core.PlaySession, error) {
	if exact && releaseID == "" {
		return nil, core.ErrNotAvailable
	}
	return m.prepareEntry(ctx, user, key, caps, releaseID, exact, true)
}

// prepareEntry is Prepare plus an optional picked release (prefer != "").
// An existing session on a different release is replaced, never reused.
func (m *Manager) prepareEntry(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, prefer string, exact, deferSupersession bool) (*core.PlaySession, error) {
	ps, _, err := m.prepareOwnedEntry(ctx, user, key, caps, prefer, exact, deferSupersession)
	return ps, err
}

func (m *Manager) prepareOwnedEntry(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, prefer string, exact, deferSupersession bool) (*core.PlaySession, core.SourceRetirement, error) {
	if user == nil {
		return nil, nil, core.ErrUnauthorized
	}
	if lookup, ok := m.deps.Catalog.(core.StoredTitleLookup); ok {
		if title, err := lookup.StoredTitle(ctx, key); err == nil && title != nil && !title.Released.IsZero() && title.Released.After(m.now()) {
			return nil, nil, fmt.Errorf("playback: title releases on %s: %w", title.Released.Format("2006-01-02"), core.ErrUnreleased)
		}
	}
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: playbackSurface(string(caps.Protocol))}
	if deferSupersession {
		id.variant, id.exact = prefer, exact
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, nil, errors.New("playback: shutting down")
		}
		m.lastCaps[user.ID] = caps
		if s := m.byIdent[id]; s != nil {
			if s.exact == exact && (prefer == "" || s.current().releaseID() == prefer) {
				m.mu.Unlock()
				s.touch(m.now())
				return s.snapshot(m.now()), s.sources, nil
			}
			old := s.id
			m.mu.Unlock()
			m.log.Debug("variant picked a different release; replacing session", "old", old, "prefer", prefer)
			m.Stop(old)
			continue
		}
		break
	}
	fk := flightKey{id: id, prefer: prefer, exact: exact, deferSupersession: deferSupersession}
	f := m.flights[fk]
	if f == nil {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.opts.PrepareTimeout)
		f = &flight{sources: newSourceLifetime(user.ID, string(caps.Protocol), caps.ClientID), done: make(chan struct{}), cancel: cancel}
		m.flights[fk] = f
		u := *user
		go m.runFlight(wctx, f, fk, &u, caps)
	}
	f.waiters++
	m.mu.Unlock()

	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.sources, f.err
		}
		return f.sess.snapshot(m.now()), f.sess.sources, nil
	case <-ctx.Done():
		m.mu.Lock()
		if f.waiters--; f.waiters == 0 {
			f.cancel() // nobody wants it any more
		}
		m.mu.Unlock()
		return nil, f.sources, ctx.Err()
	}
}

func (m *Manager) runFlight(ctx context.Context, f *flight, fk flightKey, user *core.User, caps core.ClientCaps) {
	defer close(f.done)
	defer f.cancel()
	s, err := m.prepare(ctx, fk.id, user, caps, fk.prefer, fk.exact, fk.deferSupersession, f.sources)
	m.mu.Lock()
	delete(m.flights, fk)
	if err != nil && m.byIdent[fk.id] == nil {
		delete(m.channelGroups, fk.id)
	}
	m.mu.Unlock()
	f.sess, f.err = s, err
	if err != nil {
		f.sources.seal()
		m.emitEvent("error", s, err)
	}
}

func (m *Manager) prepare(ctx context.Context, id identity, user *core.User, caps core.ClientCaps, prefer string, exact, deferSupersession bool, sources *sourceLifetime) (*session, error) {
	log := m.log.With("user", userLabel(user), "client", caps.ClientID, "key", id.key)
	if err := m.reserve(ctx, user, id); err != nil {
		log.Info("stream limit", "err", err)
		return nil, err
	}
	registered := false
	defer func() {
		if !registered {
			m.mu.Lock()
			m.unreserveLocked(id)
			m.mu.Unlock()
			m.reg.poke()
		}
	}()

	title, err := m.deps.Catalog.Get(ctx, id.key)
	if err != nil {
		return nil, fmt.Errorf("playback: title %s: %w", id.key, err)
	}
	if title == nil || (!title.Released.IsZero() && title.Released.After(m.now())) {
		return nil, fmt.Errorf("playback: title is not released: %w", core.ErrUnreleased)
	}
	if title.Type == core.TypeShow || title.Type == core.TypeSeason {
		return nil, fmt.Errorf("playback: %s is a %s, not a playable item: %w", id.key, title.Type, core.ErrNotFound)
	}
	profile := m.deps.SelectProfile(caps, user)
	s := newSession(randomID(16), id, user, caps, title, profileKey(profile), m.now())
	s.sources = sources
	defer func() {
		if !registered {
			sources.seal()
		}
	}()
	s.preparedOnly = deferSupersession
	s.prefer = prefer
	s.exact = exact
	s.resetSpeed(m.now(), m.opts.SpeedGrace)
	m.mu.Lock()
	s.playbackGroup = m.channelGroups[id]
	m.mu.Unlock()

	g, err := m.openInitial(ctx, s, profile)
	if err != nil {
		log.Info("prepare failed", "err", err)
		return nil, err
	}
	s.adopt(g)
	// Pin before the session becomes visible: a Stop racing registration
	// (a replacing Prepare, Close) must find the pin it releases, or the
	// pin taken afterwards would never be dropped.
	m.pinLive(g.releaseID())

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.dropGeneration(g)
		m.unpinLive(g.releaseID())
		return nil, errors.New("playback: shutting down")
	}
	// Distinct picked-release flights can race to register the same
	// user/client/title. Keep the first completed session and discard the
	// losing source instead of overwriting byIdent and leaking a live stream.
	if existing := m.byIdent[id]; existing != nil {
		m.mu.Unlock()
		m.dropGeneration(g)
		m.unpinLive(g.releaseID())
		// Concurrent flights with different exactness must never let an exact
		// caller inherit a fallback-enabled session, even for the same release.
		current := existing.current()
		if existing.exact != exact || (exact && (current == nil || current.releaseID() != prefer)) {
			return nil, core.ErrNotAvailable
		}
		return existing, nil
	}
	var replaced []string
	for _, o := range m.sessions {
		// Broker supersession occurs on Open, not while a plan is prepared.
		if !deferSupersession && o.ident.userID == id.userID && o.ident.clientID == id.clientID && o.ident.surface == id.surface && o.ident.key != id.key {
			replaced = append(replaced, o.id)
		}
	}
	m.unreserveLocked(id)
	registered = true
	m.sessions[s.id] = s
	m.byIdent[id] = s
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	m.mu.Unlock()

	for _, old := range replaced {
		log.Debug("client switched titles; replacing session", "old", old)
		m.Stop(old)
	}
	m.reg.poke()
	m.saveResume(ctx, s, g)
	if c, ok := m.deps.Cache.(core.CachePlayRecorder); ok {
		title := s.title.Title
		if s.title.ShowTitle != "" {
			title = s.title.ShowTitle + " · " + title
		}
		c.RecordPlay(g.releaseID(), title, g.src.Size())
	}
	ginfo := g.infoSnapshot()
	log.Info("playback session started", "session", s.id, "release", g.res.Release.Name,
		"container", ginfo.Container, "size", g.src.Size(), "duration", ginfo.Duration, "estimated", ginfo.Estimated)
	m.emitEvent("start", s, nil)
	return s, nil
}

func (m *Manager) lookup(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Session returns a snapshot of a live session.
func (m *Manager) Session(id string) (*core.PlaySession, bool) {
	s := m.lookup(id)
	if s == nil {
		return nil, false
	}
	return s.snapshot(m.now()), true
}

// SessionFor finds the live session of a client for a title.
func (m *Manager) SessionFor(userID int64, clientID string, key core.RatingKey) (*core.PlaySession, bool) {
	m.mu.Lock()
	s := m.byIdent[identity{userID: userID, clientID: clientID, key: key}]
	m.mu.Unlock()
	if s == nil {
		return nil, false
	}
	return s.snapshot(m.now()), true
}

func (m *Manager) SessionForSurface(surface string, userID int64, clientID string, key core.RatingKey, release string, exact bool) (*core.PlaySession, bool) {
	m.mu.Lock()
	s := m.byIdent[identity{userID: userID, clientID: clientID, key: key, surface: playbackSurface(surface), variant: release, exact: exact}]
	m.mu.Unlock()
	if s == nil {
		return nil, false
	}
	return s.snapshot(m.now()), true
}

// MarkSurfaceOpened changes Activity visibility only after broker admission.
func (m *Manager) MarkSurfaceOpened(id string) bool {
	return m.setSurfacePrepared(id, false)
}

// MarkSurfacePrepared hides a source after its final frontend session stops
// while another scoped plan still holds the source lease.
func (m *Manager) MarkSurfacePrepared(id string) bool {
	return m.setSurfacePrepared(id, true)
}

func (m *Manager) setSurfacePrepared(id string, prepared bool) bool {
	s := m.lookup(id)
	if s == nil {
		return false
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.preparedOnly = prepared
	s.mu.Unlock()
	m.reg.poke()
	return true
}

// Sessions returns snapshots of every live session, oldest first.
func (m *Manager) Sessions() []*core.PlaySession {
	m.mu.Lock()
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	now := m.now()
	out := make([]*core.PlaySession, 0, len(list))
	for _, s := range list {
		out = append(out, s.snapshot(now))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Stop ends a session: its source is closed once reads finish, its live
// pin is dropped and its HLS session, if any, is stopped.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return
	}
	g := m.detachSessionLocked(s)
	m.mu.Unlock()
	m.finishSessionStop(s, g)
}

// Caller holds m.mu; no I/O or backend cleanup occurs here.
func (m *Manager) detachSessionLocked(s *session) *generation {
	delete(m.sessions, s.id)
	if m.byIdent[s.ident] == s {
		delete(m.byIdent, s.ident)
		delete(m.channelGroups, s.ident)
	}
	s.mu.Lock()
	s.closed = true
	if s.stopCancel != nil {
		s.stopCancel()
	}
	g := s.cur
	s.mu.Unlock()
	if g != nil {
		delete(m.tokens, g.sess.id+"/"+g.token)
	}
	s.sources.seal()
	return g
}

func (m *Manager) finishSessionStop(s *session, g *generation) {
	id := s.id
	if g != nil {
		g.retire()
		m.unpinLive(g.releaseID())
	}
	if hls, ok := m.deps.HLSLookup(id); ok && hls != nil {
		m.deps.Transcoder.Stop(hls.ID())
	}
	m.reg.poke()
	m.emitEvent("stop", s, nil)
	m.kickPins()
	m.log.Info("playback session ended", "session", id, "key", s.ident.key, "bytes", s.bytes.Load())
}

// InputURL is the loopback URL ffmpeg reads the session's current source
// from; "" for an unknown session. The URL changes on failover.
func (m *Manager) InputURL(id string) string {
	s := m.lookup(id)
	if s == nil {
		return ""
	}
	return m.urlFor(s.current())
}

func (m *Manager) urlFor(g *generation) string {
	return m.baseURL + "/src/" + g.sess.id + "/" + g.token
}

// SetDecision records the transcode decision the front end made for a
// session, shown in Sessions(). A remux/transcode decision satisfies a
// pending RequiresRemux.
func (m *Manager) SetDecision(id string, d core.Decision) {
	if s := m.lookup(id); s != nil {
		s.mu.Lock()
		s.decision, s.explicit = d.Kind, true
		s.decisionReason = d.Reason
		if d.Kind == core.DecisionRemux || d.Kind == core.DecisionTranscode {
			s.requiresRemux = false
		}
		s.mu.Unlock()
		m.emitEvent("update", s, nil)
	}
}

// RequiresRemux reports whether the session must be (re)started as a
// zero-encode remux instead of direct play (see core.RemuxRequirer).
func (m *Manager) RequiresRemux(sessionID string) bool {
	s := m.lookup(sessionID)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requiresRemux
}

// Timeline renews a viewer's lease while playing or paused. Buffering alone
// cannot preserve a failed stream; stopped ends it after Options.StoppedGrace.
func (m *Manager) Timeline(userID int64, key core.RatingKey, state string) {
	now := m.now()
	m.mu.Lock()
	var list []*session
	for _, s := range m.sessions {
		if s.ident.userID == userID && s.ident.key == key {
			list = append(list, s)
		}
	}
	m.mu.Unlock()
	for _, s := range list {
		if state == "stopped" {
			s.mu.Lock()
			s.stopAt = now.Add(m.opts.StoppedGrace)
			s.mu.Unlock()
		} else if state == "playing" || state == "paused" {
			s.touch(now)
		}
	}
	if state == "stopped" {
		m.kickPins()
	}
}

// ClientCaps returns the capabilities to resolve related titles (the next
// episode) with: those of the user's live session for key (live=true), else
// of the user's most recent Prepare.
func (m *Manager) ClientCaps(userID int64, key core.RatingKey) (caps core.ClientCaps, live bool, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *session
	for _, s := range m.sessions {
		if s.ident.userID == userID && s.ident.key == key {
			if best == nil || s.startedAt.After(best.startedAt) {
				best = s
			}
		}
	}
	if best != nil {
		return best.caps, true, true
	}
	c, ok := m.lastCaps[userID]
	return c, false, ok
}

// ActiveFor reports whether the user has a live session for the title.
func (m *Manager) ActiveFor(userID int64, key core.RatingKey) bool {
	_, live, _ := m.ClientCaps(userID, key)
	return live
}

// janitor expires idle and stopped sessions.
func (m *Manager) janitor() {
	every := m.opts.IdleTimeout / 4
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.expire()
		}
	}
}

// expire stops sessions idle longer than IdleTimeout (an HLS session's last
// access counts as activity) and stopped sessions past their grace.
func (m *Manager) expire() {
	now := m.now()
	m.mu.Lock()
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		s.mu.Lock()
		last, stopAt, viewerState := s.lastActive, s.stopAt, s.viewerState
		s.mu.Unlock()
		if hls, ok := m.deps.HLSLookup(s.id); ok && hls != nil {
			if la := hls.LastAccess(); la.After(last) {
				last = la
			}
		}
		idleTimeout := m.opts.IdleTimeout
		if viewerState == "paused" {
			idleTimeout = max(idleTimeout, core.PausedSessionRetention)
		}
		switch {
		case !stopAt.IsZero() && !now.Before(stopAt) && !last.After(stopAt.Add(-m.opts.StoppedGrace)):
			m.log.Debug("session stopped by client", "session", s.id)
			m.Stop(s.id)
		case now.Sub(last) > idleTimeout:
			m.log.Debug("session idle", "session", s.id, "idle", now.Sub(last))
			m.Stop(s.id)
		}
	}
}
