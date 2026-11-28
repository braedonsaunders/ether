package livetv

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/lru"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func init() {
	store.Register("livetv", 18, `CREATE TABLE livetv_readiness(channel_id TEXT PRIMARY KEY, active_until INTEGER NOT NULL);`)
	store.Register("livetv", 1, `CREATE TABLE livetv_channels(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE livetv_sources(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE livetv_programmes(source_id TEXT NOT NULL, channel_id TEXT NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(source_id,channel_id,start_ms));
CREATE INDEX livetv_programmes_window ON livetv_programmes(channel_id,start_ms,end_ms);
CREATE TABLE livetv_policies(user_id INTEGER PRIMARY KEY, data TEXT NOT NULL);`)
}

type Service struct {
	watchSearch                               watchChannelSearchCache
	searchDemanded                            atomic.Bool
	searchVirtualMu                           sync.Mutex
	searchVirtualSnapshots                    map[string]virtualSearchIndexedSnapshot
	searchMu                                  sync.Mutex
	searchBuilding, searchReady, searchClosed bool
	searchDone                                chan struct{}

	ownerGroupsMu        sync.Mutex
	ownerGroupsIndex     *ownerGroupIndex
	ownerGroupsIndexRev  int64
	ownerGroupChoices    []OwnerGroupChoice
	ownerGroupChoicesRev int64
	inputMergeMu         sync.Mutex
	inputMergeIdx        *inputMergeIndex
	inputMergeRev        int64
	loginPool            loginPool
	closeOnce            sync.Once
	lifetimeStop         chan struct{}
	lifetimeDone         chan struct{}
	lifetimeWake         chan struct{}
	hasChannels          atomic.Bool
	lifecycleMu          sync.Mutex
	running              bool

	readinessWake         chan struct{}
	readiness             map[string]channelReadiness
	readyTitles           map[core.RatingKey]bool
	programmeSubs         map[string]Programme
	activityWake          chan struct{}
	noActivitySources     atomic.Bool
	hasOnDemand           atomic.Bool
	sourceWake            chan struct{}
	vodWake               chan struct{}
	maintainWake          chan struct{}
	breakWake             chan struct{}
	personalWake          chan struct{}
	sweepWake             chan struct{}
	showcasePendingTotals map[string][2]int
	lastClientActivity    time.Time            // guarded by mu
	sourceUsed            map[string]time.Time // guarded by mu
	vodRefreshMu          sync.Mutex
	maintenance           maintenanceHealth
	virtualGuide          virtualGuideCache
	breakObserved         chan breakObservation
	observedBreaks        map[core.RatingKey]breakObservation
	guideAIMu             sync.Mutex
	graphicsWake          chan struct{}
	reelSnapshots         map[string]*ReelSnapshot
	graphicsSkips         map[string]time.Time
	reelSubstitutions     map[string]reelSubEntry
	personalDirty         map[int64]uint64
	personalRecipes       map[personalKey]string
	personalMu            sync.Mutex
	personalLineup        map[int64]time.Time
	personalClocks        map[personalKey]*personalClock
	broadcasts            broadcastRegistry
	broadcastProviders    map[string]map[string]string
	providerWait          providerWaitQueue
	chanSnap              channelSnapshot // decoded lineup shared by request paths (channel_cache.go)
	catchUpListMu         sync.Mutex
	catchUpLists          map[catchUpListKey]catchUpListEntry
	guidePreviewMu        sync.Mutex
	guidePreview          map[string]guidePreviewEntry
	authCache             authCache  // provider credentials for the stream path (auth.go)
	inputRelay            inputRelay // loopback ffmpeg input relay (input_relay.go)
	probeMu               sync.Mutex
	probeCache            map[string]liveProbeEntry
	probeWait             map[string]*liveProbeFlight
	healthMu              sync.RWMutex
	channelHealth         map[string]sweepResult
	healthRev             atomic.Uint64
	nextHealthExpiry      atomic.Int64 // earliest future UnixNano cooldown/failure expiry, 0 if none
	sourceMu              sync.Mutex
	sourceHealth          map[string]streamSourceHealth
	history               sourceHistoryStore // learned per-source evidence (source_history.go)
	transcoderMu          sync.RWMutex
	o                     Options
	kinds                 *KindRegistry
	policyReads           atomic.Uint64
	mu                    sync.Mutex
	breakOwners           map[string]int64 // admitted save identities; never persisted
	breakOwnerRevision    uint64           // identity admission invalidates background retries
	lineupUnix            int64
	onChange              func()
	sessions              map[string]*session
	// watched is when each channel was last tuned or playing (guarded by
	// mu). activeWatched tracks channels with an active viewer right now.
	// viewerWatched tracks bounded per-viewer recent channel tunes for
	// sports watch-target preference ranking.
	watched          map[string]time.Time
	activeWatched    map[string]time.Time
	viewerWatched    map[int64]map[string]time.Time
	viewerWatchedGen map[int64]uint64
	warmMu           sync.Mutex
	warmed           map[warmTarget]time.Time
	vis              broadcastVisibilityState

	progressMu       sync.Mutex
	progress         map[string]*progressTracker
	lineupMu         sync.Mutex
	lineupIndex      []ChannelSummary
	lineupOrders     map[string][]int
	lineupRev        int64
	coverageMu       sync.Mutex
	coverageStats    map[guideKey]guideStat
	coverageAt       time.Time
	coverageRev      int64
	nextSubscription int64 // guarded by mu
	ctxMu            sync.RWMutex
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	dvrOnce          sync.Once
	dvr              *transcode.LiveDVRCache
	direct           ownedServiceState // Direct-path owned timelines and slates (owned_*.go)
}

func (s *Service) transcoder() core.Transcoder {
	s.transcoderMu.RLock()
	tc := s.o.Transcoder
	s.transcoderMu.RUnlock()
	return tc
}

func (s *Service) setTranscoder(tc core.Transcoder) {
	s.transcoderMu.Lock()
	s.o.Transcoder = tc
	s.transcoderMu.Unlock()
}

func New(o Options) (*Service, error) {
	if o.DB == nil {
		return nil, fmt.Errorf("livetv requires a database")
	}
	if o.Logger == nil {
		o.Logger = slog.Default().With("component", "livetv")
	}
	if o.NewTimer == nil {
		o.NewTimer = time.NewTimer
	}
	if o.NewTicker == nil {
		o.NewTicker = time.NewTicker
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapLiveTV, 20*time.Second)
	}
	o.HTTPClient = sourceHTTPClient(o.HTTPClient)
	ctx, cancel := context.WithCancel(context.Background())
	kinds := o.Kinds
	if kinds == nil {
		kinds = NewKindRegistry()
	}
	s := &Service{
		watchSearch:    watchChannelSearchCache{results: lru.New(lru.Options[watchChannelSearchKey, []string]{MaxEntries: 2048})},
		breakObserved:  make(chan breakObservation, 64),
		observedBreaks: map[core.RatingKey]breakObservation{},
		graphicsWake:   make(chan struct{}, 1),
		o:              o,
		kinds:          kinds,
		sessions:       map[string]*session{},
		ctx:            ctx,
		cancel:         cancel,
		lineupUnix:     o.Now().Unix(),
	}
	// Identity and wake channels stay stable across worker restarts. History
	// loading and background work remain gated on channel existence.
	s.history.s = s
	s.history.wake = make(chan struct{}, 1)
	s.history.pg = store.DialectOf(o.DB) == store.Postgres
	s.readinessWake = make(chan struct{}, 1)
	s.readiness = map[string]channelReadiness{}
	s.activityWake = make(chan struct{}, 1)
	s.sourceWake = make(chan struct{}, 1)
	s.vodWake = make(chan struct{}, 1)
	s.maintainWake = make(chan struct{}, 1)
	s.breakWake = make(chan struct{}, 1)
	s.personalWake = make(chan struct{}, 1)
	s.sweepWake = make(chan struct{}, 1)
	s.broadcasts.onAcquire = s.wakeMaintenance
	s.virtualGuide.wake = make(chan struct{}, 1)
	s.lifetimeStop = make(chan struct{})
	s.lifetimeDone = make(chan struct{})
	s.lifetimeWake = make(chan struct{}, 1)
	var exists bool
	if err := o.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM livetv_channels LIMIT 1)`).Scan(&exists); err != nil {
		cancel()
		return nil, err
	}
	s.hasChannels.Store(exists)
	s.ReconcileEnabled()
	go func() {
		defer close(s.lifetimeDone)
		for {
			select {
			case <-s.lifetimeStop:
				return
			case <-s.lifetimeWake:
				s.ReconcileEnabled()
				if s.o.ActivityChanged != nil {
					s.o.ActivityChanged(s.Enabled())
				}
			}
		}
	}()
	return s, nil
}

// Enabled is shared by protocol surfaces and all background work admission.
// An indexed existence probe also sees channels committed by another cluster node.
// It never decodes a lineup or scans the media library. Changes wake the sleeping
// lifetime controller; workers never wait for themselves to drain.
func (s *Service) Enabled() bool {
	var exists bool
	if err := s.o.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM livetv_channels LIMIT 1)`).Scan(&exists); err != nil {
		return false
	}
	if s.hasChannels.Swap(exists) != exists {
		select {
		case s.lifetimeWake <- struct{}{}:
		default:
		}
	}
	return exists
}

// ReconcileEnabled drains workers before changing their lifetime context.
func (s *Service) ReconcileEnabled() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	enabled := s.Enabled()
	if enabled == s.running {
		return
	}
	if !enabled {
		s.cancel()
		s.broadcasts.close()
		s.wg.Wait()
		s.waitVirtualGuideInflight()
		s.mu.Lock()
		all := s.sessions
		s.sessions = map[string]*session{}
		s.watched = nil
		s.activeWatched = nil
		s.viewerWatched = nil
		s.viewerWatchedGen = nil
		s.sourceUsed = nil
		s.mu.Unlock()
		for _, ss := range all {
			ss.close(s)
		}
		s.virtualGuide.mu.Lock()
		s.virtualGuide.entries = nil
		s.virtualGuide.timeline = nil
		s.virtualGuide.lastDemand = time.Time{}
		s.virtualGuide.timelineDemand = time.Time{}
		s.virtualGuide.mu.Unlock()
		s.running = false
		return
	}
	// The first activation keeps the context created by New: a request may
	// already have attached a session to it. Only a drained lifetime needs a
	// replacement context.
	s.ctxMu.Lock()
	if s.ctx.Err() != nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	s.ctxMu.Unlock()
	s.broadcasts.mu.Lock()
	s.broadcasts.closed = false
	s.broadcasts.mu.Unlock()
	s.running = true
	if s.o.Picker != nil && s.o.Users != nil {
		s.wg.Add(1)
		go s.personalLoop()
	}
	s.wg.Add(1)
	go s.virtualGuideLoop()
	// Channel readiness belongs to the Live TV lifetime: it stops when Live
	// TV is turned off and restarts with it.
	s.wg.Add(1)
	go s.readinessLoop()
	if s.o.BreakInput != nil && s.o.BreakAnalyzer != nil {
		s.wg.Add(1)
		go s.breakLoop()
	}
	if s.o.Showcase && s.o.Catalog != nil {
		s.wg.Add(1)
		go s.showcaseLoop()
	}
	s.wg.Add(4)
	go s.vodLoop()
	go s.maintain()
	go s.refreshLoop()
	go s.healthSweepLoop()
	s.history.start(s)
}

// A neutral playback identity is shared by source requests. Explicit source
// and per-channel identities override it without changing each other's headers.
const sourceUserAgent = "MediaPlayer/1.0"

// sourceHTTPClient returns the client for every upstream Live TV request. It
// copies the caller's client so the default identity never mutates it.
func sourceHTTPClient(base *http.Client) *http.Client {
	c := *base
	if c.Transport == nil {
		c.Transport = http.DefaultTransport
	}
	c.Transport = sourceUserAgentTransport{base: c.Transport}
	return &c
}

// sourceUserAgentTransport adds the default player identity when a source has
// not set its own. An owner's user agent is already on the request and wins.
type sourceUserAgentTransport struct{ base http.RoundTripper }

func (t sourceUserAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("User-Agent") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("User-Agent", sourceUserAgent)
	}
	return t.base.RoundTrip(r)
}

// sourceOrigin names where a source's credentials are sent without its query
// string: playlist and guide URLs, and Xtream's derived ones, embed them.
func sourceOrigin(src Source) string {
	if src.Auth != nil {
		if origin := urlOrigin(src.Auth.Server); origin != "" {
			return origin
		}
	}
	if origin := urlOrigin(src.PlaylistURL); origin != "" {
		return origin
	}
	return urlOrigin(src.GuideURL)
}
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.searchMu.Lock()
		s.searchClosed = true
		s.searchMu.Unlock()
		close(s.lifetimeStop)
		<-s.lifetimeDone
		s.cancel()
		s.broadcasts.close()
		s.wg.Wait()
		s.waitVirtualGuideInflight()
		s.mu.Lock()
		all := s.sessions
		s.sessions = map[string]*session{}
		s.mu.Unlock()
		for _, ss := range all {
			ss.close(s)
		}
		s.history.close()
	})
	return nil
}

func (s *Service) notifySessionsChanged() {
	if s != nil && s.o.SessionsChanged != nil {
		s.o.SessionsChanged()
	}
}

func (s *Service) notifySourcesChanged() {
	if s != nil && s.o.SourcesChanged != nil {
		s.o.SourcesChanged()
	}
}

func (s *Service) invalidate(channelID, sourceID string) {
	s.broadcasts.invalidate(channelID, sourceID != "")
	s.mu.Lock()
	var stale []*session
	for id, ss := range s.sessions {
		if ss.info.Channel.ID == channelID || sourceID != "" && ss.info.Channel.SourceID == sourceID {
			stale = append(stale, ss)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, ss := range stale {
		s.sessionLog(ss).Info("live session expired")
		ss.close(s)
		if s.o.SessionExpired != nil {
			s.o.SessionExpired(ss.info.ID)
		}
	}
	if len(stale) > 0 {
		s.notifySessionsChanged()
	}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Native clients parse Media.uuid as a canonical UUID.
func liveSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Service) wakeMaintenance() {
	if s.maintainWake != nil {
		select {
		case s.maintainWake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) maintainActive() bool {
	return len(s.recentlyWatched()) > 0 || s.hasLiveActivity()
}

func (s *Service) maintain() {
	defer s.wg.Done()
	steps := s.maintenanceSteps()
	timer := s.newTimer(15 * time.Second)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()
	wasio := false
	for {
		if !s.maintainActive() {
			wasio = false
			select {
			case <-s.serviceContext().Done():
				return
			case <-s.maintainWake:
				continue
			case <-s.graphicsWake:
				if s.virtualGuideActive(s.o.Now()) {
					steps("refreshReels", s.refreshReels)
					s.wakeVirtualGuide()
				} else {
					s.mu.Lock()
					s.reelSnapshots = nil
					s.mu.Unlock()
				}
				continue
			}
		}
		if !wasio {
			wasio = true
			select {
			case <-s.maintainWake:
			default:
			}
			steps("refreshReels", s.refreshReels)
			steps("ensureGraphics", s.ensureGraphics)
			if s.o.Warm != nil {
				steps("warmProgrammes", s.warmProgrammes)
			}
		}
		lastSlow := s.o.Now()
		for s.maintainActive() {
			timer.Reset(5 * time.Second)
			select {
			case <-s.serviceContext().Done():
				return
			case <-s.graphicsWake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				steps("refreshReels", s.refreshReels)
			case <-timer.C:
				steps("expireSessions", s.expireSessions)
				steps("expireBroadcasts", func() { s.broadcasts.expire(s.o.Now()) })
				if now := s.o.Now(); now.Sub(lastSlow) >= 15*time.Second {
					lastSlow = now
					steps("refreshReels", s.refreshReels)
					steps("ensureGraphics", s.ensureGraphics)
					if s.o.Warm != nil {
						steps("warmProgrammes", s.warmProgrammes)
					}
				}
			}
		}
	}
}

func (s *Service) warmProgrammes() {
	active := s.activeWatchedChannels()
	if len(active) == 0 {
		return
	}
	cs, err := s.virtualChannels(s.serviceContext())
	if err != nil {
		return
	}
	s.mu.Lock()
	watchedAt := make(map[string]time.Time, len(s.watched))
	for id, at := range s.watched {
		watchedAt[id] = at
	}
	s.mu.Unlock()
	sort.SliceStable(cs, func(i, j int) bool { return watchedAt[cs[i].ID].After(watchedAt[cs[j].ID]) })
	admitted := 0
	for _, c := range cs {
		// Strictly demand-driven: only channels with an active viewer warm content,
		// and at most ONE lookahead item per watched channel.
		if !c.Enabled || c.Kind != "virtual" || !active[c.ID] {
			continue
		}
		if admitted >= 2 {
			break
		}
		admitted++
		now := s.o.Now()
		ps := Schedule(s.graphicsChannel(c), now, now.Add(defaultProgrammeWarmLead+time.Minute))
		targets, breakReturn := selectChannelWarmTargets(ps, now, defaultProgrammeWarmLead)
		for idx, p := range targets {
			// Continuous outputs already acquire their on-air decoder alongside
			// the title. A second detail warm competes for the same cold bytes.
			if !p.Start.After(now) && s.hasBroadcastOutput(c.ID) {
				continue
			}
			offset := p.Offset(maxTime(now, p.Start))
			wk := warmTarget{channelID: c.ID, key: p.Item.Key, offsetSec: int64(offset / time.Second)}
			s.warmMu.Lock()
			if s.warmed == nil {
				s.warmed = map[warmTarget]time.Time{}
			}
			last := s.warmed[wk]
			s.warmMu.Unlock()
			if now.Sub(last) < channelWarmRefresh(p, now) {
				continue
			}
			if !s.isActivelyWatched(c.ID) {
				break
			}
			ctx, cancel := context.WithTimeout(s.serviceContext(), 15*time.Second)
			err := s.o.Warm(ctx, p.Item.Key, offset)
			cancel()
			if err == nil {
				s.warmMu.Lock()
				s.warmed[wk] = s.o.Now()
				s.warmMu.Unlock()
				s.noteWarmExecuted(idx > 0 && breakReturn)
			}
		}
	}
	now := s.o.Now()
	s.warmMu.Lock()
	for k, t := range s.warmed {
		if now.Sub(t) > time.Hour {
			delete(s.warmed, k)
		}
	}
	s.warmMu.Unlock()
}

func (s *Service) expireSessions() {
	now := s.o.Now()
	s.mu.Lock()
	var stale []*session
	for id, ss := range s.sessions {
		// A bounded startup or programme preparation may hold the session
		// lock across I/O. Never let it block monitoring or other viewers.
		if !ss.mu.TryLock() {
			continue
		}
		expired := s.sessionLivenessLocked(ss, now).Expired
		ss.mu.Unlock()
		if expired {
			stale = append(stale, ss)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, ss := range stale {
		s.sessionLog(ss).Info("live session expired")
		ss.closeExpired(s)
		if s.o.SessionExpired != nil {
			s.o.SessionExpired(ss.info.ID)
		}
	}
	if len(stale) > 0 {
		s.notifySessionsChanged()
	}
}
func (s *Service) Channels(ctx context.Context) ([]Channel, error) {
	cs, err := s.allChannels(ctx)
	for i := range cs {
		cs[i].Health = s.sweepState(cs[i])
	}
	return cs, err
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// channels reads every channel in number order straight from db (a
// transaction sees its own writes). Request paths use allChannels.
func channels(ctx context.Context, db queryer) ([]Channel, error) {
	rows, err := db.QueryContext(ctx, `SELECT data FROM livetv_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	out, err := decodeChannelRows(rows)
	if err != nil {
		return nil, err
	}
	sortChannels(out)
	return out, nil
}
func (s *Service) Channel(ctx context.Context, id string) (Channel, error) {
	s.virtualGuide.mu.RLock()
	entry := s.virtualGuide.entries[id]
	var snapshotEpoch int64
	if entry != nil {
		snapshotEpoch = entry.epoch
	}
	s.virtualGuide.mu.RUnlock()
	if entry != nil {
		var epoch int64
		if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&epoch); err != nil {
			return Channel{}, err
		}
		if epoch == snapshotEpoch {
			return entry.channel, nil
		}
		s.wakeVirtualGuide()
	}

	var b []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_channels WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, core.ErrNotFound
	}
	var c Channel
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}
func itemFromTitle(t core.Title) Item {
	var released int64
	if !t.Released.IsZero() {
		released = t.Released.UnixMilli()
	}
	enf := contentrating.EffectiveEnforcement(t.EnforcementRating, t.ContentRating, t.Adult, t.Keywords)
	return Item{Key: t.Key, Title: t.Title, DurationMS: t.Runtime.Milliseconds(), Summary: t.Summary, Rating: t.ContentRating, EnforcementRating: enf, Thumb: t.PosterPath, Art: t.BackdropPath, ShowKey: t.ShowKey, Season: t.Season, Episode: t.Episode, ReleasedMS: released}
}
func (s *Service) expand(ctx context.Context, c *Channel) error {
	var prepare func([]ProgramSource, string)
	prepare = func(sources []ProgramSource, prefix string) {
		for i := range sources {
			source := &sources[i]
			if source.ID == "" {
				source.ID = fmt.Sprintf("%s-source-%d", prefix, i)
			}
			if (source.Strip || source.Kind == "lineup") && source.StartDate == "" {
				source.StartDate = s.o.Now().In(zoneOf(c.Timezone)).Format("2006-01-02")
			}
			prepare(source.Sources, source.ID)
		}
	}
	prepare(c.Sources, c.ID)
	for i := range c.Blocks {
		b := &c.Blocks[i]
		if b.ID == "" {
			b.ID = fmt.Sprintf("%s-block-%d", c.ID, i)
		}
		prepare(b.Sources, b.ID)
	}
	for i := range c.Pins {
		p := &c.Pins[i]
		if p.ID == "" {
			p.ID = fmt.Sprintf("%s-pin-%d", c.ID, i)
		}
		prepare(p.Sources, p.ID)
	}
	if c.Personal != nil {
		return nil
	}
	items, err := s.resolveProgramming(ctx, c.Items, c.Collection, c.Filter, c.Sources, c.Watched)
	if err != nil {
		return err
	}
	if c.Collection != 0 || c.Filter != nil || len(c.Sources) > 0 || c.Watched != "" {
		c.Items = items
	}
	// Prime time picks its series first; a series then has one home slot.
	ctx = context.WithValue(ctx, claimedSeriesKey{}, map[core.RatingKey]bool{})
	order := make([]int, len(c.Blocks))
	for i := range order {
		order[i] = i
	}
	prime := func(b Block) bool { return b.Start >= 19*60 && b.Start < 23*60 }
	sort.SliceStable(order, func(i, j int) bool { return prime(c.Blocks[order[i]]) && !prime(c.Blocks[order[j]]) })
	for _, i := range order {
		b := &c.Blocks[i]
		if b.Collection == 0 && b.Filter == nil && len(b.Sources) == 0 && b.Watched == "" {
			continue
		}
		blockItems, err := s.resolveProgramming(ctx, b.Items, b.Collection, b.Filter, b.Sources, b.Watched)
		if err != nil {
			return err
		}
		b.Items = blockItems
	}
	for i := range c.Pins {
		pin := &c.Pins[i]
		if len(pin.Sources) == 0 {
			continue
		}
		pin.Items, err = s.resolveProgramming(ctx, pin.Items, 0, nil, pin.Sources, "")
		if err != nil {
			return err
		}
	}
	return nil
}

// resolveProgramming turns a collection, smart filter or weighted mix into
// a stable item list. With none of those set, items pass through (watched
// still applies when asked). Shows become their episodes, in catalogue order.
func (s *Service) resolveProgramming(ctx context.Context, existing []Item, collection core.RatingKey, filter *core.Filter, sources []ProgramSource, watched string) ([]Item, error) {
	var items []Item
	var err error
	switch {
	case len(sources) > 0:
		parts := make([][]Item, len(sources))
		weights := make([]int, len(sources))
		for i := range sources {
			src := &sources[i]
			parts[i], err = s.expandProgramSource(ctx, src, 0)
			if err != nil {
				return nil, err
			}
			if src.Kind != "lineup" {
				// A lineup's episodes live on its series; its own list is a preview.
				src.Items = append([]Item(nil), parts[i]...)
			}
			weights[i] = src.Weight
			if weights[i] <= 0 {
				weights[i] = 1
			}
			if weights[i] > 20 {
				weights[i] = 20
			}
		}
		items = interleave(parts, weights)
	case collection != 0 || filter != nil:
		items, err = s.expandQuery(ctx, collection, filter)
		if err != nil {
			return nil, err
		}
	default:
		items = append([]Item(nil), existing...)
	}
	if watched == "" {
		return items, nil
	}
	return s.filterWatched(ctx, items, watched)
}

func interleave(parts [][]Item, weights []int) []Item {
	idx := make([]int, len(parts))
	out := []Item{}
	for len(out) < 10000 {
		progressed := false
		for i, p := range parts {
			for n := 0; n < weights[i] && idx[i] < len(p); n++ {
				out = append(out, p[idx[i]])
				idx[i]++
				progressed = true
				if len(out) >= 10000 {
					return out
				}
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

func (s *Service) expandQuery(ctx context.Context, collection core.RatingKey, filter *core.Filter) ([]Item, error) {
	var page core.Page
	var err error
	if collection != 0 {
		if s.o.Collections == nil {
			return nil, fmt.Errorf("collections unavailable")
		}
		page, err = s.o.Collections.Items(ctx, &core.User{IsOwner: true}, collection, 0, 2000)
	} else {
		if s.o.Catalog == nil || filter == nil {
			return nil, fmt.Errorf("catalogue unavailable")
		}
		for len(page.Items) < 2000 {
			var p core.Page
			if p, err = s.o.Catalog.Browse(ctx, core.BrowseQuery{Filter: *filter, Sort: core.SortTitle, Offset: len(page.Items), Limit: 2000 - len(page.Items)}); err != nil {
				break
			}
			page.Total = p.Total
			page.Items = append(page.Items, p.Items...)
			if len(p.Items) == 0 || len(page.Items) >= p.Total {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	items := []Item{}
	for _, t := range page.Items {
		ts := []core.Title{t}
		if t.Type == core.TypeShow {
			if s.o.Catalog == nil {
				return nil, fmt.Errorf("catalogue unavailable")
			}
			ts, err = s.o.Catalog.Grandchildren(ctx, t.Key)
			if err != nil {
				return nil, err
			}
		}
		for _, t := range ts {
			if !t.Adult && t.Runtime > 0 && (t.Type == core.TypeMovie || t.Type == core.TypeEpisode) {
				items = append(items, itemFromTitle(t))
			}
			if len(items) >= 10000 {
				return nil, fmt.Errorf("channel exceeds 10000 programmes")
			}
		}
	}
	return items, nil
}

func (s *Service) filterWatched(ctx context.Context, items []Item, watched string) ([]Item, error) {
	if watched != "watched" && watched != "unwatched" {
		return nil, fmt.Errorf("watched must be watched or unwatched")
	}
	if s.o.DB == nil {
		return nil, fmt.Errorf("watch history unavailable")
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT DISTINCT rating_key FROM state_history WHERE completed=1`)
	if err != nil {
		return nil, fmt.Errorf("watch history unavailable")
	}
	defer rows.Close()
	seen := map[core.RatingKey]struct{}{}
	for rows.Next() {
		var key int64
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		seen[core.RatingKey(key)] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var out []Item
	for _, it := range items {
		_, hit := seen[it.Key]
		if watched == "watched" && hit || watched == "unwatched" && !hit {
			out = append(out, it)
		}
	}
	return out, nil
}
func checkPlay(mode, align, fit string, repeat int, watched string) error {
	switch mode {
	case "", "order", "shuffle", "random", "marathon", "newest":
	default:
		return fmt.Errorf("unknown playout")
	}
	switch align {
	case "", "hour", "half":
	default:
		return fmt.Errorf("alignment must be hour or half hour")
	}
	switch fit {
	case "", "cut", "pad", "shift":
	default:
		return fmt.Errorf("fit must be cut or pad")
	}
	if repeat < 0 || repeat > 30 {
		return fmt.Errorf("repeat window must be 0–30 days")
	}
	switch watched {
	case "", "watched", "unwatched":
	default:
		return fmt.Errorf("watched must be watched or unwatched")
	}
	return nil
}

func blocksOverlap(a, b Block) bool {
	if a.End <= b.Start || b.End <= a.Start {
		return false
	}
	if len(a.Days) == 0 || len(b.Days) == 0 {
		return true
	}
	for _, d := range a.Days {
		for _, e := range b.Days {
			if d == e {
				return true
			}
		}
	}
	return false
}

func validateChannel(c *Channel) error {
	apps, err := normalizeHiddenApps(c.HiddenApps)
	if err != nil {
		return err
	}
	c.HiddenApps = apps
	shown, err := normalizeHiddenApps(c.ShownApps)
	if err != nil {
		return err
	}
	if len(shown) > 0 && len(apps) > 0 {
		shown = slices.DeleteFunc(shown, func(app string) bool { return slices.Contains(apps, app) })
		if len(shown) == 0 {
			shown = nil
		}
	}
	c.ShownApps = shown
	if c.Look != nil {
		if err := c.Look.Bug.Validate(); err != nil {
			return err
		}
		if err := validateLookRundown(c.Look); err != nil {
			return err
		}
	}
	if c.Look != nil && c.Look.BreakPattern != nil {
		if e := c.Look.BreakPattern.Validate(); e != nil {
			return e
		}
		if c.Look.ReelID == "" {
			return fmt.Errorf("commercial pods require a reel")
		}
		c.Look.BreakContent = "reel"
		c.Look.Placement = "insert"
	}
	if err := validatePersonal(*c); err != nil {
		return err
	}
	if !config.ValidLiveTVStability(c.Stability) {
		return fmt.Errorf("invalid stability preset")
	}
	for _, slot := range c.Clock {
		if !strings.HasPrefix(slot.SegmentID, "plugin:") || slot.Minute < 0 || slot.Minute > 59 || slot.EveryMinutes < 0 || slot.EveryMinutes > 1440 || slot.From < 0 || slot.Until > 1440 || slot.Until <= slot.From {
			return fmt.Errorf("invalid clock slot")
		}
		for _, d := range slot.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("invalid clock weekday")
			}
		}
	}

	c.Name = strings.TrimSpace(c.Name)
	c.Number = strings.TrimSpace(c.Number)
	if c.Name == "" || c.Number == "" || len(c.Name) > 200 || len(c.Number) > 32 {
		return fmt.Errorf("channel needs a name and number")
	}
	if c.Kind != "virtual" && c.Kind != "iptv" {
		return fmt.Errorf("kind must be virtual or iptv")
	}
	if c.Kind == "iptv" {
		m, err := normalizeChannelInputMerge(c.InputMerge)
		if err != nil {
			return err
		}
		c.InputMerge = m
		if err := normalizeStreamSources(c); err != nil {
			return err
		}
	} else {
		c.InputMerge = nil
		c.MergedInto = ""
		c.SuppressedAutoBackups = nil
		c.OwnerStreamSources = false
	}
	if c.Personal == nil && c.Kind == "virtual" && len(c.Items) == 0 && len(c.Blocks) == 0 && len(c.Pins) == 0 && !c.OffAir && len(c.Fallback) == 0 {
		return fmt.Errorf("virtual channel needs base programming")
	}
	if err := checkPlay(c.Mode, c.Align, c.Fit, c.RepeatDays, c.Watched); err != nil {
		return err
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("unknown timezone")
		}
	}
	check := func(items []Item) error {
		if len(items) > 10000 {
			return fmt.Errorf("too many items")
		}
		for _, it := range items {
			if len(it.BreakPoints) > 256 {
				return fmt.Errorf("maximum 256 programme break points")
			}
			var lastMeta int64
			for _, p := range it.BreakPoints {
				if p.AtMS <= lastMeta || p.AtMS >= it.DurationMS || p.Confidence < 0 || p.Confidence > 1 {
					return fmt.Errorf("invalid programme break point metadata")
				}
				switch p.Source {
				case "chapters", "detected", "even":
				default:
					return fmt.Errorf("invalid break point source")
				}
				lastMeta = p.AtMS
			}
			if len(it.BreakPointsMS) > 256 {
				return fmt.Errorf("maximum 256 programme break points")
			}
			var last int64
			for _, point := range it.BreakPointsMS {
				if point <= last || point >= it.DurationMS {
					return fmt.Errorf("programme break points must be sorted inside its duration")
				}
				last = point
			}
			if (it.Key <= 0 && !strings.HasPrefix(it.Slate, "plugin:")) || it.DurationMS < 1000 || it.DurationMS > 24*60*60*1000 {
				return fmt.Errorf("programme needs a catalogue key and duration of 1 second to 24 hours")
			}
		}
		return nil
	}
	if err := check(c.Items); err != nil {
		return err
	}
	for i, b := range c.Blocks {
		if b.Start < 0 || b.End <= b.Start || b.End > 1440 {
			return fmt.Errorf("time blocks must be within 00:00–24:00")
		}
		if c.Personal == nil && len(b.Items) == 0 && len(c.Fallback) == 0 && !c.OffAir {
			return fmt.Errorf("UTC time blocks must be nonoverlapping, within 00:00–24:00, and contain programming")
		}
		if err := check(b.Items); err != nil {
			return err
		}
		if err := check(b.Filler); err != nil {
			return err
		}
		if err := validateLookRundown(b.Breaks); err != nil {
			return err
		}
		if err := checkPlay(b.Mode, b.Align, b.Fit, b.RepeatDays, b.Watched); err != nil {
			return err
		}
		for _, d := range b.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("time block day must be 0–6")
			}
		}
		for j := i + 1; j < len(c.Blocks); j++ {
			if blocksOverlap(b, c.Blocks[j]) {
				return fmt.Errorf("UTC time blocks must be nonoverlapping, within 00:00–24:00, and contain programming")
			}
		}
	}
	for _, pin := range c.Pins {
		if err := validateLookRundown(pin.Breaks); err != nil {
			return err
		}
	}
	if err := validateProgramming(c, check); err != nil {
		return err
	}
	if err := check(c.Filler); err != nil {
		return err
	}
	if err := check(c.Fallback); err != nil {
		return err
	}
	if c.Epoch.IsZero() {
		c.Epoch = time.Unix(0, 0).UTC()
	}
	return nil
}

// DraftChannel uses the normal channel expansion and validation without saving.
// It is demand-driven by an owner's explicit channel proposal.
func (s *Service) DraftChannel(ctx context.Context, c Channel) (Channel, error) {
	if err := s.expand(ctx, &c); err != nil {
		return c, err
	}
	return c, validateChannel(&c)
}

func (s *Service) SaveChannel(ctx context.Context, c Channel) (Channel, error) {
	if c.ID == "" {
		c.ID = randomID()
	}
	var old Channel
	var hasOld bool
	if c.LogoMatched || c.Kind == "iptv" {
		if prev, err := s.Channel(ctx, c.ID); err == nil {
			old = prev
			hasOld = true
			c.ProviderAdult = old.ProviderAdult
			c.ProviderNumber = old.ProviderNumber
			c.ProviderGuideID = old.ProviderGuideID
		}
		if !hasOld || !old.LogoMatched || old.Logo != c.Logo {
			c.LogoMatched = false
		}
	}
	// A metadata save must not re-query collections or personal providers.
	s.virtualGuide.mu.RLock()
	previous := s.virtualGuide.entries[c.ID]
	s.virtualGuide.mu.RUnlock()
	sameProgramming := previous != nil && snapshotInputFingerprint(c, 0) == snapshotInputFingerprint(previous.channel, 0)
	if !sameProgramming {
		if err := s.expand(ctx, &c); err != nil {
			return c, err
		}
	}
	reconcileSavedAutoBackups(&c, old, hasOld)
	if err := validateChannel(&c); err != nil {
		return c, err
	}
	invalidated, err := s.saveChannelWithMerges(ctx, &c)
	if err == nil {
		if !c.Enabled {
			s.cancelGraphics(c.ID)
		}
		s.captureBreakOwner(ctx, c.ID)
		for _, cid := range invalidated {
			s.invalidate(cid, "")
		}
		if err = s.refreshVirtualGuideIfWarm(ctx); err != nil {
			return c, err
		}
		s.wakeSourceRefresh()
		s.wakeHealthSweep()
		s.wakeBreakLoop()
		s.wakePersonalLoop()
		s.bumpLineup()
	}
	return c, err
}

// cancelGraphics drops a channel's queued and running graphics renders
// when the graphics service supports it: a disabled or deleted channel
// must stop rendering at once, not at the next planning tick.
func (s *Service) cancelGraphics(channelID string) {
	if c, ok := s.o.Graphics.(interface{ CancelChannel(string) }); ok {
		c.CancelChannel(channelID)
	}
}

func (s *Service) DeleteChannel(ctx context.Context, id string) error {
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var invalidated []string
	if _, err = tx.ExecContext(ctx, `DELETE FROM livetv_channels WHERE id=?`, id); err != nil {
		return err
	}
	mergedChanged, inv, err := s.reconcileInputMergesTx(ctx, tx)
	if err != nil {
		return err
	}
	autoChanged, autoInv, err := s.reconcileAutoBackupsTx(ctx, tx, 0, nil)
	if err != nil {
		return err
	}
	invalidated = append(inv, autoInv...)
	if mergedChanged || autoChanged {
		if _, err = tx.ExecContext(ctx, `UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1`); err != nil {
			return err
		}
	}
	err = tx.Commit()
	if err == nil {
		for _, cid := range invalidated {
			s.invalidate(cid, "")
		}
		s.cancelGraphics(id)
		s.mu.Lock()
		delete(s.breakOwners, id)
		s.mu.Unlock()
		s.invalidate(id, "")
		if err = s.refreshVirtualGuideIfWarm(ctx); err != nil {
			return err
		}
		s.wakeSourceRefresh()
		s.wakeHealthSweep()
		s.wakeBreakLoop()
		s.wakePersonalLoop()
		s.bumpLineup()
	}
	return err
}

// SetOnChange registers the Plex lineup refresh. Save, delete and source
// import call it after the database commit.
func (s *Service) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// LineupChanged is the DVR refreshedAt clients poll. It moves whenever the
// channel set changes, including twice in the same wall-clock second.
func (s *Service) LineupChanged() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lineupUnix == 0 {
		return time.Time{}
	}
	return time.Unix(s.lineupUnix, 0).UTC()
}

func (s *Service) bumpLineup() {
	s.Enabled() // reconcile channel existence after every committed lineup change
	s.wakeVirtualGuide()
	select {
	case s.graphicsWake <- struct{}{}:
	default:
	}
	s.mu.Lock()
	now := s.o.Now().Unix()
	if now <= s.lineupUnix {
		now = s.lineupUnix + 1
	}
	s.lineupUnix = now
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	s.notifySourcesChanged()
}
func (s *Service) Policy(ctx context.Context, userID int64) (Policy, error) {
	s.policyReads.Add(1)
	p, err := readPolicy(ctx, s.o.DB, userID)
	p.indexChannels()
	return p, err
}
func (s *Service) SavePolicy(ctx context.Context, p Policy) error {
	if p.UserID <= 0 {
		return fmt.Errorf("userId must be positive")
	}
	err := savePolicy(ctx, s.o.DB, p)
	if err == nil {
		if err = s.refreshVirtualGuideIfWarm(ctx); err != nil {
			return err
		}
		s.wakePersonalLoop()
		// The viewer's channel set changed: clients refresh their lineup.
		s.bumpLineup()
	}
	return err
}
func (s *Service) allowed(ctx context.Context, u *core.User, c Channel, p *Programme) bool {
	if !s.Enabled() {
		return false
	}
	var lookup func(context.Context, core.RatingKey) (*core.Title, error)
	if s.o.Catalog != nil {
		lookup = s.o.Catalog.Get
		if c.Personal != nil {
			if cached, ok := s.o.Catalog.(interface {
				CachedGet(context.Context, core.RatingKey) (*core.Title, error)
			}); ok {
				lookup = cached.CachedGet
			}
		}
	}
	return s.allowedWithLookup(ctx, u, c, p, lookup, false)
}

type loadedPolicyKey struct{}

type loadedPolicy struct {
	id   int64
	user *core.User
	pol  Policy
}

// withPolicy reads the viewer's Live TV policy and validates the compiled
// owner-group rule index in one statement, reusing both for the request.
func (s *Service) withPolicy(ctx context.Context, u *core.User) (context.Context, Policy, error) {
	if u == nil {
		return ctx, Policy{}, nil
	}
	if loaded, ok := ctx.Value(loadedPolicyKey{}).(loadedPolicy); ok && loaded.id == u.ID {
		return ctx, loaded.pol, nil
	}
	s.policyReads.Add(1)
	prevGroupRev, prevGroupIdx := s.getOwnerGroupCacheState()
	prevHasGroups := 0
	if prevGroupIdx != nil && len(prevGroupIdx.groups) > 0 {
		prevHasGroups = 1
	}
	var rev int64
	var rawPolicy, rawGroups string
	err := s.o.DB.QueryRowContext(
		ctx,
		`SELECT generation,COALESCE((SELECT data FROM livetv_policies WHERE user_id=?),''),CASE WHEN generation=? THEN '' ELSE COALESCE((SELECT data FROM livetv_owner_groups WHERE id=1),'[]') END FROM livetv_channel_epoch WHERE id=1 AND (EXISTS(SELECT 1 FROM livetv_policies WHERE user_id=?) OR (generation!=? AND (?=1 OR EXISTS(SELECT 1 FROM livetv_owner_groups WHERE id=1))))`,
		u.ID, prevGroupRev, u.ID, prevGroupRev, prevHasGroups,
	).Scan(&rev, &rawPolicy, &rawGroups)
	pol := Policy{UserID: u.ID}
	groupIdx := prevGroupIdx
	if errors.Is(err, sql.ErrNoRows) {
		if groupIdx == nil {
			groupIdx = compileOwnerGroupIndex(nil)
		}
	} else if err != nil {
		return ctx, Policy{}, err
	} else {
		if rawPolicy != "" {
			if err := json.Unmarshal([]byte(rawPolicy), &pol); err != nil {
				return ctx, Policy{}, err
			}
			pol.UserID = u.ID
		}
		if groupIdx == nil || rev != prevGroupRev {
			decoded, err := s.loadOwnerGroupIndex(ctx, rev, rawGroups)
			if err != nil {
				return ctx, Policy{}, err
			}
			groupIdx = decoded
			s.setOwnerGroupCacheState(rev, groupIdx)
		}
	}
	pol.indexChannels()
	ctx = contextWithOwnerGroupIndex(ctx, groupIdx)
	return context.WithValue(ctx, loadedPolicyKey{}, loadedPolicy{id: u.ID, user: u, pol: pol}), pol, nil
}

func (s *Service) allowedWithLookup(ctx context.Context, u *core.User, c Channel, p *Programme, lookup func(context.Context, core.RatingKey) (*core.Title, error), strict bool) bool {
	return s.allowedWithLookupOpts(ctx, u, c, p, lookup, strict, false)
}

func (s *Service) allowedWithLookupOpts(ctx context.Context, u *core.User, c Channel, p *Programme, lookup func(context.Context, core.RatingKey) (*core.Title, error), strict, skipSweep bool) bool {
	if u == nil || !c.Enabled || c.MergedInto != "" {
		return false
	}
	if app := core.LiveApp(ctx); app != "" && slices.Contains(c.HiddenApps, app) {
		return false
	}
	if !skipSweep && !u.IsOwner && s.sweepState(c) == "unavailable" {
		return false
	}
	var pol Policy
	var err error
	ctx, pol, err = s.withPolicy(ctx, u)
	if err != nil {
		return false
	}
	if !s.publishedTo(ctx, c) || s.sourcePaused(ctx, c) {
		return false
	}
	if c.Personal != nil && (s.o.Picker == nil || (pol.Personal != nil && pol.Personal.Enabled != nil && !*pol.Personal.Enabled)) {
		return false
	}
	if !u.IsOwner && !pol.Permits(c.ID, c.Private) {
		return false
	}
	var rating string
	if u.MaxContentRating != "" || pol.MaxRating != "" {
		rating = s.channelContentRating(ctx, c, p)
	}
	return s.allowedProgrammeContent(ctx, u, c, p, pol, rating, lookup, strict)
}

func (s *Service) allowedProgrammeContent(ctx context.Context, u *core.User, c Channel, p *Programme, pol Policy, rating string, lookup func(context.Context, core.RatingKey) (*core.Title, error), strict bool) bool {
	if c.Kind != "iptv" && p != nil && strings.HasPrefix(p.Item.Slate, "plugin:") && contentrating.EnforceLevelOf(p.Item.Rating) == contentrating.Unrated && (u.MaxContentRating != "" || pol.MaxRating != "") {
		return false
	}
	if u.MaxContentRating != "" || pol.MaxRating != "" {
		if p != nil && p.Item.EnforcementRating != "" && (p.Item.EnforcementRating == contentrating.UncertifiedAdult || contentrating.EnforceLevelOf(p.Item.EnforcementRating) > contentrating.EnforceLevelOf(rating)) {
			rating = p.Item.EnforcementRating
		}
		if contentrating.EnforceExceeds(rating, u.MaxContentRating) || contentrating.EnforceExceeds(rating, pol.MaxRating) {
			return false
		}
	}
	if p != nil && p.Item.Key != 0 && (lookup != nil || strict) {
		if lookup == nil {
			return false
		}
		checkTitle := func(t *core.Title) bool {
			if t == nil || t.Adult {
				return false
			}
			enf := contentrating.EffectiveEnforcement(t.EnforcementRating, t.ContentRating, t.Adult, t.Keywords)
			if contentrating.EnforceExceeds(enf, u.MaxContentRating) || contentrating.EnforceExceeds(enf, pol.MaxRating) {
				return false
			}
			if len(u.AllowedLibraries) > 0 {
				section := core.LibraryMovies
				if t.Type != core.TypeMovie {
					section = core.LibraryShows
				}
				ok := false
				for _, id := range u.AllowedLibraries {
					if id == section {
						ok = true
					}
				}
				if !ok {
					return false
				}
			}
			if s.o.Allowed != nil && !s.o.Allowed(u, t) {
				return false
			}
			return true
		}
		t, err := lookup(ctx, p.Item.Key)
		if err != nil || !checkTitle(t) || (strict && t.Key != p.Item.Key) {
			return false
		}
		if strict && t.ShowKey != 0 {
			show, err := lookup(ctx, t.ShowKey)
			if err != nil || show == nil || show.Key != t.ShowKey || !checkTitle(show) {
				return false
			}
		}
	}
	return true
}

// AllowedChannelCached checks one channel and its current programme using
// local title facts only. Missing programme title or inherited show facts fail
// closed. The supplied lookup must not perform network or release resolution.
func (s *Service) AllowedChannelCached(ctx context.Context, u *core.User, id string, cached func(context.Context, core.RatingKey) (*core.Title, error)) error {
	c, err := s.Channel(ctx, id)
	if err != nil {
		return err
	}
	now := s.o.Now()
	programmes, err := s.programmes(ctx, s.personalChannel(ctx, c, u), now, now.Add(time.Second))
	if err != nil {
		return err
	}
	var current *Programme
	if len(programmes) > 0 {
		current = &programmes[0]
	}
	if !s.allowedWithLookup(ctx, u, c, current, cached, true) {
		return core.ErrNotFound
	}
	return nil
}
func (s *Service) AllowedChannels(ctx context.Context, u *core.User) ([]Channel, error) {
	return s.allowedChannels(ctx, u, nil)
}

// AllowedChannelsForIDs checks a group's selected channels without waiting for
// unrelated channels to finish their background preparation.
func (s *Service) AllowedChannelsForIDs(ctx context.Context, u *core.User, ids []string) ([]Channel, error) {
	return s.allowedChannels(ctx, u, ids)
}

func (s *Service) allowedChannels(ctx context.Context, u *core.User, ids []string) ([]Channel, error) {
	if !s.Enabled() {
		return nil, nil
	}
	ctx, pol, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, err
	}
	var cs []Channel
	if ids == nil {
		cs, err = s.Channels(ctx)
		if err == nil {
			if _, idx := s.getOwnerGroupCacheState(); idx != nil {
				ctx = contextWithOwnerGroupIndex(ctx, idx)
			}
		}
	} else {
		var selected map[string]Channel
		selected, err = s.readChannelsByIDs(ctx, ids, make(map[string]Channel, len(ids)))
		for _, id := range ids {
			if c, ok := selected[id]; ok {
				cs = append(cs, c)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return s.filterAllowedChannels(ctx, u, pol, cs)
}

func (s *Service) filterAllowedChannels(ctx context.Context, u *core.User, pol Policy, cs []Channel) ([]Channel, error) {
	rated := u != nil && (strings.TrimSpace(u.MaxContentRating) != "" || strings.TrimSpace(pol.MaxRating) != "")
	now := s.o.Now()
	var guided []Channel
	var positions []int
	audience := make([]bool, len(cs))
	for i, c := range cs {
		audience[i] = s.audienceAllowed(ctx, u, c)
		if audience[i] && (rated || c.Kind == "virtual") {
			guided = append(guided, c)
			positions = append(positions, i)
		}
	}
	current, err := s.programmesAt(ctx, guided, now)
	if err != nil {
		return nil, err
	}
	airing := make(map[int]Programme, len(current))
	for i, p := range current {
		airing[positions[i]] = p
	}
	policy, err := s.loadProgrammePolicy(ctx, current)
	if err != nil {
		return nil, err
	}
	out := []Channel{}
	for i, c := range cs {
		if !audience[i] {
			continue
		}
		var programme *Programme
		if p, ok := airing[i]; ok {
			programme = &p
		}
		if s.allowedProgrammeWithLookup(ctx, u, c, programme, policy.lookup(c)) {
			c.StreamURL = ""
			c.redactArchiveSource()
			c.StreamSources = nil
			out = append(out, s.personalChannel(ctx, c, u))
		}
	}
	return out, nil
}

// AllowedChannel resolves one channel by id or number and applies the same
// policy check as AllowedChannels without loading the lineup. Plex tune used
// the whole list only to find the requested channel.
func (s *Service) AllowedChannel(ctx context.Context, u *core.User, key string) (Channel, error) {
	if !s.Enabled() {
		return Channel{}, core.ErrNotAvailable
	}
	ctx, _, err := s.withPolicy(ctx, u)
	if err != nil {
		return Channel{}, err
	}
	c, err := s.Channel(ctx, key)
	if err == nil {
		return s.allowedChannelRecord(ctx, u, c)
	}
	if !errors.Is(err, core.ErrNotFound) {
		return Channel{}, err
	}
	all, err := s.channelSummaries(ctx)
	if err != nil {
		return Channel{}, err
	}
	for _, summary := range all {
		if summary.Number != key || !summary.Enabled || summary.MergedInto != "" {
			continue
		}
		c, err = s.Channel(ctx, summary.ID)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return Channel{}, err
		}
		c, err = s.allowedChannelRecord(ctx, u, c)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		return c, err
	}
	return Channel{}, core.ErrNotFound
}

func (s *Service) allowedChannelRecord(ctx context.Context, u *core.User, c Channel) (Channel, error) {
	c = s.personalChannel(ctx, c, u)
	p, ok, err := s.programmeAt(ctx, s.personalChannel(ctx, c, u), s.o.Now())
	if err != nil {
		return Channel{}, err
	}
	var current *Programme
	if ok {
		current = &p
	}
	if !s.allowedProgramme(ctx, u, c, current) {
		return Channel{}, core.ErrNotFound
	}
	c.StreamURL = ""
	c.redactArchiveSource()
	c.StreamSources = nil
	return c, nil
}

// ChannelPermitted decides one channel. A memo on ctx, when present, answers
// repeat checks for the same channel without another guide read. Callers that
// are not looking at a live channel must not call this.
func (s *Service) ChannelPermitted(ctx context.Context, u *core.User, id string) (bool, error) {
	if !s.Enabled() {
		return false, nil
	}
	if u == nil || id == "" {
		return false, nil
	}
	if memo, ok := channelMemoFrom(ctx); ok {
		if decided, hit := memo.get(id); hit {
			return decided, nil
		}
	}
	c, err := s.Channel(ctx, id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			rememberChannel(ctx, id, false)
			return false, nil
		}
		return false, err
	}
	p, ok, err := s.programmeAt(ctx, s.personalChannel(ctx, c, u), s.o.Now())
	if err != nil {
		return false, err
	}
	var current *Programme
	if ok {
		current = &p
	}
	allowed := s.allowedProgramme(ctx, u, c, current)
	rememberChannel(ctx, id, allowed)
	return allowed, nil
}

// ChannelPermissions is the set-based form of ChannelPermitted for one row
// policy pass. Unknown and denied channels are both absent from the result.
func (s *Service) ChannelPermissions(ctx context.Context, u *core.User, ids []string) (map[string]bool, error) {
	channels, err := s.PermittedChannels(ctx, u, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(channels))
	for id := range channels {
		out[id] = true
	}
	return out, nil
}

// PermittedChannels reads selected channel records and their current
// programmes in batches, returning the records that pass both policies.
func (s *Service) PermittedChannels(ctx context.Context, u *core.User, ids []string) (map[string]Channel, error) {
	out := make(map[string]Channel, len(ids))
	if u == nil || len(ids) == 0 {
		return out, nil
	}
	ctx, _, err := s.withPolicy(core.WithCatalogViewer(ctx, u), u)
	if err != nil {
		return nil, err
	}
	selected, err := s.readChannelsByIDs(ctx, ids, make(map[string]Channel, len(ids)))
	if err != nil {
		return nil, err
	}
	channels := make([]Channel, 0, len(selected))
	for _, c := range selected {
		if s.audienceAllowed(ctx, u, c) {
			channels = append(channels, c)
		}
	}
	if len(channels) == 0 {
		return out, nil
	}
	programmes, err := s.programmesAt(ctx, channels, s.o.Now())
	if err != nil {
		return nil, err
	}
	policy, err := s.loadProgrammePolicy(ctx, programmes)
	if err != nil {
		return nil, err
	}
	for i, channel := range channels {
		var current *Programme
		if p, ok := programmes[i]; ok {
			current = &p
		}
		if s.allowedProgrammeWithLookup(ctx, u, channel, current, policy.lookup(channel)) {
			channel.StreamURL = ""
			channel.redactArchiveSource()
			channel.StreamSources = nil
			out[channel.ID] = channel
		}
	}
	return out, nil
}

func (s *Service) loadProgrammePolicy(ctx context.Context, programmes map[int]Programme) (*searchPolicyCache, error) {
	policy := newSearchPolicyCache(s)
	keys := make([]core.RatingKey, 0, len(programmes))
	for _, p := range programmes {
		if p.policyTag == nil && p.Item.Key != 0 {
			keys = append(keys, p.Item.Key)
		}
	}
	return policy, policy.preload(ctx, keys)
}

// programmeAt reads the single programme airing at a time. The primary key
// is (source, channel, start), so the latest start at or before that time is
// one indexed row rather than a walk of the channel's guide.
func (s *Service) programmeAt(ctx context.Context, c Channel, at time.Time) (Programme, bool, error) {
	if c.Personal != nil && c.personalViewer == nil {
		if v, ok := ctx.Value(loadedPolicyKey{}).(loadedPolicy); ok {
			c = s.personalChannel(ctx, c, v.user)
		}
	}
	if c.Kind == "virtual" {
		if _, err := s.withVirtualGuide(ctx, []Channel{c}); err != nil {
			return Programme{}, false, err
		}
		ps := s.viewerSchedule(ctx, c, at, at.Add(time.Second))
		if len(ps) == 0 {
			return Programme{}, false, nil
		}
		return ps[0], true, nil
	}
	var b []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND start_ms<=? ORDER BY start_ms DESC LIMIT 1`, guideSource(c), c.GuideID, at.UnixMilli()).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return Programme{}, false, nil
	}
	if err != nil {
		return Programme{}, false, err
	}
	var p Programme
	if err = json.Unmarshal(b, &p); err != nil {
		return Programme{}, false, err
	}
	if !p.End.After(at) {
		return Programme{}, false, nil
	}
	p.ChannelID = c.ID
	p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
	return p, true, nil
}

// LiveProgrammes returns every guide programme currently airing on channel c
// at time at, ordered by start_ms ascending. Uses the (source_id, channel_id,
// end_ms, start_ms) index so cost depends only on concurrent programmes on c.
func (s *Service) LiveProgrammes(ctx context.Context, c Channel, at time.Time) ([]Programme, error) {
	if c.GuideID == "" {
		return nil, nil
	}
	srcID := guideSource(c)
	if srcID == "" {
		return nil, nil
	}
	// Allow up to 999ms past at so simultaneous events staggered by +1ms
	// within the same second are all seen as live right at kickoff.
	rows, err := s.o.DB.QueryContext(ctx,
		`SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND end_ms>? AND start_ms<=? ORDER BY start_ms ASC`,
		srcID, c.GuideID, at.UnixMilli(), at.UnixMilli()+999)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Programme
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var p Programme
		if err := json.Unmarshal(b, &p); err != nil {
			continue
		}
		p.ChannelID = c.ID
		p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		out = append(out, p)
	}
	return out, rows.Err()
}

// programmesAtBatch bounds the channels one programmesAt statement covers
// (two parameters each).
const programmesAtBatch = 200

// programmesAt is programmeAt for every channel in cs, keyed by index. Guide
// channels are read in one statement per batch, each channel still an
// indexed seek on the primary key, so a large lineup costs a handful of
// statements instead of one per channel.
func (s *Service) programmesAt(ctx context.Context, cs []Channel, at time.Time) (map[int]Programme, error) {
	out := make(map[int]Programme, len(cs))
	if len(cs) > 0 {
		var err error
		ctx, err = s.withVirtualGuide(ctx, cs)
		if err != nil {
			return nil, err
		}
	}
	type pair struct{ source, channel string }
	want := map[pair][]int{}
	var order []pair
	for i, c := range cs {
		if c.Kind == "virtual" {
			if c.Personal != nil && c.personalViewer == nil {
				if v, ok := ctx.Value(loadedPolicyKey{}).(loadedPolicy); ok {
					c = s.personalChannel(ctx, c, v.user)
				}
			}
			if ps := s.viewerSchedule(ctx, c, at, at.Add(time.Second)); len(ps) > 0 {
				out[i] = ps[0]
			}
			continue
		}
		k := pair{guideSource(c), c.GuideID}
		if _, ok := want[k]; !ok {
			order = append(order, k)
		}
		want[k] = append(want[k], i)
	}
	ms := at.UnixMilli()
	for start := 0; start < len(order); start += programmesAtBatch {
		batch := order[start:min(start+programmesAtBatch, len(order))]
		values := make([]string, len(batch))
		args := make([]any, 0, 2*len(batch)+1)
		for i, k := range batch {
			values[i] = "(?,?)"
			args = append(args, k.source, k.channel)
		}
		args = append(args, ms)
		rows, err := s.o.DB.QueryContext(ctx, `WITH ch(source_id, channel_id) AS (VALUES `+strings.Join(values, ",")+`)
SELECT ch.source_id, ch.channel_id, (SELECT p.data FROM livetv_programmes p WHERE p.source_id=ch.source_id AND p.channel_id=ch.channel_id AND p.start_ms<=? ORDER BY p.start_ms DESC LIMIT 1) FROM ch`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k pair
			var b []byte
			if err = rows.Scan(&k.source, &k.channel, &b); err != nil {
				rows.Close()
				return nil, err
			}
			if b == nil {
				continue
			}
			var p Programme
			if err = json.Unmarshal(b, &p); err != nil {
				rows.Close()
				return nil, err
			}
			if !p.End.After(at) {
				continue
			}
			for _, i := range want[k] {
				q := p
				q.ChannelID = cs[i].ID
				q.ID = fmt.Sprintf("%s-%d", cs[i].ID, q.Start.UnixMilli())
				out[i] = q
			}
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// guideChannel is the one channel both the guide and the playout schedule:
// the saved programming with its On Air graphics. Availability never reshapes
// it; a title that cannot play is replaced on air for its own slot only, so
// every other programme keeps the time the guide gave it.
func (s *Service) guideChannel(_ context.Context, c Channel) Channel {
	return s.graphicsChannel(c)
}

func (s *Service) programmes(ctx context.Context, c Channel, from, until time.Time) ([]Programme, error) {
	if c.Personal != nil {
		return s.personalProgrammes(ctx, c, c.personalViewer, from, until)
	}
	if c.Kind == "virtual" {
		s.virtualGuide.mu.RLock()
		materialized := s.virtualGuide.entries[c.ID] != nil
		s.virtualGuide.mu.RUnlock()
		if !materialized {
			var saved bool
			if err := s.o.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM livetv_channels WHERE id=?)`, c.ID).Scan(&saved); err != nil {
				return nil, err
			}
			if !saved {
				// Trusted callers may preview a recipe before saving it. Public
				// viewer paths resolve a persisted channel first and can never
				// reach this owner/draft scheduling path.
				return Schedule(s.guideChannel(ctx, c), from, until), nil
			}
		}
		if _, err := s.withVirtualGuide(ctx, []Channel{c}); err != nil {
			return nil, err
		}
		if err := s.checkVirtualWindow(c, from, until); err != nil {
			return nil, err
		}
		return s.virtualProgrammes(c, from, until, "playback"), nil
	}
	if until.Sub(from) <= time.Second {
		p, ok, err := s.programmeAt(ctx, c, from)
		if err != nil || !ok {
			return nil, err
		}
		return []Programme{p}, nil
	}
	// end_ms leads so the guide index can skip programmes that have already ended.
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND end_ms>? AND start_ms<? ORDER BY start_ms`, guideSource(c), c.GuideID, from.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Programme{}
	for rows.Next() {
		var b []byte
		var p Programme
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		p.ChannelID = c.ID
		p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Service) Guide(ctx context.Context, u *core.User, from, until time.Time) ([]Programme, error) {
	return s.guide(ctx, u, from, until, nil)
}

// GuideForChannels builds the guide for the requested channels only. The
// console sends the rows it can see; nil means the whole lineup (XMLTV).
func (s *Service) GuideForChannels(ctx context.Context, u *core.User, from, until time.Time, ids []string) ([]Programme, error) {
	return s.guide(ctx, u, from, until, ids)
}

func channelFromSummary(c ChannelSummary) Channel {
	hidden := c.rawHiddenApps
	if hidden == nil && c.ShownApps == nil {
		hidden = c.HiddenApps
	}
	return Channel{
		ID: c.ID, Number: c.Number, Name: c.Name, Kind: c.Kind, sweepKey: c.sweepKey,
		Private: c.Private, Enabled: c.Enabled, Favorite: c.Favorite, HiddenApps: hidden, ShownApps: c.ShownApps,
		Rating: c.Rating, Logo: c.Logo, SourceID: c.SourceID,
		GuideID: c.GuideID, GuideSourceID: c.GuideSourceID, Group: c.Group,
	}
}

func (s *Service) guide(ctx context.Context, u *core.User, from, until time.Time, ids []string) ([]Programme, error) {
	if !s.Enabled() {
		return nil, nil
	}
	if !until.After(from) || until.Sub(from) > 7*24*time.Hour {
		return nil, fmt.Errorf("guide window must be positive and at most 7 days")
	}
	var err error
	ctx, _, err = s.withPolicy(ctx, u)
	if err != nil {
		return nil, err
	}
	var cs []Channel
	if ids != nil {
		full, err := s.channelsByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		cs = make([]Channel, 0, len(ids))
		for _, id := range ids {
			if c, ok := full[id]; ok {
				cs = append(cs, c)
			}
		}
	} else {
		// Identity comes from the cached lineup index; only virtual channels
		// need their programming snapshot.
		all, err := s.channelSummaries(ctx)
		if err != nil {
			return nil, err
		}
		if _, idx := s.getOwnerGroupCacheState(); idx != nil {
			ctx = contextWithOwnerGroupIndex(ctx, idx)
		}
		virtualIDs := make([]string, 0)
		for _, c := range all {
			if c.Kind == "virtual" {
				virtualIDs = append(virtualIDs, c.ID)
			}
		}
		full, err := s.channelsByIDs(ctx, virtualIDs)
		if err != nil {
			return nil, err
		}
		cs = make([]Channel, 0, len(all))
		for _, c := range all {
			if c.Kind == "virtual" {
				if v, ok := full[c.ID]; ok {
					cs = append(cs, v)
				}
				continue
			}
			cs = append(cs, channelFromSummary(c))
		}
	}

	ctx, err = s.withVirtualGuide(ctx, cs)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if err := s.checkVirtualWindow(c, from, until); err != nil {
			return nil, err
		}
	}
	orderIndex := make(map[string]int, len(cs))
	byKey := make(map[guideKey][]Channel)
	out := []Programme{}
	if until.Sub(from) <= time.Second {
		// What is airing now (Home's On Now row): one batched read.
		guided := make([]Channel, 0, len(cs))
		at := make([]int, len(cs))
		for i, c := range cs {
			at[i] = -1
			if c.Enabled && c.Kind != "virtual" {
				at[i] = len(guided)
				guided = append(guided, c)
			}
		}
		airing, err := s.programmesAt(ctx, guided, from)
		if err != nil {
			return nil, err
		}
		for i, c := range cs {
			if !s.audienceAllowed(ctx, u, c) {
				continue
			}
			if at[i] >= 0 {
				if p, ok := airing[at[i]]; ok && s.allowedProgramme(ctx, u, c, &p) {
					out = append(out, p)
				}
				continue
			}
			var ps []Programme
			var err error
			if c.Kind == "virtual" {
				ps = s.viewerGuide(ctx, s.personalChannel(ctx, c, u), from, until)
			} else {
				ps, err = s.programmes(ctx, c, from, until)
			}
			if err != nil {
				return nil, err
			}
			for _, p := range ps {
				if s.allowedProgramme(ctx, u, c, &p) {
					out = append(out, p)
				}
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
		return out, nil
	}
	for _, c := range cs {
		if !s.audienceAllowed(ctx, u, c) {
			continue
		}
		orderIndex[c.ID] = len(orderIndex)
		if c.Kind == "virtual" {
			for _, p := range s.viewerGuide(ctx, s.personalChannel(ctx, c, u), from, until) {
				if s.allowedProgramme(ctx, u, c, &p) {
					out = append(out, p)
				}
			}
			continue
		}
		if c.GuideID == "" {
			continue
		}
		k := guideKey{guideSource(c), c.GuideID}
		byKey[k] = append(byKey[k], c)
	}
	perKey, err := s.programmesForKeys(ctx, byKey, from, until)
	if err != nil {
		return nil, err
	}
	for k, ps := range perKey {
		for _, c := range byKey[k] {
			for _, p := range ps {
				p.ChannelID = c.ID
				p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
				if s.allowedProgramme(ctx, u, c, &p) {
					out = append(out, p)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		if orderIndex[out[i].ChannelID] != orderIndex[out[j].ChannelID] {
			return orderIndex[out[i].ChannelID] < orderIndex[out[j].ChannelID]
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// programmesForKeys uses bounded channel-key batches for every lineup size.
// The source/channel/end index skips expired rows and unrelated sources.
func (s *Service) programmesForKeys(ctx context.Context, wanted map[guideKey][]Channel, from, until time.Time) (map[guideKey][]Programme, error) {
	out := make(map[guideKey][]Programme, len(wanted))
	if len(wanted) == 0 {
		return out, nil
	}
	// Return one ordered schedule per guide key. The wire result carries
	// the source and channel once instead of repeating them per programme.
	aggregate := "json_agg(CAST(data AS json) ORDER BY start_ms)"
	if store.DialectOf(s.o.DB) == store.SQLite {
		aggregate = "json_group_array(json(data) ORDER BY start_ms)"
	}
	scan := func(args []any, marks []string) error {
		rows, err := s.o.DB.QueryContext(ctx, `SELECT source_id, channel_id, `+aggregate+`
			FROM livetv_programmes WHERE (source_id,channel_id) IN (`+strings.Join(marks, ",")+`)
			AND end_ms>? AND start_ms<? GROUP BY source_id, channel_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k guideKey
			var b []byte
			if err = rows.Scan(&k.source, &k.guide, &b); err != nil {
				return err
			}
			var schedule []Programme
			if err = json.Unmarshal(b, &schedule); err != nil {
				return err
			}
			out[k] = schedule
		}
		return rows.Err()
	}
	keys := make([]guideKey, 0, len(wanted))
	for k := range wanted {
		keys = append(keys, k)
	}
	for start := 0; start < len(keys); start += guideBatch {
		end := min(start+guideBatch, len(keys))
		marks := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*2+2)
		for _, k := range keys[start:end] {
			marks = append(marks, "(?,?)")
			args = append(args, k.source, k.guide)
		}
		args = append(args, from.UnixMilli(), until.UnixMilli())
		if err := scan(args, marks); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// OnNow is the bounded guide used to build a live row: one policy read, then
// at most the current and next programme for each enabled channel.
func (s *Service) OnNow(ctx context.Context, u *core.User, now, until time.Time) ([]Channel, []Programme, error) {
	return s.onNow(ctx, u, now, until, 0, nil)
}

// OnNowN stops after limit allowed channels. Home uses it so a large guide
// does not query a programme window for every channel on the request.
func (s *Service) OnNowN(ctx context.Context, u *core.User, now, until time.Time, limit int) ([]Channel, []Programme, error) {
	return s.onNow(ctx, u, now, until, limit, nil)
}

// OnNowForIDs rehydrates only the channels in a persisted live shelf.
func (s *Service) OnNowForIDs(ctx context.Context, u *core.User, now, until time.Time, ids []string) ([]Channel, []Programme, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	return s.onNow(ctx, u, now, until, 0, ids)
}

func (s *Service) onNow(ctx context.Context, u *core.User, now, until time.Time, limit int, ids []string) ([]Channel, []Programme, error) {
	if u == nil || !until.After(now) {
		return nil, nil, fmt.Errorf("invalid on-now window")
	}
	ctx, _, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	var cs []Channel
	if ids == nil {
		cs, err = s.Channels(ctx)
	} else {
		var selected map[string]Channel
		selected, err = s.readChannelsByIDs(ctx, ids, make(map[string]Channel, len(ids)))
		for _, id := range ids {
			if c, ok := selected[id]; ok {
				cs = append(cs, c)
			}
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if _, idx := s.getOwnerGroupCacheState(); idx != nil {
		ctx = contextWithOwnerGroupIndex(ctx, idx)
	}
	channels := make([]Channel, 0, min(len(cs), max(limit, 1)))
	programmes := make([]Programme, 0, 2*min(len(cs), max(limit, 1)))
	// Channels are read a batch at a time (one guide query per batch, never
	// one per channel) and the walk stops once the row is full.
	const guideChunk = 128
	size := guideChunk
	if limit > 0 {
		size = min(guideChunk, max(limit, 16))
	}
	for start := 0; start < len(cs); {
		end := min(start+size, len(cs))
		// A sparse lineup can widen the next batch without changing the order.
		size = min(guideChunk, 2*size)
		chunk := make([]Channel, 0, end-start)
		for _, c := range cs[start:end] {
			if c.Enabled {
				chunk = append(chunk, c)
			}
		}
		start = end
		var readyVirtual map[string]bool
		ctx, readyVirtual, err = s.withOnNowVirtual(ctx, chunk)
		if err != nil {
			return nil, nil, err
		}
		chunk = slices.DeleteFunc(chunk, func(c Channel) bool {
			return c.Kind == "virtual" && c.Personal == nil && !readyVirtual[c.ID]
		})
		guide, err := s.programmesLimitBatch(ctx, chunk, now, until, 2)
		if err != nil {
			return nil, nil, err
		}
		for i, c := range chunk {
			ps, ok := guide[i]
			if !ok {
				ps, err = s.programmesLimit(ctx, c, now, until, 2)
				if err != nil {
					if errors.Is(err, core.ErrPreparing) {
						continue
					}
					return nil, nil, err
				}
			}
			var current *Programme
			if len(ps) > 0 {
				current = &ps[0]
			}
			if !s.allowedProgramme(ctx, u, c, current) {
				continue
			}
			c.StreamURL = ""
			c.redactArchiveSource()
			c.StreamSources = nil
			channels = append(channels, s.personalChannel(ctx, c, u))
			programmes = append(programmes, ps...)
			if limit > 0 && len(channels) >= limit {
				return channels, programmes, nil
			}
		}
	}
	return channels, programmes, nil
}

// programmesLimitBatch reads up to limit programmes in [from, until) for
// every guide-backed channel of cs in one query. Personal and virtual
// channels are absent from the result (their guide is built elsewhere);
// a guide channel with nothing on maps to an empty list.
func (s *Service) programmesLimitBatch(ctx context.Context, cs []Channel, from, until time.Time, limit int) (map[int][]Programme, error) {
	out := make(map[int][]Programme, len(cs))
	type pair struct{ source, channel string }
	want := map[pair][]int{}
	var order []pair
	for i, c := range cs {
		if c.Personal != nil || c.Kind == "virtual" {
			continue
		}
		if c.GuideID == "" || guideSource(c) == "" {
			out[i] = nil
			continue
		}
		k := pair{guideSource(c), c.GuideID}
		if _, ok := want[k]; !ok {
			order = append(order, k)
		}
		want[k] = append(want[k], i)
		out[i] = nil
	}
	if len(order) == 0 || limit <= 0 {
		return out, nil
	}
	// Each branch seeks one guide key and stops at its own limit. Applying
	// the cap after Scan overreads the whole horizon on every Home proof.
	branches := make([]string, len(order))
	args := make([]any, 0, 5*len(order))
	for i, k := range order {
		branches[i] = `SELECT * FROM (SELECT source_id,channel_id,data,start_ms FROM livetv_programmes
 WHERE source_id=? AND channel_id=? AND end_ms>? AND start_ms<? ORDER BY start_ms LIMIT ?) AS p`
		args = append(args, k.source, k.channel, from.UnixMilli(), until.UnixMilli(), limit)
	}
	rows, err := s.o.DB.QueryContext(ctx, strings.Join(branches, " UNION ALL ")+" ORDER BY source_id,channel_id,start_ms", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k pair
		var b []byte
		var start int64
		if err := rows.Scan(&k.source, &k.channel, &b, &start); err != nil {
			return nil, err
		}
		var p Programme
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		for _, i := range want[k] {
			q := p
			q.ChannelID = cs[i].ID
			q.ID = fmt.Sprintf("%s-%d", cs[i].ID, q.Start.UnixMilli())
			out[i] = append(out[i], q)
		}
	}
	return out, rows.Err()
}

func (s *Service) programmesLimit(ctx context.Context, c Channel, from, until time.Time, limit int) ([]Programme, error) {
	if c.Personal != nil {
		if v, ok := ctx.Value(loadedPolicyKey{}).(loadedPolicy); ok {
			c = s.personalChannel(ctx, c, v.user)
		}
		s.demandPersonalReadiness(c, c.personalViewer)
		ps, err := s.personalProgrammesLimit(ctx, c, c.personalViewer, from, until, limit)
		if limit > 0 && len(ps) > limit {
			ps = ps[:limit]
		}
		return ps, err
	}
	if limit <= 0 {
		return nil, nil
	}
	if c.Kind == "virtual" {
		if _, err := s.withVirtualGuide(ctx, []Channel{c}); err != nil {
			return nil, err
		}
		if err := s.checkVirtualWindow(c, from, until); err != nil {
			return nil, err
		}
		ps := s.virtualProgrammes(c, from, until, "playback")
		if len(ps) > limit {
			ps = ps[:limit]
		}
		return ps, nil
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND end_ms>? AND start_ms<? ORDER BY start_ms LIMIT ?`, guideSource(c), c.GuideID, from.UnixMilli(), until.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Programme, 0, limit)
	for rows.Next() {
		var b []byte
		var p Programme
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		p.ChannelID = c.ID
		p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		v, _, err := s.decodeSource(b)
		if err != nil {
			return nil, err
		}
		v.HasPlaylist = v.Playlist != "" || v.PlaylistURL != ""
		v.HasGuide = s.sourceGuideFeatures(v).Remote || v.Guide != "" || v.GuideURL != "" || v.PlaylistGuide
		v.Playlist = ""
		v.Guide = ""
		redactSourceAuth(&v)
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	sortSourcesByPriority(out)
	for i := range out {
		s.fillVODStatus(ctx, &out[i])
	}
	return out, s.fillSourceStats(ctx, out)
}
func (s *Service) readURL(ctx context.Context, raw string, max int64) (string, error) {
	return s.readURLWith(ctx, raw, max, nil, "source")
}

// ImportSource atomically replaces source programming and logs the outcome.
// Owner changes to mapping, enabled state, channel numbering, favourites and
// restrictions survive refresh.
func (s *Service) ImportSource(ctx context.Context, src Source) (Source, error) {
	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	if src.ID == "" {
		src.ID = randomID()
	}
	release, err := s.acquireRefreshWait(ctx, src.ID, ownerEditLeaseWait)
	if err != nil {
		s.o.Logger.Warn("Live TV source import failed", "sourceId", src.ID, "source", src.Name, "error", err)
		return src, err
	}
	defer release()
	return s.importSourceLogged(ctx, src)
}

// importSourceLogged is called only while the source refresh lease is held.
func (s *Service) importSourceLogged(ctx context.Context, src Source) (Source, error) {
	started := s.o.Now()
	v, err := s.importSource(ctx, src)
	if err != nil {
		s.o.Logger.Warn("Live TV source import failed",
			"sourceId", v.ID, "source", v.Name, "url", sourceOrigin(v), "error", err)
		eventSrc := v
		if eventSrc.ID == "" {
			eventSrc.ID = src.ID
		}
		if eventSrc.Name == "" {
			eventSrc.Name = src.Name
		}
		if eventSrc.UpdatedAt.IsZero() {
			eventSrc.UpdatedAt = src.UpdatedAt
		}
		var authErr *providerAuthError
		if errors.As(err, &authErr) {
			s.emitProviderAuthFailed(eventSrc.ID, eventSrc.Name, "", authErr.StatusCode, err)
		}
		s.emitGuideFailed(eventSrc, err)
		return v, err
	}
	s.o.Logger.Info("Live TV source imported",
		"sourceId", v.ID, "source", v.Name, "url", sourceOrigin(v),
		"channels", v.ChannelCount, "guideChannels", v.GuideChannelCount)
	s.emitGuideRefreshed(v, s.o.Now().Sub(started))
	return v, nil
}

// importSource is ImportSource without the outcome log.
func (s *Service) importSource(ctx context.Context, src Source) (Source, error) {
	if old, err := s.storedSourceByID(ctx, src.ID); err == nil && (src.Disabled || old.Disabled != src.Disabled) {
		old.Disabled = src.Disabled
		b, err := s.encodeSource(old)
		if err != nil {
			return old, err
		}
		tx, err := s.o.DB.BeginTx(ctx, nil)
		if err != nil {
			return old, err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `UPDATE livetv_sources SET data=? WHERE id=?`, b, old.ID); err != nil {
			return old, err
		}
		autoChanged, autoInv, err := s.reconcileAutoBackupsTx(ctx, tx, 0, nil)
		if err != nil {
			return old, err
		}
		if autoChanged {
			if _, err = tx.ExecContext(ctx, `UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1`); err != nil {
				return old, err
			}
		}
		err = tx.Commit()
		if err == nil {
			for _, cid := range autoInv {
				s.invalidate(cid, "")
			}
		}
		s.forgetSourceAuth(old.ID)
		s.wakeSourceRefresh()
		s.notifySourcesChanged()
		// The source's channels leave or rejoin every lineup and guide.
		s.bumpLineup()
		old.Playlist, old.Guide = "", ""
		redactSourceAuth(&old)
		return old, err
	}
	if src.Disabled {
		return src, core.ErrNotAvailable
	}

	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	if src.ID == "" {
		src.ID = randomID()
	}
	if strings.TrimSpace(src.Name) == "" {
		return src, fmt.Errorf("source needs a name")
	}
	if src.RefreshHours < -1 || src.RefreshHours > 24*365 || src.ActivityRefreshMinutes < 0 || src.ActivityRefreshMinutes > 24*60 || (src.ActivityRefreshMinutes > 0 && src.ActivityRefreshMinutes < 30) {
		return src, fmt.Errorf("invalid source refresh interval")
	}
	if src.Kind == legacyStremioKind {
		return src, errLegacyStremio
	}
	if src.MaxConnections < 0 || src.MaxConnections > 100 || src.TuneGapSeconds > 60 || src.SlotWaitSeconds < 0 || src.SlotWaitSeconds > 10 || src.TunesPerHour < 0 || src.TunesPerHour > 10000 {
		return src, fmt.Errorf("connection limits must be between 0 and 100, the gap between streams at most 60 seconds, and streams per hour at most 10,000")
	}
	if err := s.keepStoredUploads(ctx, &src); err != nil {
		return src, err
	}
	if err := s.prepareSourceAuth(ctx, &src); err != nil {
		return src, err
	}
	kind, ok := s.kindRegistry().Classify(src)
	if !ok && strings.HasPrefix(src.Kind, "plugin:") {
		return src, fmt.Errorf("the plugin behind this source is off or removed; switch it on in Plugins")
	}
	if !ok {
		return src, fmt.Errorf("source needs a playlist or an XMLTV guide")
	}
	progress := importProgress(ctx)
	src.SkippedVOD = 0
	src.PlaylistGuide = false
	// Account and groups are what this fetch reports; a source body from the
	// owner API carries stale read-only copies.
	src.Account, src.Groups, src.ArchiveTimezone = nil, nil, ""
	fetched, err := kind.Fetch(ctx, s, &src)
	if err != nil {
		return src, err
	}
	cs, g := fetched.Channels, fetched.Guide
	if err := s.refreshLogins(ctx, &src, cs); err != nil {
		return src, err
	}
	s.matchLogos(ctx, cs)
	g.Programmes = dedupeProgrammes(g.Programmes)
	s.enrichImportedArtwork(ctx, g.Programmes)
	if progress != nil {
		progress.setChannels(len(cs))
		progress.setProgrammes(len(g.Programmes))
		progress.setPhase("saving")
	}
	// Everything the import compares against is read, decoded and encoded
	// before it takes the writer; the transaction then checks one epoch
	// row and writes what changed. Stored channels include virtual ones,
	// whose programming is megabytes of JSON, so decoding them inside the
	// transaction held every other writer for seconds.
	pre, err := readImportSnapshot(ctx, s.o.DB, src.ID)
	if err != nil {
		return src, err
	}
	progPlan, err := planProgrammes(g.Programmes)
	if err != nil {
		return src, err
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return src, err
	}
	defer tx.Rollback()
	// Bulk edits share this source lock, so refresh cannot restore a stale
	// enabled/favourite/group snapshot over an edit that already succeeded.
	if err := s.lockSourceEdits(ctx, tx, []string{src.ID}); err != nil {
		return src, err
	}
	if err := s.writeLoginStreams(ctx, tx, src); err != nil {
		return src, err
	}
	if err := s.invalidateVODConnection(ctx, tx, src); err != nil {
		return src, err
	}
	// lockSourceEdits above makes a concurrent save of the same source on
	// another node wait, so the owner's channel edits read inside this
	// transaction cannot be lost. The source row itself is written only when
	// it changed.
	src.UpdatedAt = s.o.Now()
	sourceWritten, err := s.writeImportedSource(ctx, tx, &src)
	if err != nil {
		return src, err
	}
	old, fresh, err := pre.current(ctx, tx)
	if err != nil {
		return src, err
	}
	oldByID := map[string]Channel{}
	for _, c := range old {
		if c.SourceID == src.ID {
			oldByID[c.ID] = c
		}
	}
	keepNumber := map[string]Channel{}
	for _, c := range cs {
		if previous, ok := oldByID[c.ID]; ok && (c.numberMissing || c.ProviderNumber == "" || numberPinned(previous, fetched.ProviderNumbers)) {
			keepNumber[c.ID] = previous
		}
	}
	if err := assignImportedNumbers(ctx, tx, src.ID, cs, keepNumber, old, s.o.Now(), strings.HasPrefix(src.Kind, "plugin:")); err != nil {
		return src, err
	}
	// Write in batches: one statement per row also fires the database's
	// per-statement viewer-policy trigger once per row, which made a large
	// import take many minutes. SQLite's default variable limit is 999.
	dialect := store.DialectOf(s.o.DB)
	batch := 1000
	if dialect == store.SQLite {
		batch = 150
	}
	// Only rows that differ from what is stored are written (guide_import.go):
	// an unchanged refresh writes nothing and leaves the viewer-policy epoch
	// alone.
	stored := make([]Channel, 0, len(cs))
	ids := make([]string, 0, len(cs))
	encoded := make([][]byte, 0, len(cs))
	{
		for _, c := range cs {
			if !fetched.cachedLineup {
				c.ProviderGuideID = c.GuideID
				if err := s.sealArchive(&c, oldByID[c.ID]); err != nil {
					return src, err
				}
			}
			if previous, ok := oldByID[c.ID]; ok {
				if _, keep := keepNumber[c.ID]; keep {
					c.Number = previous.Number
				}
				c.Enabled = previous.Enabled
				c.HiddenApps = previous.HiddenApps
				c.ShownApps = previous.ShownApps
				c.Favorite = previous.Favorite
				c.Private = previous.Private
				c.Rating = previous.Rating
				if guidePinned(previous) {
					c.GuideID = previous.GuideID
					c.GuideSourceID = previous.GuideSourceID
				}
				c.Transcode = previous.Transcode
				c.Stability = previous.Stability
				c.Screens = previous.Screens
				c.Look = previous.Look
				c.DVRMinutes = previous.DVRMinutes
				c.InputMerge = previous.InputMerge
				c.SuppressedAutoBackups = slices.Clone(previous.SuppressedAutoBackups)
				c.OwnerStreamSources = previous.OwnerStreamSources
				if previous.CustomGroup {
					c.Group, c.CustomGroup = previous.Group, true
				}
				if fetched.AddonInputs {
					c.StreamSources = mergeAddonStreams(c.StreamSources, previous.StreamSources)
				} else {
					c.StreamSources = mergeImportedStreams(c, previous)
				}
				if len(c.StreamSources) > 0 {
					if err := normalizeStreamSources(&c); err != nil {
						return src, err
					}
				}
			}
			if guide, ok := g.Channels[c.GuideID]; ok && c.Logo == "" {
				c.Logo = guide.Logo
			}
			stored = append(stored, c)
		}
	}
	crossChanged, crossInvalidated, crossWritten, err := s.reconcileImportedMerges(ctx, tx, src, stored, old)
	if err != nil {
		return src, err
	}
	for _, c := range stored {
		b, e := json.Marshal(c)
		if e != nil {
			return src, e
		}
		ids, encoded = append(ids, c.ID), append(encoded, b)
	}
	var savedChannels, savedProgrammes func(int)
	if progress != nil {
		savedChannels, savedProgrammes = progress.setSavedChannels, progress.setSavedProgrammes
	}
	var storedRows map[string][]byte
	if fresh {
		storedRows = pre.sourceRows(src.ID)
	}
	writes, err := writeSourceChannels(ctx, tx, dialect, batch, src.ID, ids, encoded, storedRows, savedChannels)
	if err != nil {
		return src, err
	}
	// Guide rows are shared by every channel mapped to this source and guide
	// ID. Keep the longest window among those channels, using the new lineup
	// for the source being refreshed.
	archiveSince := make(map[string]int64)
	noteArchive := func(c Channel) {
		if days := min(c.ArchiveDays(), 30); days > 0 && c.GuideID != "" && guideSource(c) == src.ID {
			since := src.UpdatedAt.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
			if previous, ok := archiveSince[c.GuideID]; !ok || since < previous {
				archiveSince[c.GuideID] = since
			}
		}
	}
	for _, c := range old {
		if c.SourceID != src.ID {
			noteArchive(c)
		}
	}
	for _, c := range stored {
		noteArchive(c)
	}
	pw, err := writeProgrammes(ctx, tx, batch, src.ID, progPlan, src.UpdatedAt, archiveSince, savedProgrammes)
	if err != nil {
		return src, err
	}
	writes.ProgrammesWritten, writes.ProgrammesDeleted, writes.ProgrammesKept = pw.ProgrammesWritten, pw.ProgrammesDeleted, pw.ProgrammesKept
	if _, err := pruneCatchUpWatch(ctx, tx, src.ID, src.UpdatedAt); err != nil {
		return src, err
	}
	gw, err := writeGuideChannels(ctx, tx, src.ID, g)
	if err != nil {
		return src, err
	}
	writes.GuideChannelsWritten, writes.GuideChannelsDeleted = gw.GuideChannelsWritten, gw.GuideChannelsDeleted
	// Duplicate backups match against the IPTV channels as just written:
	// on SQLite, when the snapshot held, that is the snapshot with this
	// import's own writes applied, decoded afresh; otherwise read here.
	var iptv []Channel
	if fresh && dialect == store.SQLite {
		written := make(map[string][]byte, len(ids)+len(crossWritten))
		for i, id := range ids {
			written[id] = encoded[i]
		}
		for id, b := range crossWritten {
			written[id] = b
		}
		iptv, err = pre.iptvAfter(src.ID, written)
	} else {
		var all []Channel
		all, err = channels(ctx, tx)
		iptv = iptvOnly(all)
	}
	if err != nil {
		return src, err
	}
	autoChanged, autoInvalidated, err := s.reconcileAutoBackupsIn(ctx, tx, batch, stored, iptv)
	if err != nil {
		return src, err
	}
	crossInvalidated = append(crossInvalidated, autoInvalidated...)
	if progress != nil {
		progress.setSavedChannels(len(stored))
		progress.setSavedProgrammes(len(g.Programmes))
	}
	changed := sourceWritten || writes.changed() || crossChanged || autoChanged
	if changed {
		// Guide-only sources change coverage without touching any channel
		// rows. Publish that change transactionally to every cluster node's
		// caches.
		if _, err = tx.ExecContext(ctx, `UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1`); err != nil {
			return src, err
		}
	}
	s.logger().Info("live TV source imported", "source", src.ID, "programmes_written", writes.ProgrammesWritten, "programmes_deleted", writes.ProgrammesDeleted,
		"programmes_unchanged", writes.ProgrammesKept, "channels_written", writes.ChannelsWritten, "channels_deleted", writes.ChannelsDeleted,
		"guide_channels_written", writes.GuideChannelsWritten, "guide_channels_deleted", writes.GuideChannelsDeleted, "source_written", sourceWritten)
	err = tx.Commit()
	s.forgetSourceAuth(src.ID)
	if err == nil {
		s.wakeSourceRefresh()
	}
	if err == nil && changed {
		if err = s.refreshVirtualGuideIfWarm(ctx); err != nil {
			return src, err
		}
		s.wakeHealthSweep()
		s.bumpLineup()
		if fetched.AddonInputs {
			var newChannels []string
			for _, c := range stored {
				if _, existed := oldByID[c.ID]; !existed {
					newChannels = append(newChannels, c.ID)
				}
			}
			if len(newChannels) > 0 {
				_, _ = s.MatchGuides(ctx, GuideMatchRequest{ChannelIDs: newChannels})
			}
		}
		// Only tunes whose stream changed or disappeared are interrupted, so
		// a scheduled refresh does not cut off viewers.
		for _, id := range changedStreams(oldByID, stored) {
			s.invalidate(id, "")
		}
		for _, id := range crossInvalidated {
			s.invalidate(id, "")
		}
	}
	if err == nil && src.Auth != nil && src.Auth.Type == "xtream" {
		// The VOD loop refreshes this provider's catalogue now and announces
		// VODChanged itself, only when the catalogue really changed.
		_, _ = s.o.DB.ExecContext(ctx, `UPDATE xtream_vod_refresh SET next=0 WHERE provider=?`, src.ID)
		s.wakeVODLoop()
	}
	src = storedSource(src)
	src.HasPlaylist = src.Playlist != "" || src.PlaylistURL != ""
	src.HasGuide = s.sourceGuideFeatures(src).Remote || src.Guide != "" || src.GuideURL != "" || src.PlaylistGuide
	src.Playlist = ""
	src.Guide = ""
	redactSourceAuth(&src)
	if err == nil {
		one := []Source{src}
		if e := s.fillSourceStats(ctx, one); e == nil {
			src = one[0]
		}
		s.notifySourcesChanged()
	}
	return src, err
}

// numberPinned reports whether a stored channel's number is the owner's
// rather than the provider's. Channels stored before provider numbers were
// recorded keep theirs, unless the new lineup is wholly provider-numbered
// (an Xtream panel, whose older M3U import generated numbers per group).
func numberPinned(previous Channel, providerNumbered bool) bool {
	if previous.ProviderNumber == "" {
		return !providerNumbered
	}
	return previous.Number != previous.ProviderNumber
}

// guidePinned reports whether a stored channel's guide mapping is the owner's
// rather than the provider's. A cross-source mapping or any edit (including an
// explicit clear) makes GuideID differ from ProviderGuideID and sticks across
// refreshes; channels stored before ProviderGuideID was recorded keep their
// existing mapping until a matching refresh records the provider baseline.
func guidePinned(previous Channel) bool {
	if previous.GuideSourceID != "" && previous.GuideSourceID != previous.SourceID {
		return true
	}
	return previous.GuideID != previous.ProviderGuideID
}

// sourceGuideFilter bounds XMLTV imports by guide ID and time. A guide-only
// source accepts every ID. Owner mappings contribute their archive windows.
func (s *Service) sourceGuideFilter(ctx context.Context, src Source, playlist []Channel, hasChannels bool) (guideFilter, error) {
	now := s.o.Now()
	filter := guideFilter{From: now.Add(-guideLookback), Until: now.Add(guideLookahead), ArchiveFrom: map[string]time.Time{}}
	if hasChannels {
		filter.IDs = map[string]struct{}{}
	}
	add := func(c Channel, id string) {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			return
		}
		if filter.IDs != nil {
			filter.IDs[id] = struct{}{}
		}
		if days := min(c.ArchiveDays(), 30); days > 1 {
			from := now.Add(-time.Duration(days) * 24 * time.Hour)
			if previous, ok := filter.ArchiveFrom[id]; !ok || from.Before(previous) {
				filter.ArchiveFrom[id] = from
			}
		}
	}
	inPlaylist := make(map[string]Channel, len(playlist))
	for _, c := range playlist {
		inPlaylist[c.ID] = c
		add(c, c.GuideID)
	}
	stored, err := s.allChannels(ctx)
	if err != nil {
		return guideFilter{}, err
	}
	for _, c := range stored {
		if guideSource(c) != src.ID {
			continue
		}
		if next, ok := inPlaylist[c.ID]; c.SourceID == src.ID && ok {
			if guidePinned(c) {
				add(next, c.GuideID)
			}
			continue
		}
		add(c, c.GuideID)
	}
	return filter, nil
}

// nameCandidates returns guide ids whose normalized name matches a playlist
// channel that has no guide id, or whose guide id the guide does not declare.
// Ambiguous matches are included: they are only stored for the owner to
// review, and the resolver never changes a channel's mapping.
func nameCandidates(cs []Channel, channels map[string]Channel, names map[string][]string) map[string]struct{} {
	if len(cs) == 0 || len(channels) == 0 {
		return nil
	}
	byName := map[string][]string{}
	for id, c := range channels {
		seen := map[string]bool{}
		for _, n := range append([]string{c.Name}, names[id]...) {
			k := normalizeGuideName(n)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			byName[k] = append(byName[k], id)
		}
	}
	channelIDs := make(map[string]bool, len(channels))
	for id := range channels {
		channelIDs[strings.ToLower(strings.TrimSpace(id))] = true
	}
	out := map[string]struct{}{}
	for _, c := range cs {
		if channelIDs[strings.ToLower(strings.TrimSpace(c.GuideID))] {
			continue
		}
		seen := map[string]bool{}
		for _, n := range []string{c.TvgName, c.Name} {
			k := normalizeGuideName(n)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			for _, id := range byName[k] {
				out[id] = struct{}{}
			}
		}
	}
	return out
}

// SavePendingSource stores a named source that will receive a file upload
// next. A body that already has a playlist or guide is imported instead.
func (s *Service) SavePendingSource(ctx context.Context, src Source) (Source, error) {
	if src.ID == "" {
		src.ID = randomID()
	}
	if strings.TrimSpace(src.Name) == "" {
		return src, fmt.Errorf("source needs a name")
	}
	if err := s.prepareSourceAuth(ctx, &src); err != nil {
		return src, err
	}
	if src.Playlist != "" || src.PlaylistURL != "" || src.Guide != "" || src.GuideURL != "" {
		return s.ImportSource(ctx, src)
	}
	old, err := s.storedSourceByID(ctx, src.ID)
	renamed := err == nil && old.Name != src.Name
	if err == nil {
		src.Playlist, src.PlaylistURL = old.Playlist, old.PlaylistURL
		src.Guide, src.GuideURL = old.Guide, old.GuideURL
		if src.Priority == 0 {
			src.Priority = old.Priority
		}
		if src.Auth == nil {
			src.Auth = old.Auth
		}
		if src.Playlist != "" || src.PlaylistURL != "" || src.Guide != "" || src.GuideURL != "" {
			return s.ImportSource(ctx, src)
		}
	} else if !errors.Is(err, core.ErrNotFound) {
		return src, err
	}
	src.UpdatedAt = s.o.Now()
	b, err := s.encodeSource(src)
	if err != nil {
		return src, err
	}
	if _, err = s.o.DB.ExecContext(ctx, `INSERT INTO livetv_sources(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, src.ID, b); err != nil {
		return src, err
	}
	if renamed {
		_, _ = s.o.DB.ExecContext(ctx, `UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1`)
		s.bumpLineup()
	}
	s.forgetSourceAuth(src.ID)
	s.wakeSourceRefresh()
	src.HasPlaylist = false
	src.HasGuide = false
	src.Playlist, src.Guide = "", ""
	redactSourceAuth(&src)
	one := []Source{src}
	if e := s.fillSourceStats(ctx, one); e == nil {
		src = one[0]
	}
	s.notifySourcesChanged()
	return src, nil
}

// ImportUpload streams a raw playlist or guide body (gzip is decompressed
// while reading) into an existing source.
func (s *Service) ImportUpload(ctx context.Context, id, part string, body io.Reader) (Source, error) {
	src, err := s.storedSourceByID(ctx, id)
	if err != nil {
		return src, err
	}
	var max int64 = maxPlaylistBytes
	kind := "playlist"
	if part == "guide" {
		max = maxGuideBytes
		kind = "guide"
	}
	rd, err := maybeGunzip(body, max, kind)
	if err != nil {
		return src, err
	}
	defer closeReader(rd)
	if part == "playlist" {
		b, err := io.ReadAll(&hardLimit{r: rd, max: max, kind: kind})
		if err != nil {
			if lim := limitCause(err); lim != nil {
				return src, lim
			}
			return src, err
		}
		src.Playlist = string(b)
		src.PlaylistURL = ""
		return s.ImportSource(ctx, src)
	}
	var cs []Channel
	if strings.TrimSpace(src.Playlist) != "" {
		cs, err = ParseM3U(strings.NewReader(src.Playlist), src.ID)
		if err != nil {
			return src, err
		}
	} else {
		// URL-backed sources do not store their playlist body. Their current
		// channels are the best available resolver input for a guide upload.
		stored, err := s.Channels(ctx)
		if err != nil {
			return src, err
		}
		for _, c := range stored {
			if c.SourceID == src.ID {
				cs = append(cs, c)
			}
		}
	}
	filter, err := s.sourceGuideFilter(ctx, src, cs, strings.TrimSpace(src.Playlist) != "" || src.PlaylistURL != "")
	if err != nil {
		return src, err
	}
	if len(cs) > 0 {
		filter.Resolve = func(channels map[string]Channel, names map[string][]string) map[string]struct{} {
			return nameCandidates(cs, channels, names)
		}
	}
	g, _, err := parseXMLTV(rd, filter)
	if err != nil {
		return src, err
	}
	trimmed, err := formatXMLTV(g)
	if err != nil {
		return src, err
	}
	src.Guide = trimmed
	src.GuideURL = ""
	return s.ImportSource(ctx, src)
}

func (s *Service) DeleteSource(ctx context.Context, id string) error {
	defer s.forgetSourceAuth(id)
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockSourceEdits(ctx, tx, []string{id}); err != nil {
		return err
	}
	d := store.DialectOf(s.o.DB)
	for _, q := range []string{`DELETE FROM livetv_channels WHERE ` + store.JSONText(d, "data", "sourceId") + `=?`, `DELETE FROM livetv_programmes WHERE source_id=?`, `DELETE FROM livetv_catchup_watch WHERE guide_source=?`, `DELETE FROM livetv_login_streams WHERE source_id=?`, `DELETE FROM livetv_sources WHERE id=?`, `DELETE FROM livetv_source_refresh WHERE id=?`, `DELETE FROM xtream_vod WHERE provider=?`, `DELETE FROM xtream_vod_refresh WHERE provider=?`, `DELETE FROM livetv_guide_channels WHERE source_id=?`,
		`UPDATE livetv_channels SET data=` + store.JSONRemove(d, "data", "guideSourceId") + ` WHERE ` + store.JSONText(d, "data", "guideSourceId") + `=?`} {
		if _, err = tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	_, delInv, err := s.reconcileInputMergesTx(ctx, tx)
	if err != nil {
		return err
	}
	_, autoInv, err := s.reconcileAutoBackupsTx(ctx, tx, 0, nil)
	if err != nil {
		return err
	}
	delInv = append(delInv, autoInv...)
	if _, err = tx.ExecContext(ctx, `UPDATE livetv_channel_epoch SET generation=generation+1 WHERE id=1`); err != nil {
		return err
	}
	err = tx.Commit()
	if err == nil {
		for _, cid := range delInv {
			s.invalidate(cid, "")
		}
	}
	s.forgetSourceAuth(id)
	if err == nil {
		s.invalidate("", id)
		s.notifySourcesChanged()
		s.wakeSourceRefresh()
		s.wakeHealthSweep()
		s.wakeVODLoop()
		if s.o.VODChanged != nil {
			s.o.VODChanged()
		}
		if err = s.refreshVirtualGuideIfWarm(ctx); err != nil {
			return err
		}
		s.bumpLineup()
	}
	return err
}

func (s *Service) newTimer(d time.Duration) *time.Timer {
	if s.o.NewTimer != nil {
		return s.o.NewTimer(d)
	}
	return time.NewTimer(d)
}
func (s *Service) newTicker(d time.Duration) *time.Ticker {
	if s.o.NewTicker != nil {
		return s.o.NewTicker(d)
	}
	return time.NewTicker(d)
}

func (s *Service) hasBroadcastOutput(channel string) bool {
	s.broadcasts.mu.Lock()
	defer s.broadcasts.mu.Unlock()
	for key := range s.broadcasts.outputs {
		if key.channelID == channel {
			return true
		}
	}
	return false
}

// serviceContext snapshots the current worker lifetime. Requests can read it
// while the lifetime controller starts or restarts workers.
func (s *Service) serviceContext() context.Context {
	s.ctxMu.RLock()
	ctx := s.ctx
	s.ctxMu.RUnlock()
	return ctx
}
