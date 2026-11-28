package liveevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plugins"
)

const (
	defaultMaxEventsPerFeed  = 512
	defaultMaxTotalEvents    = 4096
	defaultMaxCachedDetails  = 256
	defaultMaxCachedQueries  = 256
	defaultMaxCachedEntities = 128
	defaultMaxCachedStanding = 128
	defaultMaxSubscribers    = 4096
	defaultSubBuffer         = 16
	defaultAlertBuffer       = 32
	maxSubSentHashes         = 1024
	maxTargetedPollIDs       = 50

	defaultCallTimeout = 20 * time.Second
	maxPollInterval    = time.Hour
	maxErrorBackoff    = 5 * time.Minute
	maxRetryAfter      = 10 * time.Minute
	entitiesCacheTTL   = 5 * time.Minute
	standingsCacheTTL  = 2 * time.Minute
)

// Options configures a live events Hub.
type Options struct {
	Source              PluginSource
	DB                  *sql.DB
	Cluster             cluster.Cluster
	ClusterBus          clusterbus.Bus
	Clock               Clock
	Log                 *slog.Logger
	MaxEventsPerFeed    int
	MaxTotalEvents      int
	MaxCachedDetails    int
	MaxCachedQueries    int
	MaxSubscribers      int
	AlertHub            *AlertHub
	WatchEnricher       WatchEnricher
	DetailWatchEnricher DetailWatchEnricher
}

// DetailWatchEnricher attaches viewer-scoped watch targets (including plugin
// stream links from detail.Links) to a single LiveEventDetail.
type DetailWatchEnricher func(ctx context.Context, viewer *core.Viewer, detail *core.LiveEventDetail)

type cachedEvent struct {
	event     core.LiveEvent
	pluginID  string
	feedID    string
	feedKey   string
	hash      string
	fetchedAt time.Time
	touchSeq  int64
}

// eventTombstone keeps enough of an evicted event to suppress replayed updates.
type eventTombstone struct {
	feedKey, hash, updatedAt string
	touchSeq                 int64
}

type cachedQuery struct {
	resp      core.LiveEventsResponse
	expiresAt time.Time
	touchSeq  int64
}

type cachedDetail struct {
	detail    core.LiveEventDetail
	expiresAt time.Time
	touchSeq  int64
}

type cachedEntities struct {
	resp      core.LiveEventEntitiesResponse
	expiresAt time.Time
	touchSeq  int64
}

type cachedStandings struct {
	resp      core.LiveEventStandingsResponse
	expiresAt time.Time
	touchSeq  int64
}

type feedPoller struct {
	feedKey string
	cancel  context.CancelFunc
	done    chan struct{}
}

// Subscription receives live event updates and optional alerts until Close is called.
type Subscription struct {
	id             int64
	hub            *Hub
	viewer         *core.Viewer
	viewerID       int64
	filterSpoilers bool
	invalidateOnly bool
	wantsAlerts    bool
	feeds          map[string]struct{} // "pluginID:feedID"
	eventIDs       map[string]struct{} // "pluginID:feedID:eventID"
	updates        chan []core.LiveEvent
	alertSub       *AlertSubscription

	sendMu       sync.Mutex
	closed       bool
	lastSentHash map[string]string
	closeOnce    sync.Once
}

// Updates returns a channel delivering batches of changed live events.
func (s *Subscription) Updates() <-chan []core.LiveEvent {
	if s == nil {
		return nil
	}
	return s.updates
}

// Alerts returns a channel delivering viewer-scoped in-app alerts, or nil if
// Alerts was false in SubscribeRequest.
func (s *Subscription) Alerts() <-chan core.LiveEventAlert {
	if s == nil || s.alertSub == nil {
		return nil
	}
	return s.alertSub.C
}

// Close unsubscribes from the hub. When the last subscriber for a feed closes,
// the hub immediately stops polling that feed.
func (s *Subscription) Close() {
	if s == nil || s.hub == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.sendMu.Lock()
		s.closed = true
		s.sendMu.Unlock()
		s.hub.unsubscribe(s.id)
	})
}

func (s *Subscription) matches(feedKey, eventID string) bool {
	if _, ok := s.feeds[feedKey]; ok {
		return true
	}
	_, ok := s.eventIDs[eventID]
	return ok
}

func (s *Subscription) enqueueUpdates(batch []core.LiveEvent) {
	if len(batch) == 0 {
		return
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.updates <- batch:
		return
	default:
	}
	// Channel full: coalesce oldest pending batch with the newest batch so the
	// subscriber never stalls the hub and never drops the latest event state.
	var merged []core.LiveEvent
	select {
	case prev := <-s.updates:
		merged = mergeEventBatches(prev, batch)
	default:
		merged = batch
	}
	select {
	case s.updates <- merged:
	default:
	}
}

func mergeEventBatches(older, newer []core.LiveEvent) []core.LiveEvent {
	index := make(map[string]int, len(older)+len(newer))
	out := make([]core.LiveEvent, 0, len(older)+len(newer))
	for _, ev := range older {
		index[ev.ID] = len(out)
		out = append(out, ev)
	}
	for _, ev := range newer {
		if idx, ok := index[ev.ID]; ok {
			out[idx] = ev
		} else {
			index[ev.ID] = len(out)
			out = append(out, ev)
		}
	}
	return out
}

// Hub caches live events from plugins, polls active feeds only while subscribed,
// deduplicates concurrent fetches, enforces viewer spoiler preferences, and
// fans out updates and alerts across single-node and clustered deployments.
type Hub struct {
	source   PluginSource
	cluster  cluster.Cluster
	cbus     clusterbus.Bus
	origin   string
	clock    Clock
	log      *slog.Logger
	spoilers *SpoilerStore

	maxEventsPerFeed int
	maxTotalEvents   int
	maxCachedDetails int
	maxCachedQueries int
	maxSubscribers   int

	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup

	sfEvents    singleflight[core.LiveEventsResponse]
	sfDetail    singleflight[core.LiveEventDetail]
	sfEntities  singleflight[core.LiveEventEntitiesResponse]
	sfStandings singleflight[core.LiveEventStandingsResponse]

	mu                  sync.Mutex
	closed              bool
	leading             bool
	feedsLoaded         bool
	feeds               map[string]FeedInfo // "pluginID:feedID" -> info
	events              map[string]*cachedEvent
	eventTombstones     map[string]eventTombstone
	feedEventCounts     map[string]int
	feedLastPolled      map[string]time.Time
	feedHealth          map[string]*FeedHealth
	queryCache          map[string]*cachedQuery
	detailCache         map[string]*cachedDetail
	entitiesCache       map[string]*cachedEntities
	standingsCache      map[string]*cachedStandings
	cacheSeq            int64
	nextSubID           int64
	subs                map[int64]*Subscription
	alertHub            *AlertHub
	pollers             map[string]*feedPoller        // feedKey -> active poller
	remoteInterests     map[string]remotePeerInterest // origin -> peer interest
	interestCancel      context.CancelFunc
	remotePruneCancel   context.CancelFunc
	nextChangeHookID    int64
	changeHooks         map[int64]ChangeHandler
	nextPollHookID      int64
	pollHooks           map[int64]FeedPollHandler
	watchEnricher       WatchEnricher
	detailWatchEnricher DetailWatchEnricher
}

// NewHub builds a live events hub. In standalone mode (opts.Cluster == nil),
// the hub acts as leader immediately and polls feeds solely on demand while
// subscribers exist. In clustered mode, call Start(ctx) to run the leader loop
// and clusterbus listeners.
func NewHub(opts Options) *Hub {
	clk := opts.Clock
	if clk == nil {
		clk = realClock{}
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	cbus := opts.ClusterBus
	if cbus == nil && opts.Cluster != nil {
		cbus = clusterbus.FromCluster(opts.Cluster)
	}
	maxPerFeed := opts.MaxEventsPerFeed
	if maxPerFeed <= 0 {
		maxPerFeed = defaultMaxEventsPerFeed
	}
	maxTotal := opts.MaxTotalEvents
	if maxTotal <= 0 {
		maxTotal = defaultMaxTotalEvents
	}
	maxDetails := opts.MaxCachedDetails
	if maxDetails <= 0 {
		maxDetails = defaultMaxCachedDetails
	}
	maxQueries := opts.MaxCachedQueries
	if maxQueries <= 0 {
		maxQueries = defaultMaxCachedQueries
	}
	maxSubs := opts.MaxSubscribers
	if maxSubs <= 0 {
		maxSubs = defaultMaxSubscribers
	}
	alertHub := opts.AlertHub
	if alertHub == nil {
		alertHub = NewAlertHub()
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	h := &Hub{
		source:              opts.Source,
		cluster:             opts.Cluster,
		cbus:                cbus,
		origin:              clusterbus.NewOrigin(),
		clock:               clk,
		log:                 log,
		maxEventsPerFeed:    maxPerFeed,
		maxTotalEvents:      maxTotal,
		maxCachedDetails:    maxDetails,
		maxCachedQueries:    maxQueries,
		maxSubscribers:      maxSubs,
		runCtx:              runCtx,
		runCancel:           runCancel,
		leading:             opts.Cluster == nil,
		feeds:               make(map[string]FeedInfo),
		events:              make(map[string]*cachedEvent),
		eventTombstones:     make(map[string]eventTombstone),
		feedEventCounts:     make(map[string]int),
		feedLastPolled:      make(map[string]time.Time),
		feedHealth:          make(map[string]*FeedHealth),
		queryCache:          make(map[string]*cachedQuery),
		detailCache:         make(map[string]*cachedDetail),
		entitiesCache:       make(map[string]*cachedEntities),
		standingsCache:      make(map[string]*cachedStandings),
		subs:                make(map[int64]*Subscription),
		alertHub:            alertHub,
		pollers:             make(map[string]*feedPoller),
		remoteInterests:     make(map[string]remotePeerInterest),
		changeHooks:         make(map[int64]ChangeHandler),
		pollHooks:           make(map[int64]FeedPollHandler),
		watchEnricher:       opts.WatchEnricher,
		detailWatchEnricher: opts.DetailWatchEnricher,
	}
	h.spoilers = NewSpoilerStore(opts.DB, clk, func(viewerID int64) {
		h.publishClusterSpoiler(viewerID)
	})
	return h
}

// Start runs clusterbus subscriptions and (when clustered) the leader polling
// lifecycle until ctx is canceled. At idle it performs zero plugin polling.
func (h *Hub) Start(ctx context.Context) {
	if h == nil {
		return
	}
	unsubBus := h.attachClusterBus(ctx)
	defer unsubBus()

	if h.cluster == nil {
		h.mu.Lock()
		h.leading = true
		h.reconcilePollersLocked()
		h.mu.Unlock()
		<-ctx.Done()
		h.Close()
		return
	}

	h.cluster.RunAsLeader(ctx, "live events hub", func(leadCtx context.Context) {
		h.mu.Lock()
		h.leading = true
		h.reconcileInterestRefresherLocked()
		h.reconcilePollersLocked()
		h.mu.Unlock()

		<-leadCtx.Done()

		h.mu.Lock()
		h.leading = false
		h.stopAllPollersLocked()
		h.reconcileInterestRefresherLocked()
		hasLocal := h.hasLocalFeedInterestLocked()
		h.mu.Unlock()

		if hasLocal && ctx.Err() == nil {
			h.publishLocalInterest(ctx)
		}
	})
	h.Close()
}

// Close stops all active pollers and background helpers and waits for them to exit.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.stopAllPollersLocked()
	if h.interestCancel != nil {
		h.interestCancel()
		h.interestCancel = nil
	}
	if h.remotePruneCancel != nil {
		h.remotePruneCancel()
		h.remotePruneCancel = nil
	}
	h.runCancel()
	h.mu.Unlock()
	h.wg.Wait()
}

// IsLeading reports whether this node is currently the active polling leader.
func (h *Hub) IsLeading() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.leading
}

// SetWatchEnricher installs or replaces the optional viewer-scoped watch target
// enricher (A5). Safe to call concurrently.
func (h *Hub) SetWatchEnricher(fn WatchEnricher) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.watchEnricher = fn
	h.mu.Unlock()
}

// SetDetailWatchEnricher installs or replaces the optional detail-level watch
// target enricher (A5), which has access to detail.Links in addition to the
// event's broadcasts and matchHints. Safe to call concurrently.
func (h *Hub) SetDetailWatchEnricher(fn DetailWatchEnricher) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.detailWatchEnricher = fn
	h.mu.Unlock()
}

// AlertHub returns the unified in-app alert broker backing /live/stream?alerts=1.
func (h *Hub) AlertHub() *AlertHub {
	if h == nil {
		return nil
	}
	return h.alertHub
}

// AlertDeliverer returns an AlertInAppDeliverer that delivers alerts through
// this hub's unified AlertHub and clusterbus relay, returning the number of
// local in-app subscribers reached.
func (h *Hub) AlertDeliverer() AlertInAppDeliverer {
	return hubAlertDeliverer{hub: h}
}

type hubAlertDeliverer struct {
	hub *Hub
}

func (d hubAlertDeliverer) PublishAlert(viewerID int64, alert core.LiveEventAlert) int {
	if d.hub == nil {
		return 0
	}
	return d.hub.publishAlert(d.hub.runCtx, viewerID, alert)
}

// OnEventChange registers a callback invoked with raw (unmasked) event
// transitions whenever the hub ingests a new or modified event. Returns an
// unsubscribe function. Used by A7 alert detection.
func (h *Hub) OnEventChange(fn ChangeHandler) func() {
	if h == nil || fn == nil {
		return func() {}
	}
	h.mu.Lock()
	h.nextChangeHookID++
	id := h.nextChangeHookID
	h.changeHooks[id] = fn
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.changeHooks, id)
		h.mu.Unlock()
	}
}

// OnFeedPoll registers a callback invoked whenever a feed poll finishes
// (whether it succeeded or failed). Returns an unsubscribe function.
func (h *Hub) OnFeedPoll(fn FeedPollHandler) func() {
	if h == nil || fn == nil {
		return func() {}
	}
	h.mu.Lock()
	h.nextPollHookID++
	id := h.nextPollHookID
	h.pollHooks[id] = fn
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.pollHooks, id)
		h.mu.Unlock()
	}
}

// SyncFeeds refreshes the active feed registry from PluginSource and stops
// pollers for any feeds whose plugin was disabled, shadowed, revoked, or removed.
func (h *Hub) SyncFeeds(ctx context.Context) {
	if h == nil || h.source == nil {
		return
	}
	list := h.source.ActiveLiveFeeds(ctx)
	next := make(map[string]FeedInfo, len(list))
	for _, f := range list {
		if f.MinIntervalSec < plugins.MinLiveFeedIntervalSec {
			f.MinIntervalSec = plugins.MinLiveFeedIntervalSec
		}
		next[f.Key()] = f
	}
	h.mu.Lock()
	h.feeds = next
	h.feedsLoaded = true
	// Drop cached events/queries/details for removed or disabled feeds.
	for id, ce := range h.events {
		if _, ok := next[ce.feedKey]; !ok {
			delete(h.events, id)
			h.feedEventCounts[ce.feedKey]--
			if h.feedEventCounts[ce.feedKey] <= 0 {
				delete(h.feedEventCounts, ce.feedKey)
			}
		}
	}
	for id, tombstone := range h.eventTombstones {
		if _, active := next[tombstone.feedKey]; !active {
			delete(h.eventTombstones, id)
		}
	}
	for k := range h.feedLastPolled {
		if _, ok := next[k]; !ok {
			delete(h.feedLastPolled, k)
		}
	}
	for k := range h.feedHealth {
		if _, ok := next[k]; !ok {
			delete(h.feedHealth, k)
		}
	}
	for k := range h.detailCache {
		if p, f, _, ok := core.ParseLiveEventID(k); !ok {
			delete(h.detailCache, k)
		} else if _, active := next[p+":"+f]; !active {
			delete(h.detailCache, k)
		}
	}
	for k := range h.queryCache {
		if p, f, ok := splitNullPrefix2(k); !ok {
			delete(h.queryCache, k)
		} else if _, active := next[p+":"+f]; !active {
			delete(h.queryCache, k)
		}
	}
	for k := range h.entitiesCache {
		if p, f, ok := splitNullPrefix2(k); !ok {
			delete(h.entitiesCache, k)
		} else if _, active := next[p+":"+f]; !active {
			delete(h.entitiesCache, k)
		}
	}
	for k := range h.standingsCache {
		if p, f, ok := splitNullPrefix2(k); !ok {
			delete(h.standingsCache, k)
		} else if _, active := next[p+":"+f]; !active {
			delete(h.standingsCache, k)
		}
	}
	h.reconcilePollersLocked()
	h.mu.Unlock()
}

func (h *Hub) ensureFeedsLoaded(ctx context.Context) {
	h.mu.Lock()
	loaded := h.feedsLoaded
	h.mu.Unlock()
	if !loaded {
		h.SyncFeeds(ctx)
	}
}

// Feeds returns all currently active live feeds contributed by enabled plugins.
func (h *Hub) Feeds(ctx context.Context) []FeedInfo {
	if h == nil || h.source == nil {
		return nil
	}
	h.ensureFeedsLoaded(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.feeds) == 0 {
		return nil
	}
	keys := make([]string, 0, len(h.feeds))
	for k := range h.feeds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]FeedInfo, 0, len(keys))
	for _, k := range keys {
		f := h.feeds[k]
		if len(f.EntityKinds) > 0 {
			f.EntityKinds = append([]string(nil), f.EntityKinds...)
		}
		out = append(out, f)
	}
	return out
}

// HasLiveFeeds reports whether at least one enabled plugin currently contributes
// an active live event feed.
func (h *Hub) HasLiveFeeds(ctx context.Context) bool {
	if h == nil || h.source == nil {
		return false
	}
	h.ensureFeedsLoaded(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.feeds) > 0
}

func (h *Hub) lookupFeed(ctx context.Context, pluginID, feedID string) (FeedInfo, bool) {
	h.ensureFeedsLoaded(ctx)
	key := pluginID + ":" + feedID
	h.mu.Lock()
	info, ok := h.feeds[key]
	h.mu.Unlock()
	return info, ok
}

func (h *Hub) resolveFeedSpecs(ctx context.Context, specs []string) []FeedRef {
	if len(specs) == 0 {
		return nil
	}
	h.ensureFeedsLoaded(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[string]struct{}, len(specs))
	var out []FeedRef
	for _, raw := range specs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ref, ok := ParseFeedRef(raw); ok {
			key := ref.String()
			if _, active := h.feeds[key]; active {
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					out = append(out, ref)
				}
			}
			continue
		}
		// Match bare feed ID across active feeds (deterministic order).
		var matches []FeedInfo
		for _, f := range h.feeds {
			if f.FeedID == raw {
				matches = append(matches, f)
			}
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].Key() < matches[j].Key() })
		for _, f := range matches {
			key := f.Key()
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				out = append(out, f.Ref())
			}
		}
	}
	return out
}

// FetchEvents queries a plugin feed with single-flight deduplication and a
// short bounded TTL cache, updating the hub's event cache and fanning out any
// detected changes.
func (h *Hub) FetchEvents(ctx context.Context, pluginID, feedID string, q EventQuery) (core.LiveEventsResponse, error) {
	if h == nil || h.source == nil {
		return core.LiveEventsResponse{}, ErrUnavailable
	}
	info, ok := h.lookupFeed(ctx, pluginID, feedID)
	if !ok {
		return core.LiveEventsResponse{}, ErrNotFound
	}
	cacheKey := q.canonicalKey(pluginID, feedID)
	now := h.clock.Now()

	h.mu.Lock()
	if cq, ok := h.queryCache[cacheKey]; ok && now.Before(cq.expiresAt) {
		h.cacheSeq++
		cq.touchSeq = h.cacheSeq
		resp := cloneEventsResponse(cq.resp)
		h.mu.Unlock()
		return resp, nil
	}
	h.mu.Unlock()

	callCtx, cancel := context.WithTimeout(h.runCtx, defaultCallTimeout)
	defer cancel()
	fullQuery := q.From == "" && q.To == "" && q.League == "" && len(q.IDs) == 0
	resp, err := h.sfEvents.Do(ctx, callCtx, cacheKey, func(runCtx context.Context) (core.LiveEventsResponse, error) {
		// A caller that missed the cache just before an earlier flight
		// stored its answer must not start a second plugin call.
		h.mu.Lock()
		if cq, ok := h.queryCache[cacheKey]; ok && h.clock.Now().Before(cq.expiresAt) {
			res := cloneEventsResponse(cq.resp)
			h.mu.Unlock()
			return res, nil
		}
		h.mu.Unlock()
		res, retryAfter, err := h.callFeedEvents(runCtx, info, q)
		if err != nil {
			if runCtx.Err() == nil {
				h.recordFeedPoll(info, retryAfter, 0, fullQuery, err)
			}
			return core.LiveEventsResponse{}, err
		}
		ttl := h.recordFeedPoll(info, 0, res.NextPollSec, fullQuery, nil)
		h.ingestEvents(pluginID, feedID, res.Events, h.IsLeading())
		h.storeQueryCache(cacheKey, res, ttl)
		return res, nil
	})
	if err != nil {
		return core.LiveEventsResponse{}, err
	}
	return cloneEventsResponse(resp), nil
}

// GetEvents returns a viewer-scoped, spoiler-filtered, watch-enriched snapshot
// of the requested namespaced event IDs. Fresh cached events are returned
// without hitting the plugin; missing or stale IDs are fetched via single-flight.
func (h *Hub) GetEvents(ctx context.Context, viewer *core.Viewer, ids []string) ([]core.LiveEvent, error) {
	if h == nil || h.source == nil {
		return nil, ErrUnavailable
	}
	if len(ids) == 0 {
		return []core.LiveEvent{}, nil
	}
	h.ensureFeedsLoaded(ctx)

	// Deduplicate requested IDs preserving caller order.
	ordered := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	type feedGroup struct {
		pluginID string
		feedID   string
		localIDs []string
	}
	missingByFeed := make(map[string]*feedGroup)
	now := h.clock.Now()

	h.mu.Lock()
	found := make(map[string]core.LiveEvent, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		p, f, localID, ok := core.ParseLiveEventID(id)
		if !ok {
			continue
		}
		feedKey := p + ":" + f
		info, active := h.feeds[feedKey]
		if !active {
			continue
		}
		ordered = append(ordered, id)
		if ce, cached := h.events[id]; cached {
			minTTL := time.Duration(max(info.MinIntervalSec, plugins.MinLiveFeedIntervalSec)) * time.Second
			if now.Sub(ce.fetchedAt) < minTTL {
				h.cacheSeq++
				ce.touchSeq = h.cacheSeq
				found[id] = ce.event.Clone()
				continue
			}
		}
		grp := missingByFeed[feedKey]
		if grp == nil {
			grp = &feedGroup{pluginID: p, feedID: f}
			missingByFeed[feedKey] = grp
		}
		grp.localIDs = append(grp.localIDs, localID)
	}
	h.mu.Unlock()

	for _, grp := range missingByFeed {
		resp, err := h.FetchEvents(ctx, grp.pluginID, grp.feedID, EventQuery{IDs: grp.localIDs})
		if err != nil {
			// Fall back to stale cached copy if available.
			h.mu.Lock()
			for _, lid := range grp.localIDs {
				fullID := core.NamespaceLiveEventID(grp.pluginID, grp.feedID, lid)
				if ce, ok := h.events[fullID]; ok {
					found[fullID] = ce.event.Clone()
				}
			}
			h.mu.Unlock()
			continue
		}
		for _, ev := range resp.Events {
			found[ev.ID] = ev.Clone()
		}
	}

	out := make([]core.LiveEvent, 0, len(ordered))
	for _, id := range ordered {
		if ev, ok := found[id]; ok {
			out = append(out, ev)
		}
	}
	return h.prepareEventsForViewer(ctx, viewer, out), nil
}

// GetDetail returns the viewer-scoped, spoiler-filtered, watch-enriched detail
// payload for one namespaced event ID.
func (h *Hub) GetDetail(ctx context.Context, viewer *core.Viewer, eventID string) (*core.LiveEventDetail, error) {
	if h == nil || h.source == nil {
		return nil, ErrUnavailable
	}
	pluginID, feedID, localID, ok := core.ParseLiveEventID(strings.TrimSpace(eventID))
	if !ok {
		return nil, ErrInvalidRequest
	}
	info, active := h.lookupFeed(ctx, pluginID, feedID)
	if !active {
		return nil, ErrNotFound
	}
	fullID := core.NamespaceLiveEventID(pluginID, feedID, localID)
	now := h.clock.Now()

	h.mu.Lock()
	if cd, ok := h.detailCache[fullID]; ok && now.Before(cd.expiresAt) {
		h.cacheSeq++
		cd.touchSeq = h.cacheSeq
		detail := cd.detail.Clone()
		h.mu.Unlock()
		return h.prepareDetailForViewer(ctx, viewer, detail), nil
	}
	h.mu.Unlock()

	callCtx, cancel := context.WithTimeout(h.runCtx, defaultCallTimeout)
	defer cancel()
	detail, err := h.sfDetail.Do(ctx, callCtx, fullID, func(runCtx context.Context) (core.LiveEventDetail, error) {
		res, err := h.callEventDetail(runCtx, info, localID)
		if err != nil {
			return core.LiveEventDetail{}, err
		}
		ttl := time.Duration(max(info.MinIntervalSec, plugins.MinLiveFeedIntervalSec)) * time.Second
		h.storeDetailCache(fullID, res, ttl)
		h.ingestEvents(pluginID, feedID, []core.LiveEvent{res.LiveEvent}, h.IsLeading())
		return res, nil
	})
	if err != nil {
		return nil, err
	}
	return h.prepareDetailForViewer(ctx, viewer, detail), nil
}

// GetEntities fetches followable entities from a plugin feed via single-flight
// with bounded caching, populating each entity's namespaced Key.
func (h *Hub) GetEntities(ctx context.Context, pluginID, feedID, kind, league, q string) (core.LiveEventEntitiesResponse, error) {
	return h.getEntities(ctx, pluginID, feedID, kind, league, q, nil)
}

// GetEntitiesByIDs fetches specific followable entities by plugin-local IDs
// (used by screens to include a viewer's favourites from non-selected leagues
// without pulling the full catalogue).
func (h *Hub) GetEntitiesByIDs(ctx context.Context, pluginID, feedID, kind string, ids []string) (core.LiveEventEntitiesResponse, error) {
	return h.getEntities(ctx, pluginID, feedID, kind, "", "", ids)
}

func (h *Hub) getEntities(ctx context.Context, pluginID, feedID, kind, league, q string, ids []string) (core.LiveEventEntitiesResponse, error) {
	if h == nil || h.source == nil {
		return core.LiveEventEntitiesResponse{}, ErrUnavailable
	}
	info, ok := h.lookupFeed(ctx, pluginID, feedID)
	if !ok {
		return core.LiveEventEntitiesResponse{}, ErrNotFound
	}
	var cleanIDs []string
	var wantedSet map[string]struct{}
	if len(ids) > 0 {
		seen := make(map[string]struct{}, len(ids))
		cleanIDs = make([]string, 0, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			cleanIDs = append(cleanIDs, id)
		}
		sort.Strings(cleanIDs)
		wantedSet = seen
	}
	idsKey := strings.Join(cleanIDs, ",")
	cacheKey := strings.Join([]string{pluginID, feedID, strings.TrimSpace(kind), strings.TrimSpace(league), strings.TrimSpace(q), idsKey}, "\x00")
	now := h.clock.Now()

	h.mu.Lock()
	if ce, ok := h.entitiesCache[cacheKey]; ok && now.Before(ce.expiresAt) {
		h.cacheSeq++
		ce.touchSeq = h.cacheSeq
		resp := cloneEntitiesResponse(ce.resp)
		h.mu.Unlock()
		return resp, nil
	}
	h.mu.Unlock()

	callCtx, cancel := context.WithTimeout(h.runCtx, defaultCallTimeout)
	defer cancel()
	resp, err := h.sfEntities.Do(ctx, callCtx, cacheKey, func(runCtx context.Context) (core.LiveEventEntitiesResponse, error) {
		query := map[string]string{}
		if v := strings.TrimSpace(kind); v != "" {
			query["kind"] = v
		}
		if v := strings.TrimSpace(league); v != "" {
			query["league"] = v
		}
		if v := strings.TrimSpace(q); v != "" {
			query["q"] = v
		}
		if idsKey != "" {
			query["ids"] = idsKey
		}
		if len(query) == 0 {
			query = nil
		}
		path := "/ether/v1/live/" + url.PathEscape(info.FeedID) + "/entities"
		pr, err := h.source.Call(runCtx, info.PluginID, plugins.Request{
			Method: http.MethodGet,
			Path:   path,
			Query:  query,
		})
		if err != nil {
			return core.LiveEventEntitiesResponse{}, classifyPluginError(err, pr)
		}
		if pr.Status == http.StatusNotFound {
			return core.LiveEventEntitiesResponse{}, ErrNotFound
		}
		if pr.Status == http.StatusTooManyRequests {
			return core.LiveEventEntitiesResponse{}, ErrRateLimited
		}
		if pr.Status < 200 || pr.Status >= 300 {
			return core.LiveEventEntitiesResponse{}, fmt.Errorf("%w: plugin %s returned %d", ErrUnavailable, info.PluginID, pr.Status)
		}
		var out core.LiveEventEntitiesResponse
		if len(pr.Body) > 0 {
			if err := json.Unmarshal(pr.Body, &out); err != nil {
				return core.LiveEventEntitiesResponse{}, fmt.Errorf("%w: invalid entities payload: %v", ErrUnavailable, err)
			}
		}
		if len(wantedSet) > 0 && len(out.Entities) > 0 {
			filtered := make([]core.LiveEventEntity, 0, len(wantedSet))
			for _, e := range out.Entities {
				if _, match := wantedSet[e.ID]; match {
					filtered = append(filtered, e)
				}
			}
			out.Entities = filtered
		}
		if out.Entities == nil {
			out.Entities = []core.LiveEventEntity{}
		}
		for i := range out.Entities {
			e := &out.Entities[i]
			if e.Key == "" && e.Kind != "" && e.ID != "" {
				e.Key = core.NamespaceLiveEntityKey(info.PluginID, info.FeedID, e.Kind, e.ID)
			}
			e.Favorite = false
		}
		h.storeEntitiesCache(cacheKey, out, entitiesCacheTTL)
		return out, nil
	})
	if err != nil {
		return core.LiveEventEntitiesResponse{}, err
	}
	return cloneEntitiesResponse(resp), nil
}

// GetStandings fetches standings tables from a plugin feed via single-flight
// with bounded caching.
func (h *Hub) GetStandings(ctx context.Context, pluginID, feedID, league string) (core.LiveEventStandingsResponse, error) {
	if h == nil || h.source == nil {
		return core.LiveEventStandingsResponse{}, ErrUnavailable
	}
	info, ok := h.lookupFeed(ctx, pluginID, feedID)
	if !ok {
		return core.LiveEventStandingsResponse{}, ErrNotFound
	}
	cacheKey := strings.Join([]string{pluginID, feedID, strings.TrimSpace(league)}, "\x00")
	now := h.clock.Now()

	h.mu.Lock()
	if cs, ok := h.standingsCache[cacheKey]; ok && now.Before(cs.expiresAt) {
		h.cacheSeq++
		cs.touchSeq = h.cacheSeq
		resp := cloneStandingsResponse(cs.resp)
		h.mu.Unlock()
		return resp, nil
	}
	h.mu.Unlock()

	callCtx, cancel := context.WithTimeout(h.runCtx, defaultCallTimeout)
	defer cancel()
	resp, err := h.sfStandings.Do(ctx, callCtx, cacheKey, func(runCtx context.Context) (core.LiveEventStandingsResponse, error) {
		var query map[string]string
		if v := strings.TrimSpace(league); v != "" {
			query = map[string]string{"league": v}
		}
		path := "/ether/v1/live/" + url.PathEscape(info.FeedID) + "/standings"
		pr, err := h.source.Call(runCtx, info.PluginID, plugins.Request{
			Method: http.MethodGet,
			Path:   path,
			Query:  query,
		})
		if err != nil {
			return core.LiveEventStandingsResponse{}, classifyPluginError(err, pr)
		}
		if pr.Status == http.StatusNotFound {
			return core.LiveEventStandingsResponse{}, ErrNotFound
		}
		if pr.Status == http.StatusTooManyRequests {
			return core.LiveEventStandingsResponse{}, ErrRateLimited
		}
		if pr.Status < 200 || pr.Status >= 300 {
			return core.LiveEventStandingsResponse{}, fmt.Errorf("%w: plugin %s returned %d", ErrUnavailable, info.PluginID, pr.Status)
		}
		var out core.LiveEventStandingsResponse
		if len(pr.Body) > 0 {
			if err := json.Unmarshal(pr.Body, &out); err != nil {
				return core.LiveEventStandingsResponse{}, fmt.Errorf("%w: invalid standings payload: %v", ErrUnavailable, err)
			}
		}
		if out.Tables == nil {
			out.Tables = []core.LiveEventTable{}
		}
		h.storeStandingsCache(cacheKey, out, standingsCacheTTL)
		return out, nil
	})
	if err != nil {
		return core.LiveEventStandingsResponse{}, err
	}
	return cloneStandingsResponse(resp), nil
}

// Subscribe registers a live subscription for events, feeds, screens, and/or
// viewer alerts. Subscribing to a feed starts polling on the leader (or
// announces interest from a follower); closing the subscription stops polling
// when no subscribers remain.
func (h *Hub) Subscribe(ctx context.Context, req SubscribeRequest) (*Subscription, error) {
	if h == nil {
		return nil, ErrUnavailable
	}
	h.ensureFeedsLoaded(ctx)

	feeds := make(map[string]struct{})
	eventIDs := make(map[string]struct{})

	for _, ref := range h.resolveFeedSpecs(ctx, req.Feeds) {
		feeds[ref.String()] = struct{}{}
	}
	for _, rawID := range req.EventIDs {
		id := strings.TrimSpace(rawID)
		if p, f, _, ok := core.ParseLiveEventID(id); ok {
			if _, full := feeds[p+":"+f]; !full {
				eventIDs[id] = struct{}{}
			}
		}
	}

	viewerID := req.viewerID()
	sub := &Subscription{
		hub:            h,
		viewer:         req.Viewer,
		viewerID:       viewerID,
		filterSpoilers: req.FilterSpoilers,
		invalidateOnly: req.InvalidateOnly,
		wantsAlerts:    req.Alerts && viewerID > 0,
		feeds:          feeds,
		eventIDs:       eventIDs,
		updates:        make(chan []core.LiveEvent, defaultSubBuffer),
		lastSentHash:   make(map[string]string),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrUnavailable
	}
	if len(h.subs) >= h.maxSubscribers {
		h.mu.Unlock()
		return nil, ErrTooManySubscribers
	}
	h.nextSubID++
	sub.id = h.nextSubID
	if sub.wantsAlerts && h.alertHub != nil {
		sub.alertSub = h.alertHub.SubscribeAlerts(viewerID)
	}
	h.subs[sub.id] = sub
	leading := h.leading
	if leading {
		h.reconcilePollersLocked()
	} else {
		h.reconcileInterestRefresherLocked()
	}
	h.mu.Unlock()

	if !leading && (len(feeds) > 0 || len(eventIDs) > 0) {
		h.publishLocalInterest(ctx)
	}
	return sub, nil
}

// SubscriptionSnapshot returns the current state of all events covered by sub
// (viewer-scoped, spoiler-filtered, and watch-enriched) and records their
// visible hashes on sub so immediate follow-up polls only send changes.
// Used by /vela/v0/live/stream to send the initial update frame on connect.
func (h *Hub) SubscriptionSnapshot(ctx context.Context, sub *Subscription) ([]core.LiveEvent, error) {
	if h == nil || sub == nil {
		return []core.LiveEvent{}, nil
	}
	var feedRefs []FeedRef
	for fk := range sub.feeds {
		if ref, ok := ParseFeedRef(fk); ok {
			feedRefs = append(feedRefs, ref)
		}
	}
	sort.Slice(feedRefs, func(i, j int) bool { return feedRefs[i].String() < feedRefs[j].String() })

	var eventIDs []string
	for eid := range sub.eventIDs {
		eventIDs = append(eventIDs, eid)
	}
	sort.Strings(eventIDs)

	seen := make(map[string]struct{})
	var raw []core.LiveEvent

	for _, ref := range feedRefs {
		fk := ref.String()
		cached := h.cachedEventsForFeed(fk)
		if len(cached) == 0 && h.source != nil {
			if resp, err := h.FetchEvents(ctx, ref.PluginID, ref.FeedID, EventQuery{}); err == nil {
				cached = resp.Events
			}
		}
		for _, ev := range cached {
			if _, dup := seen[ev.ID]; dup {
				continue
			}
			seen[ev.ID] = struct{}{}
			raw = append(raw, ev.Clone())
		}
	}

	if len(eventIDs) > 0 {
		var missing []string
		h.mu.Lock()
		for _, eid := range eventIDs {
			if _, dup := seen[eid]; dup {
				continue
			}
			if ce, ok := h.events[eid]; ok {
				seen[eid] = struct{}{}
				raw = append(raw, ce.event.Clone())
			} else {
				missing = append(missing, eid)
			}
		}
		h.mu.Unlock()

		if len(missing) > 0 && h.source != nil {
			byFeed := make(map[FeedRef][]string)
			for _, eid := range missing {
				if p, f, lid, ok := core.ParseLiveEventID(eid); ok {
					ref := FeedRef{PluginID: p, FeedID: f}
					byFeed[ref] = append(byFeed[ref], lid)
				}
			}
			for ref, lids := range byFeed {
				if resp, err := h.FetchEvents(ctx, ref.PluginID, ref.FeedID, EventQuery{IDs: lids}); err == nil {
					for _, ev := range resp.Events {
						if _, dup := seen[ev.ID]; !dup {
							seen[ev.ID] = struct{}{}
							raw = append(raw, ev.Clone())
						}
					}
				}
			}
		}
	}

	sort.Slice(raw, func(i, j int) bool { return raw[i].ID < raw[j].ID })
	if len(raw) == 0 {
		return []core.LiveEvent{}, nil
	}

	var out []core.LiveEvent
	if sub.filterSpoilers && sub.invalidateOnly {
		out = h.ApplySpoilers(ctx, sub.viewerWithID(), raw)
	} else if sub.filterSpoilers {
		out = h.prepareEventsForViewer(ctx, sub.viewerWithID(), raw)
	} else {
		out = raw
	}

	sub.sendMu.Lock()
	// Merge any concurrent updates that raced with snapshot collection so the
	// initial update frame carries the newest state without duplicate frames.
	for {
		select {
		case pending := <-sub.updates:
			out = mergeEventBatches(out, pending)
		default:
			goto drained
		}
	}
drained:
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for _, ev := range out {
		if len(sub.lastSentHash) >= maxSubSentHashes {
			sub.lastSentHash = make(map[string]string)
		}
		sub.lastSentHash[ev.ID] = VisibleHash(ev)
	}
	sub.sendMu.Unlock()
	return out, nil
}

func (s *Subscription) viewerWithID() *core.Viewer {
	if s.viewer != nil {
		return s.viewer
	}
	if s.viewerID > 0 {
		return &core.Viewer{ID: s.viewerID}
	}
	return nil
}

func (h *Hub) unsubscribe(subID int64) {
	h.mu.Lock()
	sub, ok := h.subs[subID]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, subID)
	alertSub := sub.alertSub
	leading := h.leading
	hadInterest := len(sub.feeds) > 0 || len(sub.eventIDs) > 0
	if leading {
		h.reconcilePollersLocked()
	} else {
		h.reconcileInterestRefresherLocked()
	}
	h.mu.Unlock()

	if alertSub != nil {
		alertSub.Close()
	}
	if !leading && hadInterest {
		h.publishLocalInterest(h.runCtx)
	}
}

// PublishAlert delivers an in-app alert to viewerID's open alert streams on this
// node and relays it over clusterbus so follower nodes deliver it as well.
// Spoiler-free viewers receive start alerts only (A7).
func (h *Hub) PublishAlert(ctx context.Context, viewerID int64, alert core.LiveEventAlert) {
	_ = h.publishAlert(ctx, viewerID, alert)
}

func (h *Hub) publishAlert(ctx context.Context, viewerID int64, alert core.LiveEventAlert) int {
	if h == nil || viewerID <= 0 {
		return 0
	}
	if ctx == nil {
		ctx = h.runCtx
	}
	if alert.At == "" {
		alert.At = h.clock.Now().UTC().Format(time.RFC3339)
	}
	delivered := h.deliverAlertLocal(ctx, viewerID, alert)
	h.publishClusterAlert(ctx, viewerID, alert)
	return delivered
}

func (h *Hub) deliverAlertLocal(ctx context.Context, viewerID int64, alert core.LiveEventAlert) int {
	if h == nil || h.alertHub == nil || h.alertHub.SubscriberCount(viewerID) == 0 {
		return 0
	}
	spoilerFree, _ := h.spoilers.SpoilerFree(ctx, viewerID)
	if spoilerFree {
		revealed := false
		if alert.EventID != "" {
			revealed, _ = h.spoilers.IsRevealed(ctx, viewerID, alert.EventID)
		}
		if !revealed && !AllowAlertWhenSpoilerFree(alert.Kind) {
			return 0
		}
	}
	return h.alertHub.PublishAlert(viewerID, alert)
}

// SpoilerFree returns viewerID's spoilerFree preference (default false).
func (h *Hub) SpoilerFree(ctx context.Context, viewerID int64) (bool, error) {
	if h == nil {
		return false, nil
	}
	return h.spoilers.SpoilerFree(ctx, viewerID)
}

// SetSpoilerFree updates viewerID's spoilerFree preference and clears
// per-subscription visible hashes so subsequent updates reflect the change.
func (h *Hub) SetSpoilerFree(ctx context.Context, viewerID int64, enabled bool) error {
	if h == nil {
		return ErrUnavailable
	}
	if err := h.spoilers.SetSpoilerFree(ctx, viewerID, enabled); err != nil {
		return err
	}
	h.clearViewerSentHashes(viewerID)
	return nil
}

// IsEventRevealed reports whether viewerID has revealed scores for eventID.
func (h *Hub) IsEventRevealed(ctx context.Context, viewerID int64, eventID string) (bool, error) {
	if h == nil {
		return false, nil
	}
	return h.spoilers.IsRevealed(ctx, viewerID, eventID)
}

// RevealEvent records an explicit per-viewer reveal for eventID, pushes an
// immediate unmasked update to any open stream of that viewer watching eventID,
// and returns the unmasked event if present in cache.
func (h *Hub) RevealEvent(ctx context.Context, viewer *core.Viewer, eventID string) (*core.LiveEvent, error) {
	if h == nil {
		return nil, ErrUnavailable
	}
	if viewer == nil || viewer.ID <= 0 {
		return nil, core.ErrUnauthorized
	}
	eventID = strings.TrimSpace(eventID)
	p, f, _, ok := core.ParseLiveEventID(eventID)
	if !ok {
		return nil, ErrInvalidRequest
	}
	if err := h.spoilers.Reveal(ctx, viewer.ID, eventID); err != nil {
		return nil, err
	}
	feedKey := p + ":" + f

	var cached *core.LiveEvent
	h.mu.Lock()
	if ce, exists := h.events[eventID]; exists {
		cp := ce.event.Clone()
		// Advance updatedAt on the viewer's revealed copy so clients merging by
		// updatedAt replace the previously spoiler-masked event immediately.
		cp.UpdatedAt = bumpUpdatedAt(cp.UpdatedAt, h.clock.Now())
		cached = &cp
	}
	var targets []*Subscription
	for _, sub := range h.subs {
		if sub.viewerID == viewer.ID && sub.matches(feedKey, eventID) {
			targets = append(targets, sub)
		}
	}
	h.mu.Unlock()

	if cached == nil {
		return nil, nil
	}
	prepared := h.prepareEventsForViewer(ctx, viewer, []core.LiveEvent{*cached})
	if len(prepared) == 0 {
		return nil, nil
	}
	ev := prepared[0]
	visHash := VisibleHash(ev)
	for _, sub := range targets {
		sub.sendMu.Lock()
		alreadySent := sub.lastSentHash[ev.ID] == visHash
		if !alreadySent {
			sub.lastSentHash[ev.ID] = visHash
		}
		sub.sendMu.Unlock()
		if !alreadySent {
			sub.enqueueUpdates([]core.LiveEvent{ev.Clone()})
		}
	}
	return &ev, nil
}

func (h *Hub) clearViewerSentHashes(viewerID int64) {
	h.mu.Lock()
	var targets []*Subscription
	for _, sub := range h.subs {
		if sub.viewerID == viewerID {
			targets = append(targets, sub)
		}
	}
	h.mu.Unlock()
	for _, sub := range targets {
		sub.sendMu.Lock()
		sub.lastSentHash = make(map[string]string)
		sub.sendMu.Unlock()
	}
}

// ApplySpoilers returns viewer-scoped, spoiler-filtered clones of events
// without invoking the watch enricher (callers that attach watch targets
// separately, such as screens, use this to avoid duplicate watch matching).
func (h *Hub) ApplySpoilers(ctx context.Context, viewer *core.Viewer, events []core.LiveEvent) []core.LiveEvent {
	if len(events) == 0 {
		return []core.LiveEvent{}
	}
	out := make([]core.LiveEvent, len(events))
	for i, ev := range events {
		out[i] = ev.Clone()
	}
	if h == nil || viewer == nil || viewer.ID <= 0 {
		return out
	}
	spoilerFree, err := h.spoilers.SpoilerFree(ctx, viewer.ID)
	if err != nil || !spoilerFree {
		return out
	}
	for i := range out {
		revealed, _ := h.spoilers.IsRevealed(ctx, viewer.ID, out[i].ID)
		if !revealed {
			out[i] = MaskEvent(out[i])
		}
	}
	return out
}

func (h *Hub) prepareEventsForViewer(ctx context.Context, viewer *core.Viewer, events []core.LiveEvent) []core.LiveEvent {
	if len(events) == 0 {
		return []core.LiveEvent{}
	}
	out := make([]core.LiveEvent, len(events))
	for i, ev := range events {
		out[i] = ev.Clone()
	}
	h.mu.Lock()
	enricher := h.watchEnricher
	h.mu.Unlock()
	if enricher != nil && viewer != nil {
		enricher(ctx, viewer, out)
	}
	if viewer == nil || viewer.ID <= 0 {
		return out
	}
	spoilerFree, err := h.spoilers.SpoilerFree(ctx, viewer.ID)
	if err != nil || !spoilerFree {
		return out
	}
	for i := range out {
		revealed, _ := h.spoilers.IsRevealed(ctx, viewer.ID, out[i].ID)
		if !revealed {
			out[i] = MaskEvent(out[i])
		}
	}
	return out
}

func (h *Hub) prepareDetailForViewer(ctx context.Context, viewer *core.Viewer, d core.LiveEventDetail) *core.LiveEventDetail {
	out := d.Clone()
	h.mu.Lock()
	detailEnricher := h.detailWatchEnricher
	enricher := h.watchEnricher
	h.mu.Unlock()
	if viewer != nil {
		if detailEnricher != nil {
			detailEnricher(ctx, viewer, &out)
		} else if enricher != nil {
			slice := []core.LiveEvent{out.LiveEvent}
			enricher(ctx, viewer, slice)
			out.LiveEvent = slice[0]
		}
	}
	if viewer == nil || viewer.ID <= 0 {
		return &out
	}
	spoilerFree, err := h.spoilers.SpoilerFree(ctx, viewer.ID)
	if err != nil || !spoilerFree {
		return &out
	}
	revealed, _ := h.spoilers.IsRevealed(ctx, viewer.ID, out.ID)
	if !revealed {
		out = MaskDetail(out)
	}
	return &out
}

// --- Poller management (leader only, zero idle work) ---

func (h *Hub) desiredPollFeedsLocked() map[string]struct{} {
	if !h.leading || h.closed {
		return nil
	}
	now := h.clock.Now()
	desired := make(map[string]struct{})
	addFeed := func(fk string) {
		if _, active := h.feeds[fk]; active {
			desired[fk] = struct{}{}
		}
	}
	for _, sub := range h.subs {
		for fk := range sub.feeds {
			addFeed(fk)
		}
		for eid := range sub.eventIDs {
			if p, f, _, ok := core.ParseLiveEventID(eid); ok {
				addFeed(p + ":" + f)
			}
		}
	}
	for _, peer := range h.remoteInterests {
		if !now.Before(peer.expires) {
			continue
		}
		for fk := range peer.feeds {
			addFeed(fk)
		}
		for eid := range peer.eventIDs {
			if p, f, _, ok := core.ParseLiveEventID(eid); ok {
				addFeed(p + ":" + f)
			}
		}
	}
	return desired
}

func (h *Hub) reconcilePollersLocked() {
	desired := h.desiredPollFeedsLocked()
	// Stop pollers no longer needed.
	for fk, p := range h.pollers {
		if _, want := desired[fk]; !want {
			p.cancel()
			delete(h.pollers, fk)
		}
	}
	// Start pollers for newly subscribed feeds.
	for fk := range desired {
		if _, running := h.pollers[fk]; running {
			continue
		}
		pCtx, cancel := context.WithCancel(h.runCtx)
		p := &feedPoller{
			feedKey: fk,
			cancel:  cancel,
			done:    make(chan struct{}),
		}
		h.pollers[fk] = p
		h.wg.Add(1)
		go func(feedKey string, ctx context.Context, done chan struct{}) {
			defer h.wg.Done()
			defer close(done)
			h.runFeedPoller(ctx, feedKey)
		}(fk, pCtx, p.done)
	}
}

func (h *Hub) stopAllPollersLocked() {
	for fk, p := range h.pollers {
		p.cancel()
		delete(h.pollers, fk)
	}
}

func (h *Hub) pollQueryForFeed(feedKey string) (FeedInfo, EventQuery, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	info, ok := h.feeds[feedKey]
	if !ok {
		return FeedInfo{}, EventQuery{}, false
	}
	wantFull := false
	localIDs := make(map[string]struct{})
	now := h.clock.Now()

	collect := func(feeds map[string]struct{}, eventIDs map[string]struct{}) {
		if _, ok := feeds[feedKey]; ok {
			wantFull = true
			return
		}
		for eid := range eventIDs {
			p, f, lid, ok := core.ParseLiveEventID(eid)
			if ok && p+":"+f == feedKey {
				localIDs[lid] = struct{}{}
			}
		}
	}
	for _, sub := range h.subs {
		collect(sub.feeds, sub.eventIDs)
		if wantFull {
			break
		}
	}
	if !wantFull {
		for _, peer := range h.remoteInterests {
			if !now.Before(peer.expires) {
				continue
			}
			collect(peer.feeds, peer.eventIDs)
			if wantFull {
				break
			}
		}
	}
	if wantFull || len(localIDs) == 0 || len(localIDs) > maxTargetedPollIDs {
		return info, EventQuery{}, true
	}
	ids := make([]string, 0, len(localIDs))
	for lid := range localIDs {
		ids = append(ids, lid)
	}
	sort.Strings(ids)
	return info, EventQuery{IDs: ids}, true
}

func (h *Hub) runFeedPoller(ctx context.Context, feedKey string) {
	var timer Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	// Respect feed minIntervalSec and any active error backoff / nextPollAt if
	// this feed was polled recently (so brief subscriptions never force extra polling).
	h.mu.Lock()
	info, ok := h.feeds[feedKey]
	lastPolled := h.feedLastPolled[feedKey]
	var waitUntil time.Time
	minInterval := time.Duration(max(info.MinIntervalSec, plugins.MinLiveFeedIntervalSec)) * time.Second
	if !lastPolled.IsZero() {
		waitUntil = lastPolled.Add(minInterval)
	}
	if fh := h.feedHealth[feedKey]; fh != nil && fh.NextPollAt.After(waitUntil) {
		waitUntil = fh.NextPollAt
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	if wait := waitUntil.Sub(h.clock.Now()); wait > 0 {
		timer = h.clock.NewTimer(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}
		info, q, active := h.pollQueryForFeed(feedKey)
		if !active {
			return
		}
		baseInterval := time.Duration(max(info.MinIntervalSec, plugins.MinLiveFeedIntervalSec)) * time.Second

		h.mu.Lock()
		h.feedLastPolled[feedKey] = h.clock.Now()
		h.mu.Unlock()

		callCtx, cancel := context.WithTimeout(ctx, defaultCallTimeout)
		cacheKey := q.canonicalKey(info.PluginID, info.FeedID)
		var retryAfter time.Duration
		var pollDelay time.Duration
		resp, err := h.sfEvents.Do(ctx, callCtx, cacheKey, func(runCtx context.Context) (core.LiveEventsResponse, error) {
			res, ra, callErr := h.callFeedEvents(runCtx, info, q)
			retryAfter = ra
			if callErr != nil {
				if runCtx.Err() == nil {
					pollDelay = h.recordFeedPoll(info, ra, 0, true, callErr)
				}
				return core.LiveEventsResponse{}, callErr
			}
			ttl := h.recordFeedPoll(info, 0, res.NextPollSec, true, nil)
			pollDelay = ttl
			h.ingestEvents(info.PluginID, info.FeedID, res.Events, true)
			h.storeQueryCache(cacheKey, res, ttl)
			return res, nil
		})
		cancel()

		var delay time.Duration
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			delay = pollDelay
			if delay <= 0 {
				h.mu.Lock()
				errs := 1
				if fh := h.feedHealth[feedKey]; fh != nil && fh.ConsecutiveErrors > 0 {
					errs = fh.ConsecutiveErrors
				}
				h.mu.Unlock()
				delay = errorBackoff(baseInterval, errs, retryAfter)
			}
			h.log.Debug("liveevents: feed poll failed", "feed", feedKey, "err", err, "backoff", delay)
		} else {
			delay = pollDelay
			if delay <= 0 {
				delay = effectivePollInterval(info, resp.NextPollSec)
			}
		}

		if timer == nil {
			timer = h.clock.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
		}
	}
}

func (h *Hub) recordFeedPoll(info FeedInfo, retryAfter time.Duration, nextPollSec int, updateThrottle bool, pollErr error) time.Duration {
	baseInterval := time.Duration(max(info.MinIntervalSec, plugins.MinLiveFeedIntervalSec)) * time.Second
	now := h.clock.Now()
	fk := info.Key()

	h.mu.Lock()
	if updateThrottle {
		h.feedLastPolled[fk] = now
	}
	st := h.feedHealth[fk]
	if st == nil {
		st = &FeedHealth{}
		h.feedHealth[fk] = st
	}
	st.LastPollAt = now
	var delay time.Duration
	if pollErr == nil {
		delay = effectivePollInterval(info, nextPollSec)
		st.LastOkAt = now
		st.LastError = ""
		st.ConsecutiveErrors = 0
		st.NextPollSec = int(delay / time.Second)
		if updateThrottle {
			st.NextPollAt = now.Add(delay)
		} else if st.NextPollAt.Before(now) {
			st.NextPollAt = time.Time{}
		}
	} else {
		st.ConsecutiveErrors++
		delay = errorBackoff(baseInterval, st.ConsecutiveErrors, retryAfter)
		st.LastError = pollErr.Error()
		st.NextPollSec = int(delay / time.Second)
		st.NextPollAt = now.Add(delay)
	}
	hooks := make([]FeedPollHandler, 0, len(h.pollHooks))
	for _, fn := range h.pollHooks {
		hooks = append(hooks, fn)
	}
	h.mu.Unlock()

	for _, fn := range hooks {
		fn(info.PluginID, info.FeedID)
	}
	return delay
}

// PluginCachedSnapshot returns cached feed overviews (health, leagues, entity
// counts) and cached events for pluginID without calling the plugin.
func (h *Hub) PluginCachedSnapshot(ctx context.Context, pluginID string) ([]FeedOverview, []core.LiveEvent) {
	if h == nil || pluginID == "" {
		return nil, nil
	}
	h.ensureFeedsLoaded(ctx)
	now := h.clock.Now()

	h.mu.Lock()
	defer h.mu.Unlock()

	var infos []FeedInfo
	for _, info := range h.feeds {
		if info.PluginID == pluginID {
			infos = append(infos, info)
		}
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].FeedID < infos[j].FeedID })

	overviews := make([]FeedOverview, 0, len(infos))
	var allEvents []core.LiveEvent

	for _, info := range infos {
		fk := info.Key()
		defaultKind := "entity"
		if len(info.EntityKinds) > 0 && info.EntityKinds[0] != "" {
			defaultKind = info.EntityKinds[0]
		}

		leaguesByID := make(map[string]*FeedLeagueSummary)
		entitySets := make(map[string]map[string]struct{})
		addEntity := func(kind, id string) {
			kind = strings.TrimSpace(kind)
			id = strings.TrimSpace(id)
			if kind == "" || id == "" {
				return
			}
			set := entitySets[kind]
			if set == nil {
				set = make(map[string]struct{})
				entitySets[kind] = set
			}
			set[id] = struct{}{}
		}

		var evCount, liveCount, upcomingCount int
		for _, ce := range h.events {
			if ce.feedKey != fk {
				continue
			}
			ev := ce.event.Clone()
			allEvents = append(allEvents, ev)
			evCount++
			switch ev.Status.State {
			case core.LiveEventStateIn, core.LiveEventStateDelayed:
				liveCount++
			case core.LiveEventStatePre:
				upcomingCount++
			}
			if lid := strings.TrimSpace(ev.League.ID); lid != "" {
				ls := leaguesByID[lid]
				if ls == nil {
					name := strings.TrimSpace(ev.League.Name)
					if name == "" {
						name = lid
					}
					ls = &FeedLeagueSummary{ID: lid, Name: name, Abbr: strings.TrimSpace(ev.League.Abbr)}
					leaguesByID[lid] = ls
				}
				ls.EventCount++
			}
			for _, comp := range ev.Competitors {
				if comp.EntityID != "" {
					addEntity(defaultKind, comp.EntityID)
				}
			}
			for _, row := range ev.Ranking {
				if row.EntityID != "" {
					addEntity(defaultKind, row.EntityID)
				}
			}
		}

		for ck, cached := range h.entitiesCache {
			if !now.Before(cached.expiresAt) {
				continue
			}
			p, f, ok := splitNullPrefix2(ck)
			if !ok || p != info.PluginID || f != info.FeedID {
				continue
			}
			for _, ent := range cached.resp.Entities {
				kind := ent.Kind
				if kind == "" {
					kind = defaultKind
				}
				addEntity(kind, ent.ID)
				if lid := strings.TrimSpace(ent.League); lid != "" {
					if _, exists := leaguesByID[lid]; !exists {
						leaguesByID[lid] = &FeedLeagueSummary{ID: lid, Name: lid}
					}
				}
			}
		}

		leagues := make([]FeedLeagueSummary, 0, len(leaguesByID))
		for _, ls := range leaguesByID {
			leagues = append(leagues, *ls)
		}
		sort.Slice(leagues, func(i, j int) bool {
			if leagues[i].EventCount != leagues[j].EventCount {
				return leagues[i].EventCount > leagues[j].EventCount
			}
			if leagues[i].Name != leagues[j].Name {
				return leagues[i].Name < leagues[j].Name
			}
			return leagues[i].ID < leagues[j].ID
		})

		var entityCounts map[string]int
		entityTotal := 0
		if len(entitySets) > 0 {
			entityCounts = make(map[string]int, len(entitySets))
			for kind, set := range entitySets {
				entityCounts[kind] = len(set)
				entityTotal += len(set)
			}
		}

		subs := 0
		for _, sub := range h.subs {
			if _, ok := sub.feeds[fk]; ok {
				subs++
				continue
			}
			for eid := range sub.eventIDs {
				if p, f, _, ok := core.ParseLiveEventID(eid); ok && p+":"+f == fk {
					subs++
					break
				}
			}
		}
		for _, peer := range h.remoteInterests {
			if !now.Before(peer.expires) {
				continue
			}
			if _, ok := peer.feeds[fk]; ok {
				subs++
				continue
			}
			for eid := range peer.eventIDs {
				if p, f, _, ok := core.ParseLiveEventID(eid); ok && p+":"+f == fk {
					subs++
					break
				}
			}
		}

		var health FeedHealth
		if fh := h.feedHealth[fk]; fh != nil {
			health = *fh
		}
		health.Subscribers = subs
		_, health.Polling = h.pollers[fk]

		overviews = append(overviews, FeedOverview{
			FeedID:         info.FeedID,
			Title:          info.Title,
			MinIntervalSec: info.MinIntervalSec,
			EntityKinds:    append([]string(nil), info.EntityKinds...),
			Active:         true,
			Health:         health,
			EventCount:     evCount,
			LiveCount:      liveCount,
			UpcomingCount:  upcomingCount,
			Leagues:        leagues,
			EntityCounts:   entityCounts,
			EntityTotal:    entityTotal,
		})
	}

	sort.Slice(allEvents, func(i, j int) bool { return allEvents[i].ID < allEvents[j].ID })
	return overviews, allEvents
}

func effectivePollInterval(info FeedInfo, nextPollSec int) time.Duration {
	sec := max(info.MinIntervalSec, nextPollSec, plugins.MinLiveFeedIntervalSec)
	d := time.Duration(sec) * time.Second
	if d > maxPollInterval {
		return maxPollInterval
	}
	return d
}

func errorBackoff(base time.Duration, consecutiveErrors int, retryAfter time.Duration) time.Duration {
	shift := consecutiveErrors - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 5 {
		shift = 5
	}
	d := base << shift
	if d > maxErrorBackoff {
		d = maxErrorBackoff
	}
	if retryAfter > d {
		d = retryAfter
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

// --- Plugin HTTP callers ---

func (h *Hub) callFeedEvents(ctx context.Context, info FeedInfo, q EventQuery) (core.LiveEventsResponse, time.Duration, error) {
	path := "/ether/v1/live/" + url.PathEscape(info.FeedID) + "/events"
	pr, err := h.source.Call(ctx, info.PluginID, plugins.Request{
		Method: http.MethodGet,
		Path:   path,
		Query:  q.queryParams(),
	})
	if err != nil {
		return core.LiveEventsResponse{}, parseRetryAfter(pr.Headers, h.clock.Now()), classifyPluginError(err, pr)
	}
	if pr.Status == http.StatusNotFound {
		return core.LiveEventsResponse{}, 0, ErrNotFound
	}
	if pr.Status == http.StatusTooManyRequests {
		return core.LiveEventsResponse{}, parseRetryAfter(pr.Headers, h.clock.Now()), ErrRateLimited
	}
	if pr.Status < 200 || pr.Status >= 300 {
		return core.LiveEventsResponse{}, parseRetryAfter(pr.Headers, h.clock.Now()), fmt.Errorf("%w: plugin %s returned %d", ErrUnavailable, info.PluginID, pr.Status)
	}
	var out core.LiveEventsResponse
	if len(pr.Body) > 0 {
		if err := json.Unmarshal(pr.Body, &out); err != nil {
			return core.LiveEventsResponse{}, 0, fmt.Errorf("%w: invalid events payload: %v", ErrUnavailable, err)
		}
	}
	normalized := make([]core.LiveEvent, 0, len(out.Events))
	now := h.clock.Now().UTC().Format(time.RFC3339)
	for _, ev := range out.Events {
		if strings.TrimSpace(ev.ID) == "" {
			continue
		}
		ev = NormalizeEvent(info.PluginID, info.FeedID, ev)
		if ev.UpdatedAt == "" {
			ev.UpdatedAt = now
		}
		normalized = append(normalized, ev)
	}
	out.Events = normalized
	return out, 0, nil
}

func (h *Hub) callEventDetail(ctx context.Context, info FeedInfo, localID string) (core.LiveEventDetail, error) {
	path := "/ether/v1/live/" + url.PathEscape(info.FeedID) + "/events/" + url.PathEscape(localID)
	pr, err := h.source.Call(ctx, info.PluginID, plugins.Request{
		Method: http.MethodGet,
		Path:   path,
	})
	if err != nil {
		return core.LiveEventDetail{}, classifyPluginError(err, pr)
	}
	if pr.Status == http.StatusNotFound {
		return core.LiveEventDetail{}, ErrNotFound
	}
	if pr.Status == http.StatusTooManyRequests {
		return core.LiveEventDetail{}, ErrRateLimited
	}
	if pr.Status < 200 || pr.Status >= 300 {
		return core.LiveEventDetail{}, fmt.Errorf("%w: plugin %s returned %d", ErrUnavailable, info.PluginID, pr.Status)
	}
	var out core.LiveEventDetail
	if len(pr.Body) == 0 {
		return core.LiveEventDetail{}, ErrNotFound
	}
	if err := json.Unmarshal(pr.Body, &out); err != nil {
		return core.LiveEventDetail{}, fmt.Errorf("%w: invalid detail payload: %v", ErrUnavailable, err)
	}
	if strings.TrimSpace(out.ID) == "" {
		out.ID = localID
	}
	out = NormalizeDetail(info.PluginID, info.FeedID, out)
	if out.UpdatedAt == "" {
		out.UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)
	}
	return out, nil
}

func classifyPluginError(err error, pr plugins.Response) error {
	if errors.Is(err, plugins.ErrRateLimited) || pr.Status == http.StatusTooManyRequests {
		return fmt.Errorf("%w: %v", ErrRateLimited, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func parseRetryAfter(headers map[string]string, now time.Time) time.Duration {
	if len(headers) == 0 {
		return 0
	}
	var raw string
	for k, v := range headers {
		if strings.EqualFold(k, "Retry-After") {
			raw = strings.TrimSpace(v)
			break
		}
	}
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// --- Ingestion, diffing, bounded cache eviction, and fan-out ---

type eventDelta struct {
	prev *core.LiveEvent
	next core.LiveEvent
}

func (h *Hub) ingestEvents(pluginID, feedID string, events []core.LiveEvent, broadcastCluster bool) []core.LiveEvent {
	if len(events) == 0 {
		return nil
	}
	feedKey := pluginID + ":" + feedID
	now := h.clock.Now()

	var changed []core.LiveEvent
	var deltas []eventDelta
	var hooks []ChangeHandler
	var matchingSubs []*Subscription

	h.mu.Lock()
	batchStart := h.cacheSeq
	for _, ev := range events {
		if strings.TrimSpace(ev.ID) == "" {
			continue
		}
		ev = NormalizeEvent(pluginID, feedID, ev)
		if ev.UpdatedAt == "" {
			ev.UpdatedAt = now.UTC().Format(time.RFC3339)
		}
		nextHash := ContentHash(ev)
		h.cacheSeq++

		prev, exists := h.events[ev.ID]
		if exists {
			if IsStaleUpdate(prev.event.UpdatedAt, ev.UpdatedAt) {
				continue
			}
			prev.fetchedAt = now
			prev.touchSeq = h.cacheSeq
			if prev.hash == nextHash {
				continue
			}
			// Content changed: guarantee strictly advancing updatedAt so clients
			// merging by updatedAt never discard a real score/status change.
			if ev.UpdatedAt == prev.event.UpdatedAt {
				ev.UpdatedAt = bumpUpdatedAt(prev.event.UpdatedAt, now)
			}
			prevCopy := prev.event.Clone()
			prev.event = ev.Clone()
			prev.hash = nextHash
			changed = append(changed, ev.Clone())
			deltas = append(deltas, eventDelta{prev: &prevCopy, next: ev.Clone()})
		} else {
			tombstone, evicted := h.eventTombstones[ev.ID]
			if evicted && IsStaleUpdate(tombstone.updatedAt, ev.UpdatedAt) {
				continue
			}
			unchanged := evicted && tombstone.hash == nextHash
			if evicted && !unchanged && ev.UpdatedAt == tombstone.updatedAt {
				ev.UpdatedAt = bumpUpdatedAt(tombstone.updatedAt, now)
			}
			delete(h.eventTombstones, ev.ID)
			h.events[ev.ID] = &cachedEvent{
				event:     ev.Clone(),
				pluginID:  pluginID,
				feedID:    feedID,
				feedKey:   feedKey,
				hash:      nextHash,
				fetchedAt: now,
				touchSeq:  h.cacheSeq,
			}
			h.feedEventCounts[feedKey]++
			if unchanged {
				continue
			}
			changed = append(changed, ev.Clone())
			deltas = append(deltas, eventDelta{prev: nil, next: ev.Clone()})
		}
	}

	h.evictEventsLocked(feedKey, batchStart)
	if len(changed) > 0 {
		if len(h.changeHooks) > 0 {
			hooks = make([]ChangeHandler, 0, len(h.changeHooks))
			for _, fn := range h.changeHooks {
				hooks = append(hooks, fn)
			}
		}
		for _, sub := range h.subs {
			for _, ev := range changed {
				if sub.matches(feedKey, ev.ID) {
					matchingSubs = append(matchingSubs, sub)
					break
				}
			}
		}
	}
	h.mu.Unlock()

	if len(changed) == 0 {
		return nil
	}

	// Notify raw change observers (e.g. A7 alert detector).
	for _, fn := range hooks {
		for _, d := range deltas {
			fn(d.prev, d.next.Clone())
		}
	}

	// Fan out to matching local subscribers (viewer-scoped, spoiler-filtered).
	for _, sub := range matchingSubs {
		var matched []core.LiveEvent
		for _, ev := range changed {
			if sub.matches(feedKey, ev.ID) {
				matched = append(matched, ev.Clone())
			}
		}
		if len(matched) == 0 {
			continue
		}
		if sub.filterSpoilers && sub.invalidateOnly {
			matched = h.ApplySpoilers(h.runCtx, sub.viewerWithID(), matched)
		} else if sub.filterSpoilers {
			matched = h.prepareEventsForViewer(h.runCtx, sub.viewerWithID(), matched)
		}
		// Deduplicate against what this subscriber already saw (suppresses
		// hidden-only changes for spoiler-free viewers).
		var visible []core.LiveEvent
		sub.sendMu.Lock()
		for _, ev := range matched {
			vh := VisibleHash(ev)
			if prevHash, ok := sub.lastSentHash[ev.ID]; ok && prevHash == vh {
				continue
			}
			if len(sub.lastSentHash) >= maxSubSentHashes {
				sub.lastSentHash = make(map[string]string)
			}
			sub.lastSentHash[ev.ID] = vh
			visible = append(visible, ev)
		}
		sub.sendMu.Unlock()
		if len(visible) > 0 {
			sub.enqueueUpdates(visible)
		}
	}

	if broadcastCluster {
		h.broadcastClusterUpdates(pluginID, feedID, changed)
	}
	return changed
}

func (h *Hub) cachedEventsForFeed(feedKey string) []core.LiveEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []core.LiveEvent
	for _, ce := range h.events {
		if ce.feedKey == feedKey {
			out = append(out, ce.event.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) evictEventsLocked(feedKey string, batchStart int64) {
	evict := func(feed string, excess int) {
		if excess <= 0 {
			return
		}
		var candidates []*cachedEvent
		for _, ce := range h.events {
			if feed == "" || ce.feedKey == feed {
				candidates = append(candidates, ce)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			aInBatch, bInBatch := a.touchSeq > batchStart, b.touchSeq > batchStart
			if aInBatch != bInBatch {
				return !aInBatch
			}
			aLive := a.event.Status.State == core.LiveEventStateIn
			bLive := b.event.Status.State == core.LiveEventStateIn
			if aLive != bLive {
				return !aLive
			}
			return a.touchSeq < b.touchSeq
		})
		for _, ce := range candidates[:excess] {
			id := ce.event.ID
			delete(h.events, id)
			h.eventTombstones[id] = eventTombstone{ce.feedKey, ce.hash, ce.event.UpdatedAt, ce.touchSeq}
			h.feedEventCounts[ce.feedKey]--
			if h.feedEventCounts[ce.feedKey] == 0 {
				delete(h.feedEventCounts, ce.feedKey)
			}
		}
	}

	evict(feedKey, h.feedEventCounts[feedKey]-h.maxEventsPerFeed)
	evict("", len(h.events)-h.maxTotalEvents)

	limit := max(h.maxTotalEvents, defaultMaxTotalEvents)
	if excess := len(h.eventTombstones) - limit; excess > 0 {
		ids := make([]string, 0, len(h.eventTombstones))
		for id := range h.eventTombstones {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			return h.eventTombstones[ids[i]].touchSeq < h.eventTombstones[ids[j]].touchSeq
		})
		for _, id := range ids[:excess] {
			delete(h.eventTombstones, id)
		}
	}
}

func (h *Hub) storeQueryCache(key string, resp core.LiveEventsResponse, ttl time.Duration) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheSeq++
	h.queryCache[key] = &cachedQuery{
		resp:      cloneEventsResponse(resp),
		expiresAt: now.Add(ttl),
		touchSeq:  h.cacheSeq,
	}
	if len(h.queryCache) > h.maxCachedQueries {
		for k, v := range h.queryCache {
			if !now.Before(v.expiresAt) {
				delete(h.queryCache, k)
			}
		}
		for len(h.queryCache) > h.maxCachedQueries {
			var oldestKey string
			var oldestSeq int64
			first := true
			for k, v := range h.queryCache {
				if first || v.touchSeq < oldestSeq {
					oldestKey = k
					oldestSeq = v.touchSeq
					first = false
				}
			}
			if first {
				break
			}
			delete(h.queryCache, oldestKey)
		}
	}
}

func (h *Hub) storeDetailCache(key string, detail core.LiveEventDetail, ttl time.Duration) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheSeq++
	h.detailCache[key] = &cachedDetail{
		detail:    detail.Clone(),
		expiresAt: now.Add(ttl),
		touchSeq:  h.cacheSeq,
	}
	if len(h.detailCache) > h.maxCachedDetails {
		for k, v := range h.detailCache {
			if !now.Before(v.expiresAt) {
				delete(h.detailCache, k)
			}
		}
		for len(h.detailCache) > h.maxCachedDetails {
			var oldestKey string
			var oldestSeq int64
			first := true
			for k, v := range h.detailCache {
				if first || v.touchSeq < oldestSeq {
					oldestKey = k
					oldestSeq = v.touchSeq
					first = false
				}
			}
			if first {
				break
			}
			delete(h.detailCache, oldestKey)
		}
	}
}

func (h *Hub) storeEntitiesCache(key string, resp core.LiveEventEntitiesResponse, ttl time.Duration) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheSeq++
	h.entitiesCache[key] = &cachedEntities{
		resp:      cloneEntitiesResponse(resp),
		expiresAt: now.Add(ttl),
		touchSeq:  h.cacheSeq,
	}
	if len(h.entitiesCache) > defaultMaxCachedEntities {
		for k, v := range h.entitiesCache {
			if !now.Before(v.expiresAt) {
				delete(h.entitiesCache, k)
			}
		}
		for len(h.entitiesCache) > defaultMaxCachedEntities {
			var oldestKey string
			var oldestSeq int64
			first := true
			for k, v := range h.entitiesCache {
				if first || v.touchSeq < oldestSeq {
					oldestKey = k
					oldestSeq = v.touchSeq
					first = false
				}
			}
			if first {
				break
			}
			delete(h.entitiesCache, oldestKey)
		}
	}
}

func (h *Hub) storeStandingsCache(key string, resp core.LiveEventStandingsResponse, ttl time.Duration) {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cacheSeq++
	h.standingsCache[key] = &cachedStandings{
		resp:      cloneStandingsResponse(resp),
		expiresAt: now.Add(ttl),
		touchSeq:  h.cacheSeq,
	}
	if len(h.standingsCache) > defaultMaxCachedStanding {
		for k, v := range h.standingsCache {
			if !now.Before(v.expiresAt) {
				delete(h.standingsCache, k)
			}
		}
		for len(h.standingsCache) > defaultMaxCachedStanding {
			var oldestKey string
			var oldestSeq int64
			first := true
			for k, v := range h.standingsCache {
				if first || v.touchSeq < oldestSeq {
					oldestKey = k
					oldestSeq = v.touchSeq
					first = false
				}
			}
			if first {
				break
			}
			delete(h.standingsCache, oldestKey)
		}
	}
}

func cloneEventsResponse(r core.LiveEventsResponse) core.LiveEventsResponse {
	out := core.LiveEventsResponse{NextPollSec: r.NextPollSec}
	if r.Events != nil {
		out.Events = make([]core.LiveEvent, len(r.Events))
		for i, ev := range r.Events {
			out.Events[i] = ev.Clone()
		}
	}
	return out
}

func cloneEntitiesResponse(r core.LiveEventEntitiesResponse) core.LiveEventEntitiesResponse {
	out := core.LiveEventEntitiesResponse{}
	if r.Entities != nil {
		out.Entities = append([]core.LiveEventEntity(nil), r.Entities...)
	}
	return out
}

func cloneStandingsResponse(r core.LiveEventStandingsResponse) core.LiveEventStandingsResponse {
	out := core.LiveEventStandingsResponse{}
	if r.Tables != nil {
		out.Tables = make([]core.LiveEventTable, len(r.Tables))
		for i, t := range r.Tables {
			out.Tables[i] = t.Clone()
		}
	}
	return out
}

func splitNullPrefix2(key string) (string, string, bool) {
	a, rest, ok := strings.Cut(key, "\x00")
	if !ok {
		return "", "", false
	}
	b, _, ok := strings.Cut(rest, "\x00")
	if !ok {
		b = rest
	}
	return a, b, a != "" && b != ""
}
