package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/stats"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

func newSystemEventsTestApp(t *testing.T) (*App, *events.Recorder) {
	t.Helper()
	a := &App{opts: Options{Version: "0.3.17-test"}}
	a.initEventBus()
	a.ensureSystemEvents()
	rec := events.NewRecorder(a.EventBus(), "*")
	t.Cleanup(func() {
		rec.Close()
		for i := len(a.closers) - 1; i >= 0; i-- {
			a.closers[i]()
		}
	})
	return a, rec
}

func validateEnvelope(t *testing.T, reg *events.Registry, ev events.Envelope) {
	t.Helper()
	if err := reg.ValidatePayload(ev.Type, ev.Payload); err != nil {
		t.Fatalf("event %q failed schema validation: %v (payload=%s)", ev.Type, err, string(ev.Payload))
	}
}

func decodePayload[T any](t *testing.T, ev events.Envelope) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(ev.Payload, &out); err != nil {
		t.Fatalf("unmarshal %q payload: %v", ev.Type, err)
	}
	return out
}

func TestSystemEventsLibraryDomainAndBatcher(t *testing.T) {
	a, rec := newSystemEventsTestApp(t)
	reg := a.EventRegistry()

	// Sync lifecycle (catalog + pluginitems).
	a.onCatalogSyncEvent(catalog.SyncEvent{Phase: "started", Trigger: "manual"})
	a.onCatalogSyncEvent(catalog.SyncEvent{Phase: "completed", Trigger: "manual", Added: 5, Updated: 12, Removed: 1, DurationMS: 320})
	a.onCatalogSyncEvent(catalog.SyncEvent{Phase: "failed", Trigger: "scheduled", Error: "tmdb timeout"})
	a.onPluginItemsSyncEvent(pluginitems.SyncEvent{Phase: "started", PluginID: "comics", KindKey: "comics", LibraryID: 9, LibraryName: "Comics", Trigger: "manual"})
	a.onPluginItemsSyncEvent(pluginitems.SyncEvent{Phase: "completed", PluginID: "comics", KindKey: "comics", LibraryID: 9, LibraryName: "Comics", Added: 3, DurationMS: 45})
	a.onPluginItemsSyncEvent(pluginitems.SyncEvent{Phase: "failed", PluginID: "comics", KindKey: "comics", LibraryID: 9, LibraryName: "Comics", Error: "plugin error"})

	// Metadata, collections, watchlist, episode aired.
	a.onCatalogMetadataError(catalog.MetadataErrorEvent{Provider: "tmdb", TitleKey: core.MovieKey(603), MediaType: core.TypeMovie, Error: "status 502"})
	a.onCollectionRebuilt(collections.RebuildEvent{CollectionID: "trending", CollectionName: "Trending", SourceKind: "tmdb_trending", ItemCount: 25, Added: 4, Removed: 2, DurationMS: 180})
	a.onWatchlistAvailable(context.Background(), 7, core.MovieKey(603))
	a.onWatchlistUnavailable(context.Background(), 7, core.Title{Key: core.MovieKey(603), Type: core.TypeMovie, Title: "The Matrix"}, "no healthy releases")
	a.onWatchlistSynced(watchlist.SyncEvent{Provider: "plex", ViewerID: 7, Added: 2, Removed: 1, TotalItems: 14, DurationMS: 90})
	a.onFavoriteEpisodeAired([]notify.DetectedEpisode{{
		ShowKey: core.ShowKey(95396), ShowTitle: "Severance",
		EpisodeKey: core.EpisodeKey(95396, 2, 1), EpisodeTitle: "Hello, Ms. Cobel",
		Season: 2, Episode: 1, AirDate: "2025-01-17", FollowerCount: 3,
	}})

	// Title added + debounced batching grouped by show+season.
	a.onCatalogTitlesAdded([]core.Title{
		{Key: core.EpisodeKey(95396, 2, 1), Type: core.TypeEpisode, Title: "Ep 1", ShowKey: core.ShowKey(95396), ShowTitle: "Severance", Season: 2, Episode: 1, PosterPath: "/p.jpg"},
		{Key: core.EpisodeKey(95396, 2, 2), Type: core.TypeEpisode, Title: "Ep 2", ShowKey: core.ShowKey(95396), ShowTitle: "Severance", Season: 2, Episode: 2},
		{Key: core.MovieKey(603), Type: core.TypeMovie, Title: "The Matrix", Year: 1999, Genres: []string{"Action"}},
	})
	a.systemEvents.batcher.Flush()

	wantTypes := []string{
		events.EventLibrarySyncStarted,
		events.EventLibrarySyncCompleted,
		events.EventLibrarySyncFailed,
		events.EventLibraryMetadataFailed,
		events.EventLibraryCollectionRebuilt,
		events.EventLibraryTitleAvailable,
		events.EventLibraryTitleUnavailable,
		events.EventLibraryWatchlistSynced,
		events.EventLibraryEpisodeAired,
		events.EventLibraryTitleAdded,
		events.EventLibraryBatchAdded,
	}
	for _, typ := range wantTypes {
		ev := rec.WaitFor(t, typ, 2*time.Second)
		validateEnvelope(t, reg, ev)
	}

	deadline := time.Now().Add(2 * time.Second)
	var batches []events.Envelope
	for time.Now().Before(deadline) {
		batches = rec.OfType(events.EventLibraryBatchAdded)
		if len(batches) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(batches) < 2 {
		t.Fatalf("expected at least 2 library.batch.added groups (episode batch + movie batch), got %d", len(batches))
	}
	var foundEpisodeBatch bool
	for _, ev := range batches {
		validateEnvelope(t, reg, ev)
		p := decodePayload[events.LibraryBatchAddedPayload](t, ev)
		if p.MediaType == "episode" && p.ShowTitle == "Severance" && p.Season == 2 {
			foundEpisodeBatch = true
			if p.EpisodeCount != 2 || p.ItemCount != 2 {
				t.Fatalf("unexpected episode batch counts: %+v", p)
			}
		}
	}
	if !foundEpisodeBatch {
		t.Fatal("missing grouped Severance S02 episode batch")
	}
}

func TestSystemEventsViewerAndAuthDomains(t *testing.T) {
	a, rec := newSystemEventsTestApp(t)
	reg := a.EventRegistry()

	// Viewer lifecycle + access + safety + watch/rating.
	a.onAuthViewerEvent(auth.ViewerEvent{Kind: "joined", ViewerID: 7, Username: "alex", Role: "friend", Source: "plex_friends"})
	a.onAuthViewerEvent(auth.ViewerEvent{Kind: "access_changed", ViewerID: 7, Username: "alex", StreamLimit: 2, MaxContentRating: "PG-13", AllowedLibraries: []core.LibraryID{1, 2}, Actor: "owner"})
	a.onAuthViewerEvent(auth.ViewerEvent{Kind: "deactivated", ViewerID: 7, Username: "alex", Reason: "paused_by_owner", Actor: "owner"})
	a.onAuthViewerEvent(auth.ViewerEvent{Kind: "merged", FromViewerID: 9, IntoViewerID: 7, Actor: "owner"})
	a.onAuthViewerEvent(auth.ViewerEvent{Kind: "grant_revoked", ViewerID: 7, Reason: "sign_out", Actor: "owner"})
	a.onSafetyPolicyUpdated(7, safety.Policy{RemoteDisabled: true, MaxBitrateKbps: 8000, BlockedGenres: []string{"Horror"}, Bedtime: &core.Bedtime{Time: "22:00", WakeTime: "07:00"}})
	a.onStateEvent(state.Event{Kind: state.EventScrobble, UserID: 7, Key: core.MovieKey(603), Completed: true})
	a.onStateEvent(state.Event{Kind: state.EventRated, UserID: 7, Key: core.MovieKey(603), Rating: 9.0})
	// Imported watch history must NOT fire viewer.watch.scrobbled.
	a.onStateEvent(state.Event{Kind: state.EventScrobble, UserID: 7, Key: core.MovieKey(604), Imported: true})

	// Auth login, device registration (dedup), new IP detection, token expired, cert renewed.
	a.emitLoginSucceeded(7, "alex", "password", "Vela", "tvOS", "192.168.1.10")
	a.emitLoginFailed("alex", "password", "invalid_credentials", "Vela", "tvOS", "198.51.100.5")
	a.onStatsDeviceRegistered(stats.Device{ID: 14, ClientIdentifier: "appletv-1", Name: "Apple TV", Platform: "tvOS"})
	a.onStatsDeviceRegistered(stats.Device{ID: 14, ClientIdentifier: "appletv-1", Name: "Apple TV", Platform: "tvOS"}) // deduped
	// Second IP for viewer 7 triggers auth.session.new_ip (first IP 192.168.1.10 established baseline).
	a.onPlaybackEventForAuth(playback.Event{
		Kind: "started",
		Session: &core.PlaySession{
			ID: "sess_1", UserID: 7, Username: "alex",
			Client: core.ClientCaps{ClientIP: "203.0.113.42", Product: "Plex for iOS", Platform: "iOS"},
		},
	})
	// Repeating the same IP must not trigger a second new_ip event.
	a.onPlaybackEventForAuth(playback.Event{
		Kind: "decision",
		Session: &core.PlaySession{
			ID: "sess_2", UserID: 7, Username: "alex",
			Client: core.ClientCaps{ClientIP: "203.0.113.42", Product: "Plex for iOS", Platform: "iOS"},
		},
	})
	a.emitTokenExpired("trakt", "watch_sync", "trakt-alex", 7, "refresh token revoked")
	a.emitCertificateRenewed([]string{"*.1-2-3-4.abc.plex.direct"}, "R11", time.Now().Add(60*24*time.Hour))

	wantTypes := []string{
		events.EventViewerAccountJoined,
		events.EventViewerAccessChanged,
		events.EventViewerAccountDeactivated,
		events.EventViewerAccountMerged,
		events.EventViewerSafetyUpdated,
		events.EventViewerWatchScrobbled,
		events.EventViewerRatingSubmitted,
		events.EventAuthGrantRevoked,
		events.EventAuthLoginSucceeded,
		events.EventAuthLoginFailed,
		events.EventAuthDeviceRegistered,
		events.EventAuthSessionNewIP,
		events.EventAuthTokenExpired,
		events.EventAuthCertificateRenewed,
	}
	for _, typ := range wantTypes {
		ev := rec.WaitFor(t, typ, 2*time.Second)
		validateEnvelope(t, reg, ev)
	}

	newIPEvents := rec.OfType(events.EventAuthSessionNewIP)
	if len(newIPEvents) != 1 {
		t.Fatalf("expected exactly 1 auth.session.new_ip event, got %d", len(newIPEvents))
	}
	ipPayload := decodePayload[events.AuthSessionNewIPPayload](t, newIPEvents[0])
	if ipPayload.RemoteIP != "203.0.113.42" || ipPayload.Location != "wan" {
		t.Fatalf("unexpected auth.session.new_ip payload: %+v", ipPayload)
	}
	if len(ipPayload.PreviousIPs) != 1 || ipPayload.PreviousIPs[0] != "192.168.1.10" {
		t.Fatalf("expected previousIPs=[192.168.1.10], got %v", ipPayload.PreviousIPs)
	}

	if got := len(rec.OfType(events.EventAuthDeviceRegistered)); got != 1 {
		t.Fatalf("expected deduped device registration count 1, got %d", got)
	}
	if got := len(rec.OfType(events.EventViewerWatchScrobbled)); got != 1 {
		t.Fatalf("expected imported watch history to be skipped (1 scrobble), got %d", got)
	}
}

type fakeCluster struct {
	mu      sync.Mutex
	self    cluster.Node
	nodes   []cluster.Node
	all     []cluster.Node
	epoch   int64
	leader  bool
	onLead  []func(bool)
	subs    map[string][]func([]byte)
	onNodes func()
}

func (f *fakeCluster) Mode() string { return cluster.ModePostgres }
func (f *fakeCluster) Self() cluster.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.self
}
func (f *fakeCluster) IsLeader() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leader
}
func (f *fakeCluster) Epoch() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch
}
func (f *fakeCluster) OnLeaderChange(fn func(bool)) func() {
	f.mu.Lock()
	f.onLead = append(f.onLead, fn)
	f.mu.Unlock()
	return func() {}
}
func (f *fakeCluster) RunAsLeader(context.Context, string, func(context.Context)) {}
func (f *fakeCluster) Publish(context.Context, string, []byte) error              { return nil }
func (f *fakeCluster) Subscribe(topic string, fn func([]byte)) func() {
	f.mu.Lock()
	if f.subs == nil {
		f.subs = make(map[string][]func([]byte))
	}
	f.subs[topic] = append(f.subs[topic], fn)
	f.mu.Unlock()
	return func() {}
}
func (f *fakeCluster) emit(topic string) {
	f.mu.Lock()
	fns := append([]func([]byte){}, f.subs[topic]...)
	f.mu.Unlock()
	for _, fn := range fns {
		fn(nil)
	}
}
func (f *fakeCluster) Nodes(context.Context) ([]cluster.Node, error) {
	f.mu.Lock()
	hook := f.onNodes
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cluster.Node, len(f.nodes))
	copy(out, f.nodes)
	return out, nil
}
func (f *fakeCluster) AllNodes(context.Context) ([]cluster.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cluster.Node, len(f.all))
	copy(out, f.all)
	return out, nil
}
func (f *fakeCluster) Start(context.Context) error { return nil }
func (f *fakeCluster) Close() error                { return nil }

func TestSystemEventsClusterSystemAndFlowsDomains(t *testing.T) {
	a, rec := newSystemEventsTestApp(t)
	reg := a.EventRegistry()

	fc := &fakeCluster{
		self:  cluster.Node{ID: "node-a", Name: "media-1", Version: "0.3.17", Leader: true, Live: true},
		nodes: []cluster.Node{{ID: "node-a", Name: "media-1", Version: "0.3.17", Leader: true, Live: true}},
		all:   []cluster.Node{{ID: "node-a", Name: "media-1", Version: "0.3.17", Leader: true, Live: true}},
		epoch: 1,
	}
	a.cluster = fc
	a.dbAvail = store.NewAvailability(nil)
	a.wireSystemEvents()

	// Seed baseline cluster membership (no spurious join for initial self).
	a.seedClusterMembership(context.Background())

	// Node B joins, then disappears.
	nodeB := cluster.Node{ID: "node-b", Name: "media-2", Version: "0.3.17", Live: true, LastSeen: time.Now().Add(-35 * time.Second)}
	fc.mu.Lock()
	fc.nodes = []cluster.Node{fc.self, nodeB}
	fc.all = []cluster.Node{fc.self, nodeB}
	fc.mu.Unlock()
	a.observeClusterMembership(context.Background())

	fc.mu.Lock()
	nodeB.Live = false
	fc.nodes = []cluster.Node{fc.self}
	fc.all = []cluster.Node{fc.self, nodeB}
	fc.mu.Unlock()
	a.observeClusterMembership(context.Background())

	// Leader election + draining (edge-triggered).
	fc.mu.Lock()
	fc.epoch = 2
	fc.leader = true
	fc.mu.Unlock()
	a.onClusterLeaderChange(true)
	a.emitClusterNodeDraining("rolling_update")
	a.emitClusterNodeDraining("rolling_update") // suppressed duplicate

	// Database outage + recovery via store.Availability.
	a.dbAvail.Fail(errors.New("connection refused"))
	a.dbAvail.OK()

	// System lifecycle, reachability, updates, backups, disk low (hysteresis), digest.
	a.emitServerStarted()
	a.emitServerStopping("shutdown")
	a.emitServerCrashed("watchdog timeout", "watchdog")
	a.recordReachability("remote_access", "reachable", "https://example.plex.direct", 200, 15, "") // seeds baseline
	a.recordReachability("remote_access", "unreachable", "https://example.plex.direct", 502, 0, "timeout")
	a.emitUpdateAvailable("0.3.16", "0.3.17", "stable", false)
	a.emitUpdateInstalled("0.3.16", "0.3.17", "stable", true)
	a.emitUpdateRolledBack("0.3.17", "0.3.16", "health check failed")
	a.onBackupEvent(backup.BackupEvent{Kind: "scheduled", SizeBytes: 1024, DurationMS: 50, Destination: "/var/backups/a.tar.gz", Encrypted: true})
	a.onBackupEvent(backup.BackupEvent{Kind: "scheduled", Err: errors.New("disk full"), ConsecutiveFailures: 2})
	a.evaluateDiskVolume("data", "/var/lib/ether", 100<<30, 100<<20) // low
	a.evaluateDiskVolume("data", "/var/lib/ether", 100<<30, 90<<20)  // still low -> suppressed
	a.emitDigestScheduled("weekly", time.Now().Add(-7*24*time.Hour), time.Now(), 18, 3)

	// Trigger real flows.bus.dropped via an unread 1-slot subscriber on a.EventBus().
	slow := a.EventBus().Subscribe(events.SubscriptionOptions{
		Name:     "test-slow-sub",
		Buffer:   1,
		Patterns: []string{"system.server.started"},
	})
	defer slow.Unsubscribe()
	a.emitServerStarted()
	a.emitServerStarted()
	a.emitServerStarted()

	wantTypes := []string{
		events.EventClusterNodeJoined,
		events.EventClusterNodeLost,
		events.EventClusterLeaderElected,
		events.EventClusterNodeDraining,
		events.EventClusterDatabaseUnavailable,
		events.EventClusterDatabaseRecovered,
		events.EventSystemServerStarted,
		events.EventSystemServerStopping,
		events.EventSystemServerCrashed,
		events.EventSystemHealthChanged,
		events.EventSystemReachabilityChanged,
		events.EventSystemUpdateAvailable,
		events.EventSystemUpdateInstalled,
		events.EventSystemUpdateRolledBack,
		events.EventSystemBackupCompleted,
		events.EventSystemBackupFailed,
		events.EventSystemDiskLow,
		events.EventFlowsDigestScheduled,
		events.EventFlowsBusDropped,
	}
	for _, typ := range wantTypes {
		ev := rec.WaitFor(t, typ, 2*time.Second)
		validateEnvelope(t, reg, ev)
	}

	if got := len(rec.OfType(events.EventClusterNodeDraining)); got != 1 {
		t.Fatalf("expected edge-triggered draining count 1, got %d", got)
	}
	if got := len(rec.OfType(events.EventSystemDiskLow)); got != 1 {
		t.Fatalf("expected hysteresis disk.low count 1, got %d", got)
	}
}

func TestObserveClusterMembershipCoalescesBursts(t *testing.T) {
	a, rec := newSystemEventsTestApp(t)
	fc := &fakeCluster{
		self:  cluster.Node{ID: "node-a", Name: "media-1", Version: "0.3.17", Leader: true, Live: true},
		nodes: []cluster.Node{{ID: "node-a", Name: "media-1", Version: "0.3.17", Leader: true, Live: true}},
		all:   []cluster.Node{{ID: "node-a", Name: "media-1", Version: "0.3.17", Leader: true, Live: true}},
		epoch: 1,
	}
	a.cluster = fc
	a.wireSystemEvents()
	a.seedClusterMembership(context.Background())

	nodeB := cluster.Node{ID: "node-b", Name: "media-2", Version: "0.3.17", Live: true}
	var inFlight, maxInFlight, totalCalls int32
	entered := make(chan struct{})
	var enteredOnce sync.Once
	release := make(chan struct{})

	fc.mu.Lock()
	fc.nodes = []cluster.Node{fc.self, nodeB}
	fc.all = []cluster.Node{fc.self, nodeB}
	fc.onNodes = func() {
		cur := atomic.AddInt32(&inFlight, 1)
		atomic.AddInt32(&totalCalls, 1)
		for {
			prev := atomic.LoadInt32(&maxInFlight)
			if cur <= prev || atomic.CompareAndSwapInt32(&maxInFlight, prev, cur) {
				break
			}
		}
		enteredOnce.Do(func() { close(entered) })
		<-release
		atomic.AddInt32(&inFlight, -1)
	}
	fc.mu.Unlock()

	for i := 0; i < 20; i++ {
		fc.emit(cluster.TopicMembers)
	}
	<-entered
	time.Sleep(40 * time.Millisecond)
	close(release)

	rec.WaitFor(t, events.EventClusterNodeJoined, 2*time.Second)
	time.Sleep(30 * time.Millisecond)

	if got := atomic.LoadInt32(&maxInFlight); got > 1 {
		t.Fatalf("TopicMembers burst ran %d concurrent Nodes() queries, want 1", got)
	}
	if got := atomic.LoadInt32(&totalCalls); got > 2 {
		t.Fatalf("TopicMembers burst ran %d Nodes() queries for 20 messages, want <= 2", got)
	}
	if got := len(rec.OfType(events.EventClusterNodeJoined)); got != 1 {
		t.Fatalf("EventClusterNodeJoined emitted %d times, want 1", got)
	}
}
