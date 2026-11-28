package app

import (
	"context"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/datafs"
	"github.com/braedonsaunders/ether/internal/events"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/pluginitems"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/stats"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

const (
	defaultLibraryBatchWindow = 15 * time.Second
	maxLibraryBatchGroups     = 64
	maxLibraryBatchSample     = 10
	maxTrackedViewerIPUsers   = 2048
	maxTrackedIPsPerViewer    = 32
	maxTrackedPreviousIPs     = 5
	maxTrackedSeenDevices     = 4096
	diskLowPercentThreshold   = 95.0
	diskLowRecoverPercent     = 90.0
	diskLowMinFreeBytes       = 512 * 1024 * 1024 // 512 MiB
)

// systemEventHooks owns stateful instrumentation trackers for the library,
// viewer, auth, cluster, system, and flows domains.
type systemEventHooks struct {
	bootAt time.Time

	batcher *libraryBatcher
	health  *componentHealthTracker
	ips     *viewerIPTracker

	devicesMu   sync.Mutex
	seenDevices map[string]struct{}

	reachMu     sync.Mutex
	reachStates map[string]string

	diskMu   sync.Mutex
	diskLow  map[string]bool
	diskFunc func(string) (int64, int64, error)

	dbMu          sync.Mutex
	dbDown        bool
	dbDownSince   time.Time
	dbLastDialect string

	clusterObserveMu sync.Mutex
	clusterMu        sync.Mutex
	clusterSeeded    bool
	clusterMembers   map[string]cluster.Node
	lastLeaderID     string
	lastLeaderEp     int64
	clusterKick      atomic.Bool
	clusterRunning   atomic.Bool
	clusterStop      chan struct{}
	clusterStopOnce  sync.Once

	drainEmitted atomic.Bool
}

func (a *App) ensureSystemEvents() *systemEventHooks {
	if a == nil {
		return nil
	}
	if a.systemEvents != nil {
		return a.systemEvents
	}
	h := &systemEventHooks{
		bootAt:         time.Now(),
		health:         newComponentHealthTracker(a.emitSystemHealthChanged),
		ips:            newViewerIPTracker(),
		seenDevices:    make(map[string]struct{}),
		reachStates:    make(map[string]string),
		diskLow:        make(map[string]bool),
		diskFunc:       datafs.Usage,
		clusterMembers: make(map[string]cluster.Node),
		clusterStop:    make(chan struct{}),
	}
	h.batcher = newLibraryBatcher(defaultLibraryBatchWindow, func(p events.LibraryBatchAddedPayload) {
		a.publishEvent(events.EventLibraryBatchAdded, p, events.WithSource("catalog"))
	})
	a.systemEvents = h
	a.closers = append(a.closers, h.batcher.Close, func() {
		h.clusterStopOnce.Do(func() { close(h.clusterStop) })
	})
	return h
}

// wireSystemEvents connects process-wide callbacks across library, viewer,
// auth, backup, cluster, and system components onto a.eventBus.
func (a *App) wireSystemEvents() {
	if a == nil || a.eventBus == nil {
		return
	}
	a.ensureSystemEvents()

	if a.users != nil {
		a.users.SetOnViewerEvent(a.onAuthViewerEvent)
	}
	if a.safety != nil {
		a.safety.SetOnPut(a.onSafetyPolicyUpdated)
	}
	if a.backups != nil {
		a.backups.SetOnEvent(a.onBackupEvent)
	}
	if a.stats != nil && a.stats.Dashboard() != nil {
		a.stats.Dashboard().SetOnDeviceRegistered(a.onStatsDeviceRegistered)
	}
	if a.pluginsRT != nil && a.pluginsRT.pluginitems != nil {
		a.pluginsRT.pluginitems.SetOnSyncEvent(a.onPluginItemsSyncEvent)
	}
	if a.state != nil {
		a.closers = append(a.closers, a.state.Subscribe(a.onStateEvent))
	}
	if a.playback != nil {
		a.closers = append(a.closers, a.playback.Subscribe(a.onPlaybackEventForAuth))
	}
	if a.dbAvail != nil {
		a.closers = append(a.closers, a.dbAvail.Subscribe(a.onDatabaseAvailability))
	}
	if a.cluster != nil {
		a.closers = append(a.closers, a.cluster.OnLeaderChange(a.onClusterLeaderChange))
		a.closers = append(a.closers, a.cluster.Subscribe(cluster.TopicMembers, func([]byte) {
			a.triggerClusterMembershipObserve()
		}))
	}
}

// runSystemEvents runs periodic low-cost observers for cluster membership
// liveness, reachability, and disk space.
func (a *App) runSystemEvents(ctx context.Context) {
	if a == nil || a.eventBus == nil {
		return
	}
	a.ensureSystemEvents()
	a.seedClusterMembership(ctx)
	a.observeReachabilityChange()
	a.checkDiskSpace()

	diskTick := time.NewTicker(time.Minute)
	defer diskTick.Stop()
	clusterTick := time.NewTicker(15 * time.Second)
	defer clusterTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-clusterTick.C:
			a.observeClusterMembership(ctx)
			a.observeReachabilityChange()
		case <-diskTick.C:
			a.checkDiskSpace()
		}
	}
}

// --- Shared lookup helpers ---

func (a *App) lookupUsername(ctx context.Context, viewerID int64) string {
	if a == nil || a.users == nil || viewerID <= 0 {
		return ""
	}
	u, err := a.users.User(ctx, viewerID)
	if err != nil || u == nil {
		return ""
	}
	if u.Username != "" {
		return u.Username
	}
	return u.Title
}

func (a *App) lookupTitleInfo(ctx context.Context, key core.RatingKey) (name, mediaType string, year int) {
	mediaType = ratingKeyMediaType(key)
	if a == nil || a.catalog == nil || key == 0 {
		return key.String(), mediaType, 0
	}
	t, err := a.catalog.Get(ctx, key)
	if err != nil || t == nil {
		return key.String(), mediaType, 0
	}
	if t.Type != "" {
		mediaType = canonicalMediaType(t.Type)
	}
	name = t.Title
	if name == "" {
		name = key.String()
	}
	return name, mediaType, t.Year
}

func canonicalMediaType(mt core.MediaType) string {
	switch mt {
	case core.TypeMovie:
		return "movie"
	case core.TypeShow:
		return "show"
	case core.TypeSeason:
		return "season"
	case core.TypeEpisode:
		return "episode"
	default:
		return "other"
	}
}

func ratingKeyMediaType(key core.RatingKey) string {
	switch key.Kind() {
	case core.KindMovie:
		return "movie"
	case core.KindShow:
		return "show"
	case core.KindSeason:
		return "season"
	case core.KindEpisode:
		return "episode"
	default:
		return "other"
	}
}

func catalogLibraryInfo(t core.Title) (id, name string) {
	if t.LibraryID != 0 {
		id = strconv.FormatInt(int64(t.LibraryID), 10)
	}
	switch t.Type {
	case core.TypeMovie:
		if id == "" {
			id = "1"
		}
		return id, "Movies"
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		if id == "" {
			id = "2"
		}
		return id, "TV Shows"
	default:
		if id == "" {
			id = "catalog"
		}
		return id, "Catalog"
	}
}

func clusterNodeRoles(n cluster.Node) []string {
	var roles []string
	if n.Leader {
		roles = append(roles, "leader")
	}
	if n.Draining {
		roles = append(roles, "draining")
	}
	if len(roles) == 0 {
		roles = append(roles, "member")
	}
	return roles
}

// --- Library domain emitters ---

func (a *App) onCatalogSyncEvent(ev catalog.SyncEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	libID := "catalog"
	libName := "Catalog"
	sourceKind := "tmdb"
	switch ev.Phase {
	case "started":
		trigger := ev.Trigger
		if trigger == "" {
			trigger = "scheduled"
		}
		a.publishEvent(events.EventLibrarySyncStarted, events.LibrarySyncStartedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			SourceKind:  sourceKind,
			Trigger:     trigger,
		}, events.WithSource("catalog"))
	case "completed":
		a.publishEvent(events.EventLibrarySyncCompleted, events.LibrarySyncCompletedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			SourceKind:  sourceKind,
			Added:       ev.Added,
			Updated:     ev.Updated,
			Removed:     ev.Removed,
			DurationMS:  ev.DurationMS,
		}, events.WithSource("catalog"))
	case "failed":
		a.publishEvent(events.EventLibrarySyncFailed, events.LibrarySyncFailedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			SourceKind:  sourceKind,
			Error:       ev.Error,
		}, events.WithSource("catalog"))
	}
}

func (a *App) onPluginItemsSyncEvent(ev pluginitems.SyncEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	libID := ""
	if ev.LibraryID != 0 {
		libID = strconv.FormatInt(ev.LibraryID, 10)
	}
	if libID == "" {
		libID = ev.KindKey
	}
	if libID == "" {
		libID = ev.PluginID
	}
	if libID == "" {
		libID = "plugin"
	}
	libName := ev.LibraryName
	if libName == "" {
		libName = libID
	}
	sourceKind := "plugin"
	if ev.PluginID != "" {
		sourceKind = "plugin:" + ev.PluginID
	}
	switch ev.Phase {
	case "started":
		trigger := ev.Trigger
		if trigger == "" {
			trigger = "scheduled"
		}
		a.publishEvent(events.EventLibrarySyncStarted, events.LibrarySyncStartedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			SourceKind:  sourceKind,
			Trigger:     trigger,
		}, events.WithSource("pluginitems"))
	case "completed":
		a.publishEvent(events.EventLibrarySyncCompleted, events.LibrarySyncCompletedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			SourceKind:  sourceKind,
			Added:       ev.Added,
			Updated:     ev.Updated,
			Removed:     ev.Removed,
			DurationMS:  ev.DurationMS,
		}, events.WithSource("pluginitems"))
	case "failed":
		a.publishEvent(events.EventLibrarySyncFailed, events.LibrarySyncFailedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			SourceKind:  sourceKind,
			Error:       ev.Error,
		}, events.WithSource("pluginitems"))
	}
}

func (a *App) onCatalogTitlesAdded(items []core.Title) {
	if a == nil || a.eventBus == nil || len(items) == 0 {
		return
	}
	h := a.ensureSystemEvents()
	for _, item := range items {
		libID, libName := catalogLibraryInfo(item)
		mt := canonicalMediaType(item.Type)
		payload := events.LibraryTitleAddedPayload{
			LibraryID:   libID,
			LibraryName: libName,
			TitleKey:    item.Key.String(),
			TitleName:   item.Title,
			ShowTitle:   item.ShowTitle,
			Season:      item.Season,
			Episode:     item.Episode,
			MediaType:   mt,
			Year:        item.Year,
			Genres:      slices.Clone(item.Genres),
			PosterURL:   item.PosterPath,
		}
		a.publishEvent(events.EventLibraryTitleAdded, payload, events.WithSource("catalog"))
		if h != nil && h.batcher != nil {
			showKey := ""
			if item.ShowKey != 0 {
				showKey = item.ShowKey.String()
			}
			h.batcher.Add(libraryBatchItem{
				LibraryID:   libID,
				LibraryName: libName,
				MediaType:   mt,
				ShowKey:     showKey,
				ShowTitle:   item.ShowTitle,
				Season:      item.Season,
				TitleName:   item.Title,
				PosterURL:   item.PosterPath,
			})
		}
	}
}

func (a *App) onCatalogMetadataError(ev catalog.MetadataErrorEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	provider := ev.Provider
	if provider == "" {
		provider = "tmdb"
	}
	mt := ratingKeyMediaType(ev.TitleKey)
	if ev.MediaType != "" {
		mt = canonicalMediaType(ev.MediaType)
	}
	a.publishEvent(events.EventLibraryMetadataFailed, events.LibraryMetadataFailedPayload{
		Provider:  provider,
		TitleKey:  ev.TitleKey.String(),
		MediaType: mt,
		Error:     ev.Error,
	}, events.WithSource("catalog"))
}

func (a *App) onCollectionRebuilt(ev collections.RebuildEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	sourceKind := ev.SourceKind
	if sourceKind == "" {
		sourceKind = "manual"
	}
	a.publishEvent(events.EventLibraryCollectionRebuilt, events.LibraryCollectionRebuiltPayload{
		CollectionID:   ev.CollectionID,
		CollectionName: ev.CollectionName,
		SourceKind:     sourceKind,
		ItemCount:      ev.ItemCount,
		Added:          ev.Added,
		Removed:        ev.Removed,
		DurationMS:     ev.DurationMS,
	}, events.WithSource("collections"))
}

func (a *App) onWatchlistAvailable(ctx context.Context, userID int64, key core.RatingKey) {
	if a == nil || a.eventBus == nil {
		return
	}
	name, mt, year := a.lookupTitleInfo(ctx, key)
	watchedBy := 1
	if a.db != nil && key != 0 {
		var n int
		if err := a.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT user_id) FROM watchlist_items WHERE rating_key=?`, int64(key)).Scan(&n); err == nil && n > 0 {
			watchedBy = n
		}
	}
	a.publishEvent(events.EventLibraryTitleAvailable, events.LibraryTitleAvailablePayload{
		TitleKey:       key.String(),
		TitleName:      name,
		MediaType:      mt,
		Year:           year,
		Source:         "watchlist",
		WatchedByCount: watchedBy,
	}, events.WithSource("watchlist"), events.WithCorrelation(events.Correlation{ViewerID: userID, TitleID: key.String()}))
}

func (a *App) onWatchlistUnavailable(ctx context.Context, userID int64, title core.Title, reason string) {
	if a == nil || a.eventBus == nil {
		return
	}
	name := title.Title
	mt := canonicalMediaType(title.Type)
	if name == "" || title.Type == "" {
		n, m, _ := a.lookupTitleInfo(ctx, title.Key)
		if name == "" {
			name = n
		}
		if title.Type == "" {
			mt = m
		}
	}
	if reason == "" {
		reason = "no healthy releases available"
	}
	a.publishEvent(events.EventLibraryTitleUnavailable, events.LibraryTitleUnavailablePayload{
		TitleKey:  title.Key.String(),
		TitleName: name,
		MediaType: mt,
		Reason:    reason,
	}, events.WithSource("watchlist"), events.WithCorrelation(events.Correlation{ViewerID: userID, TitleID: title.Key.String()}))
}

func (a *App) onWatchlistSynced(ev watchlist.SyncEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	provider := ev.Provider
	if provider == "" {
		provider = "watchlist"
	}
	a.publishEvent(events.EventLibraryWatchlistSynced, events.LibraryWatchlistSyncedPayload{
		Provider:   provider,
		ViewerID:   ev.ViewerID,
		Added:      ev.Added,
		Removed:    ev.Removed,
		TotalItems: ev.TotalItems,
		DurationMS: ev.DurationMS,
	}, events.WithSource("watchlist"))
}

func (a *App) onFavoriteEpisodeAired(eps []notify.DetectedEpisode) {
	if a == nil || a.eventBus == nil || len(eps) == 0 {
		return
	}
	for _, ep := range eps {
		showTitle := ep.ShowTitle
		if showTitle == "" && ep.ShowKey != 0 {
			showTitle, _, _ = a.lookupTitleInfo(context.Background(), ep.ShowKey)
		}
		a.publishEvent(events.EventLibraryEpisodeAired, events.LibraryEpisodeAiredPayload{
			ShowKey:       ep.ShowKey.String(),
			ShowTitle:     showTitle,
			EpisodeKey:    ep.EpisodeKey.String(),
			EpisodeTitle:  ep.EpisodeTitle,
			Season:        ep.Season,
			Episode:       ep.Episode,
			AirDate:       ep.AirDate,
			FollowerCount: ep.FollowerCount,
		}, events.WithSource("favorite-episodes"))
	}
}

// --- Viewer & Auth domain emitters ---

func (a *App) onAuthViewerEvent(ev auth.ViewerEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	switch ev.Kind {
	case "joined":
		role := ev.Role
		if role == "" {
			role = "friend"
		}
		source := ev.Source
		if source == "" {
			source = "plex_friends"
		}
		a.publishEvent(events.EventViewerAccountJoined, events.ViewerAccountJoinedPayload{
			ViewerID: ev.ViewerID,
			Username: ev.Username,
			Role:     role,
			Source:   source,
		}, events.WithSource("auth"))
	case "deactivated":
		a.publishEvent(events.EventViewerAccountDeactivated, events.ViewerAccountDeactivatedPayload{
			ViewerID: ev.ViewerID,
			Username: ev.Username,
			Reason:   ev.Reason,
			Actor:    ev.Actor,
		}, events.WithSource("auth"))
	case "merged":
		a.publishEvent(events.EventViewerAccountMerged, events.ViewerAccountMergedPayload{
			FromViewerID: ev.FromViewerID,
			IntoViewerID: ev.IntoViewerID,
			Actor:        ev.Actor,
		}, events.WithSource("auth"))
	case "access_changed":
		var libs []string
		if len(ev.AllowedLibraries) > 0 {
			libs = make([]string, 0, len(ev.AllowedLibraries))
			for _, id := range ev.AllowedLibraries {
				libs = append(libs, strconv.FormatInt(int64(id), 10))
			}
		}
		a.publishEvent(events.EventViewerAccessChanged, events.ViewerAccessChangedPayload{
			ViewerID:         ev.ViewerID,
			Username:         ev.Username,
			StreamLimit:      ev.StreamLimit,
			MaxContentRating: ev.MaxContentRating,
			AllowedLibraries: libs,
			Actor:            ev.Actor,
		}, events.WithSource("auth"))
	case "grant_revoked":
		a.publishEvent(events.EventAuthGrantRevoked, events.AuthGrantRevokedPayload{
			ViewerID:  ev.ViewerID,
			GrantKind: "sessions",
			Reason:    ev.Reason,
			Actor:     ev.Actor,
		}, events.WithSource("auth"))
	}
}

func (a *App) onSafetyPolicyUpdated(userID int64, p safety.Policy) {
	if a == nil || a.eventBus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	username := a.lookupUsername(ctx, userID)
	a.publishEvent(events.EventViewerSafetyUpdated, events.ViewerSafetyUpdatedPayload{
		ViewerID:           userID,
		Username:           username,
		RemoteDisabled:     p.RemoteDisabled,
		MaxBitrateKbps:     p.MaxBitrateKbps,
		BedtimeEnabled:     p.Bedtime != nil && strings.TrimSpace(p.Bedtime.Time) != "",
		BlockedGenresCount: len(p.BlockedGenres),
		Actor:              "owner",
	}, events.WithSource("safety"))
}

func (a *App) onStateEvent(ev state.Event) {
	if a == nil || a.eventBus == nil || ev.UserID <= 0 || ev.Key == 0 {
		return
	}
	switch ev.Kind {
	case state.EventCompleted, state.EventScrobble, state.EventUnscrobble, state.EventProgress:
		a.publishViewerWatchEvent(ev)
	case state.EventRated:
		if ev.Imported || ev.Account {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		username := a.lookupUsername(ctx, ev.UserID)
		titleName, _, _ := a.lookupTitleInfo(ctx, ev.Key)
		a.publishEvent(events.EventViewerRatingSubmitted, events.ViewerRatingSubmittedPayload{
			ViewerID:  ev.UserID,
			Username:  username,
			TitleKey:  ev.Key.String(),
			TitleName: titleName,
			Rating:    ev.Rating,
		}, events.WithSource("state"))
	}
}

// wireIdentityLocal attaches viewer-created instrumentation to a Local identity store.
func (a *App) wireIdentityLocal(local *identity.Local) {
	if a == nil || local == nil {
		return
	}
	local.SetOnViewerCreated(func(viewerID int64, username, role, source string) {
		if a.eventBus == nil {
			return
		}
		if role == "" {
			role = "local"
		}
		if source == "" {
			source = "local"
		}
		a.publishEvent(events.EventViewerAccountJoined, events.ViewerAccountJoinedPayload{
			ViewerID: viewerID,
			Username: username,
			Role:     role,
			Source:   source,
		}, events.WithSource("identity"))
	})
}

// wireIdentityGrants attaches grant revocation and first-seen device instrumentation.
func (a *App) wireIdentityGrants(grants *identity.Grants) {
	if a == nil || grants == nil {
		return
	}
	grants.SetOnRevoked(func(viewerID int64, grantKind, reason string) {
		if a.eventBus == nil {
			return
		}
		if grantKind == "" {
			grantKind = "grant"
		}
		a.publishEvent(events.EventAuthGrantRevoked, events.AuthGrantRevokedPayload{
			ViewerID:  viewerID,
			GrantKind: grantKind,
			Reason:    reason,
		}, events.WithSource("identity"))
	})
	grants.SetOnDeviceIssued(func(viewerID int64, surface string, dev core.Device, isFirstSeen bool) {
		if a.eventBus == nil || !isFirstSeen {
			return
		}
		clientID := dev.ID
		if clientID == "" {
			clientID = dev.DeviceName
		}
		if clientID == "" {
			clientID = dev.Client
		}
		if !a.markDeviceSeen(viewerID, clientID) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		username := a.lookupUsername(ctx, viewerID)
		platform := dev.Platform
		if platform == "" {
			platform = dev.Product
		}
		if platform == "" {
			platform = surface
		}
		name := dev.DeviceName
		if name == "" {
			name = dev.Client
		}
		if name == "" {
			name = dev.Product
		}
		deviceID := clientID
		if deviceID == "" {
			deviceID = surface + ":" + strconv.FormatInt(viewerID, 10)
		}
		a.publishEvent(events.EventAuthDeviceRegistered, events.AuthDeviceRegisteredPayload{
			DeviceID:             deviceID,
			ClientIdentifier:     dev.ID,
			Name:                 name,
			Platform:             platform,
			ViewerID:             viewerID,
			Username:             username,
			IsFirstSeenForViewer: true,
		}, events.WithSource("identity"))
	})
}

// wireIdentityLimiter attaches auth rate-limiting instrumentation.
func (a *App) wireIdentityLimiter(limiter *identity.LoginLimiter, scope string) {
	if a == nil || limiter == nil {
		return
	}
	limiter.SetOnLimited(func(ev identity.RateLimitEvent) {
		ns := ev.Namespace
		if ns == "" {
			ns = scope
		}
		if ns == "" {
			ns = "login"
		}
		if ev.FirstInWin && a.log != nil {
			app := ev.App
			if app == "" {
				app = "unknown"
			}
			rem := ev.Remaining
			if rem >= time.Second {
				rem = rem.Round(time.Second)
			} else if rem > 0 {
				rem = rem.Round(time.Millisecond)
			}
			attrs := []any{
				"app", app,
				"namespace", ns,
				"scope", ev.BucketKind,
				"remote_ip", ev.RemoteIP,
				"remaining", rem.String(),
			}
			if ev.Identity != "" {
				attrs = append(attrs, "identity", ev.Identity)
			}
			a.log.Warn("sign-in locked out", attrs...)
		}
		if a.eventBus == nil {
			return
		}
		a.publishEvent(events.EventAuthRateLimited, events.AuthRateLimitedPayload{
			Scope:     ns,
			RemoteIP:  ev.RemoteIP,
			Attempts:  ev.Attempts,
			WindowSec: ev.WindowSec,
		}, events.WithSource("identity"))
	})
}

// wireIdentityQuickConnect attaches Quick Connect approval instrumentation.
func (a *App) wireIdentityQuickConnect(quick *identity.QuickConnect) {
	if a == nil || quick == nil {
		return
	}
	quick.SetOnApproved(func(viewerID int64, target identity.ApprovedTarget) {
		if a.eventBus == nil {
			return
		}
		deviceName := target.DeviceName
		if deviceName == "" {
			deviceName = target.Client
		}
		if deviceName == "" {
			deviceName = target.Product
		}
		platform := target.Surface
		if platform == "" {
			platform = target.Product
		}
		a.publishEvent(events.EventAuthDeviceQuickConnectApproved, events.AuthDeviceQuickConnectApprovedPayload{
			ViewerID:   viewerID,
			DeviceName: deviceName,
			Platform:   platform,
		}, events.WithSource("identity"))
	})
}

// wireIdentityInvite attaches viewer invite lifecycle instrumentation.
func (a *App) wireIdentityInvite(inv *identity.Invite) {
	if a == nil || inv == nil {
		return
	}
	inv.SetOnEvent(func(ev identity.InviteEvent) {
		if a.eventBus == nil {
			return
		}
		switch ev.Kind {
		case "created":
			a.publishEvent(events.EventViewerInviteCreated, events.ViewerInviteCreatedPayload{
				InviteID:  ev.InviteID,
				Label:     ev.Label,
				MaxUses:   ev.MaxUses,
				ExpiresAt: ev.ExpiresAt,
				Actor:     "owner",
			}, events.WithSource("identity"))
		case "accepted":
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			username := a.lookupUsername(ctx, ev.ViewerID)
			a.publishEvent(events.EventViewerInviteAccepted, events.ViewerInviteAcceptedPayload{
				InviteID: ev.InviteID,
				ViewerID: ev.ViewerID,
				Username: username,
			}, events.WithSource("identity"))
		case "viewer_created":
			a.publishEvent(events.EventViewerAccountJoined, events.ViewerAccountJoinedPayload{
				ViewerID: ev.ViewerID,
				Username: ev.Label,
				Role:     "local",
				Source:   "invite",
				InviteID: ev.InviteID,
			}, events.WithSource("identity"))
		}
	})
}

func (a *App) markDeviceSeen(viewerID int64, clientID string) bool {
	if clientID == "" {
		return true
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return true
	}
	key := strconv.FormatInt(viewerID, 10) + "|" + clientID
	h.devicesMu.Lock()
	defer h.devicesMu.Unlock()
	if _, ok := h.seenDevices[key]; ok {
		return false
	}
	if len(h.seenDevices) >= maxTrackedSeenDevices {
		// Evict oldest arbitrary entry to stay bounded.
		for k := range h.seenDevices {
			delete(h.seenDevices, k)
			break
		}
	}
	h.seenDevices[key] = struct{}{}
	return true
}

func (a *App) onStatsDeviceRegistered(dev stats.Device) {
	if a == nil || a.eventBus == nil {
		return
	}
	if !a.markDeviceSeen(0, dev.ClientIdentifier) {
		return
	}
	a.publishEvent(events.EventAuthDeviceRegistered, events.AuthDeviceRegisteredPayload{
		DeviceID:             "dev_" + strconv.FormatInt(dev.ID, 10),
		ClientIdentifier:     dev.ClientIdentifier,
		Name:                 dev.Name,
		Platform:             dev.Platform,
		IsFirstSeenForViewer: true,
	}, events.WithSource("stats"))
}

func (a *App) emitLoginSucceeded(viewerID int64, username, method, client, platform, remoteIP string) {
	if a == nil || a.eventBus == nil {
		return
	}
	a.publishEvent(events.EventAuthLoginSucceeded, events.AuthLoginSucceededPayload{
		ViewerID: viewerID,
		Username: username,
		Method:   method,
		Client:   client,
		Platform: platform,
		RemoteIP: remoteIP,
	}, events.WithSource("auth"))
	if viewerID > 0 && remoteIP != "" {
		a.observeViewerIP(viewerID, username, remoteIP, client, platform, "")
	}
}

func (a *App) emitLoginFailed(username, method, reason, client, platform, remoteIP string) {
	if a == nil || a.eventBus == nil {
		return
	}
	a.publishEvent(events.EventAuthLoginFailed, events.AuthLoginFailedPayload{
		Username: username,
		Method:   method,
		Reason:   reason,
		Client:   client,
		Platform: platform,
		RemoteIP: remoteIP,
	}, events.WithSource("auth"))
}

func (a *App) emitTokenExpired(provider, scope, accountID string, viewerID int64, errMsg string) {
	if a == nil || a.eventBus == nil {
		return
	}
	var username string
	if viewerID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		username = a.lookupUsername(ctx, viewerID)
	}
	a.publishEvent(events.EventAuthTokenExpired, events.AuthTokenExpiredPayload{
		Provider:  provider,
		Scope:     scope,
		AccountID: accountID,
		ViewerID:  viewerID,
		Username:  username,
		Error:     errMsg,
	}, events.WithSource(provider))
}

func (a *App) emitCertificateRenewed(domains []string, issuer string, expiresAt time.Time) {
	if a == nil || a.eventBus == nil {
		return
	}
	a.publishEvent(events.EventAuthCertificateRenewed, events.AuthCertificateRenewedPayload{
		Domains:   slices.Clone(domains),
		Issuer:    issuer,
		ExpiresAt: expiresAt,
	}, events.WithSource("certs"))
}

func (a *App) onPlaybackEventForAuth(ev playback.Event) {
	if a == nil || a.eventBus == nil || ev.Session == nil {
		return
	}
	if ev.Kind != "prepared" && ev.Kind != "started" && ev.Kind != "decision" {
		return
	}
	s := ev.Session
	if s.UserID <= 0 || strings.TrimSpace(s.Client.ClientIP) == "" {
		return
	}
	username := s.Username
	if username == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		username = a.lookupUsername(ctx, s.UserID)
	}
	a.observeViewerIP(s.UserID, username, s.Client.ClientIP, s.Client.Product, s.Client.Platform, s.ID)
}

func (a *App) observeViewerIP(viewerID int64, username, rawIP, client, platform, sessionID string) {
	if a == nil || a.eventBus == nil || viewerID <= 0 {
		return
	}
	ip := normalizeIPString(rawIP)
	if ip == "" {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil || h.ips == nil {
		return
	}
	prev, isNew := h.ips.Observe(viewerID, ip)
	if !isNew {
		return
	}
	location := "wan"
	if addr, err := netip.ParseAddr(ip); err == nil && identity.LAN(addr) {
		location = "lan"
	}
	distinct := a.distinctActiveViewerIPs(viewerID, ip)
	a.publishEvent(events.EventAuthSessionNewIP, events.AuthSessionNewIPPayload{
		ViewerID:          viewerID,
		Username:          username,
		RemoteIP:          ip,
		PreviousIPs:       prev,
		Location:          location,
		Client:            client,
		Platform:          platform,
		SessionID:         sessionID,
		DistinctActiveIPs: distinct,
	}, events.WithSource("auth"))
}

func (a *App) distinctActiveViewerIPs(viewerID int64, currentIP string) int {
	ips := map[string]struct{}{}
	if currentIP != "" {
		ips[currentIP] = struct{}{}
	}
	if a != nil && a.playback != nil {
		for _, s := range a.playback.Sessions() {
			if s == nil || s.UserID != viewerID {
				continue
			}
			if ip := normalizeIPString(s.Client.ClientIP); ip != "" {
				ips[ip] = struct{}{}
			}
		}
	}
	if len(ips) == 0 {
		return 1
	}
	return len(ips)
}

func normalizeIPString(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.Unmap().String()
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().String()
	}
	return s
}

// --- Cluster domain emitters ---

func (a *App) triggerClusterMembershipObserve() {
	if a == nil || a.cluster == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return
	}
	select {
	case <-h.clusterStop:
		return
	default:
	}
	h.clusterKick.Store(true)
	if !h.clusterRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-h.clusterStop:
				cancel()
			case <-ctx.Done():
			}
		}()
		defer cancel()
		for {
			h.clusterKick.Store(false)
			if ctx.Err() == nil {
				a.observeClusterMembership(ctx)
			}
			h.clusterRunning.Store(false)
			if !h.clusterKick.Load() || ctx.Err() != nil || !h.clusterRunning.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

func (a *App) seedClusterMembership(ctx context.Context) {
	if a == nil || a.cluster == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return
	}
	h.clusterObserveMu.Lock()
	defer h.clusterObserveMu.Unlock()
	nodes, err := a.cluster.Nodes(ctx)
	if err != nil {
		return
	}
	h.clusterMu.Lock()
	defer h.clusterMu.Unlock()
	if h.clusterSeeded {
		return
	}
	for _, n := range nodes {
		h.clusterMembers[n.ID] = n
		if n.Leader {
			h.lastLeaderID = n.ID
		}
	}
	h.lastLeaderEp = a.cluster.Epoch()
	h.clusterSeeded = true
}

func (a *App) observeClusterMembership(ctx context.Context) {
	if a == nil || a.cluster == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return
	}
	h.clusterObserveMu.Lock()
	defer h.clusterObserveMu.Unlock()
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	live, err := a.cluster.Nodes(qctx)
	if err != nil {
		return
	}
	liveMap := make(map[string]cluster.Node, len(live))
	for _, n := range live {
		liveMap[n.ID] = n
	}

	h.clusterMu.Lock()
	if !h.clusterSeeded {
		h.clusterMembers = liveMap
		for _, n := range live {
			if n.Leader {
				h.lastLeaderID = n.ID
			}
		}
		h.lastLeaderEp = a.cluster.Epoch()
		h.clusterSeeded = true
		h.clusterMu.Unlock()
		return
	}
	var joined []cluster.Node
	var lost []cluster.Node
	for id, n := range liveMap {
		if _, existed := h.clusterMembers[id]; !existed {
			joined = append(joined, n)
		}
	}
	for id, prev := range h.clusterMembers {
		if _, stillLive := liveMap[id]; !stillLive {
			lost = append(lost, prev)
		}
	}
	h.clusterMembers = liveMap
	h.clusterMu.Unlock()

	memberCount := len(liveMap)
	for _, n := range joined {
		a.publishEvent(events.EventClusterNodeJoined, events.ClusterNodeJoinedPayload{
			NodeID:      n.ID,
			NodeName:    n.Name,
			Version:     n.Version,
			Roles:       clusterNodeRoles(n),
			MemberCount: memberCount,
		}, events.WithSource("cluster"))
	}
	if len(lost) > 0 {
		allByID := make(map[string]cluster.Node)
		if all, err := a.cluster.AllNodes(qctx); err == nil {
			for _, n := range all {
				allByID[n.ID] = n
			}
		}
		now := time.Now()
		for _, prev := range lost {
			cur := prev
			if fresh, ok := allByID[prev.ID]; ok {
				cur = fresh
			}
			lastSeenMS := cur.LastSeenAge(now).Milliseconds()
			if lastSeenMS < 0 {
				lastSeenMS = 0
			}
			a.publishEvent(events.EventClusterNodeLost, events.ClusterNodeLostPayload{
				NodeID:      cur.ID,
				NodeName:    cur.Name,
				LastSeenMS:  lastSeenMS,
				Roles:       clusterNodeRoles(cur),
				MemberCount: memberCount,
			}, events.WithSource("cluster"))
		}
	}
}

func (a *App) onClusterLeaderChange(leader bool) {
	if a == nil || a.cluster == nil || a.eventBus == nil || !leader {
		return
	}
	h := a.ensureSystemEvents()
	self := a.cluster.Self()
	epoch := a.cluster.Epoch()

	var prevLeader string
	if h != nil {
		h.clusterMu.Lock()
		if h.lastLeaderID == self.ID && h.lastLeaderEp == epoch {
			h.clusterMu.Unlock()
			return
		}
		if h.lastLeaderID != self.ID {
			prevLeader = h.lastLeaderID
		}
		h.lastLeaderID = self.ID
		h.lastLeaderEp = epoch
		h.clusterMu.Unlock()
	}
	a.publishEvent(events.EventClusterLeaderElected, events.ClusterLeaderElectedPayload{
		LeaderNodeID:     self.ID,
		LeaderName:       self.Name,
		Epoch:            epoch,
		PreviousLeaderID: prevLeader,
	}, events.WithSource("cluster"))
}

func (a *App) emitClusterNodeDraining(reason string) {
	if a == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h != nil && !h.drainEmitted.CompareAndSwap(false, true) {
		return
	}
	var nodeID, nodeName string
	if a.cluster != nil {
		self := a.cluster.Self()
		nodeID = self.ID
		nodeName = self.Name
	}
	if reason == "" {
		reason = "drain"
	}
	a.publishEvent(events.EventClusterNodeDraining, events.ClusterNodeDrainingPayload{
		NodeID:         nodeID,
		NodeName:       nodeName,
		Reason:         reason,
		ActiveSessions: a.safeActiveSessionCount(),
	}, events.WithSource("cluster"))
}

func (a *App) resetClusterDrainingFlag() {
	if a == nil || a.systemEvents == nil {
		return
	}
	a.systemEvents.drainEmitted.Store(false)
}

func (a *App) onDatabaseAvailability(reachable bool) {
	if a == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return
	}
	dialect := "sqlite"
	if a.db != nil && store.DialectOf(a.db) == store.Postgres {
		dialect = "postgres"
	} else if a.cluster != nil && a.cluster.Mode() == cluster.ModePostgres {
		dialect = "postgres"
	}
	var lastErr string
	if a.dbAvail != nil {
		lastErr = a.dbAvail.State().LastError
	}

	h.dbMu.Lock()
	if !reachable {
		alreadyDown := h.dbDown
		if !alreadyDown {
			h.dbDown = true
			h.dbDownSince = time.Now()
			h.dbLastDialect = dialect
		}
		h.dbMu.Unlock()
		if !alreadyDown {
			errText := lastErr
			if errText == "" {
				errText = "database unreachable"
			}
			a.publishEvent(events.EventClusterDatabaseUnavailable, events.ClusterDatabaseUnavailablePayload{
				Dialect: dialect,
				Error:   errText,
			}, events.WithSource("database"))
			a.observeSystemHealth("database", "down", errText)
		}
		return
	}
	wasDown := h.dbDown
	downSince := h.dbDownSince
	if h.dbLastDialect != "" {
		dialect = h.dbLastDialect
	}
	h.dbDown = false
	h.dbDownSince = time.Time{}
	h.dbMu.Unlock()

	if wasDown {
		dur := time.Since(downSince).Milliseconds()
		if dur < 0 {
			dur = 0
		}
		a.publishEvent(events.EventClusterDatabaseRecovered, events.ClusterDatabaseRecoveredPayload{
			Dialect:          dialect,
			OutageDurationMS: dur,
			ReplayedWrites:   0,
		}, events.WithSource("database"))
		a.observeSystemHealth("database", "ok", "database connection restored")
	}
}

// --- System & Flows domain emitters ---

func (a *App) safeActiveSessionCount() int {
	return a.activeSessionCount()
}

func (a *App) emitServerStarted() {
	if a == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	mode := "standalone"
	var nodeID string
	if a.cluster != nil {
		nodeID = a.cluster.Self().ID
		if a.cluster.Mode() == cluster.ModePostgres {
			mode = "cluster"
		}
	}
	var startupMS int64
	if h != nil && !h.bootAt.IsZero() {
		startupMS = time.Since(h.bootAt).Milliseconds()
		if startupMS < 0 {
			startupMS = 0
		}
	}
	a.publishEvent(events.EventSystemServerStarted, events.SystemServerStartedPayload{
		Version:   a.opts.Version,
		Mode:      mode,
		NodeID:    nodeID,
		StartupMS: startupMS,
	}, events.WithSource("app"))
}

func (a *App) emitServerStopping(reason string) {
	if a == nil || a.eventBus == nil {
		return
	}
	if reason == "" {
		reason = "shutdown"
	}
	a.publishEvent(events.EventSystemServerStopping, events.SystemServerStoppingPayload{
		Version:        a.opts.Version,
		Reason:         reason,
		ActiveSessions: a.safeActiveSessionCount(),
	}, events.WithSource("app"))
}

func (a *App) emitServerCrashed(exitReason, watcher string) {
	if a == nil || a.eventBus == nil {
		return
	}
	if exitReason == "" {
		exitReason = "unexpected exit"
	}
	a.publishEvent(events.EventSystemServerCrashed, events.SystemServerCrashedPayload{
		Version:    a.opts.Version,
		ExitReason: exitReason,
		Watcher:    watcher,
	}, events.WithSource("app"))
}

func (a *App) emitSystemHealthChanged(component, fromStatus, toStatus, summary string) {
	if a == nil || a.eventBus == nil {
		return
	}
	a.publishEvent(events.EventSystemHealthChanged, events.SystemHealthChangedPayload{
		FromStatus: fromStatus,
		ToStatus:   toStatus,
		Component:  component,
		Summary:    summary,
	}, events.WithSource("health"))
}

func (a *App) observeSystemHealth(component, status, summary string) {
	if a == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil || h.health == nil {
		return
	}
	h.health.Observe(component, status, summary)
}

func (a *App) observeIntegrityResult(res store.Integrity) {
	if a == nil || a.eventBus == nil {
		return
	}
	if res.OK {
		a.observeSystemHealth("database_integrity", "ok", "database integrity check passed")
		return
	}
	summary := "database integrity check failed"
	if len(res.Findings) > 0 {
		summary = res.Findings[0]
	}
	a.observeSystemHealth("database_integrity", "degraded", summary)
}

func (a *App) observeReachabilityChange() {
	if a == nil || a.registrar == nil || a.eventBus == nil {
		return
	}
	st := a.registrar.Status()
	if st.Reachability != nil {
		status := "unreachable"
		if st.Reachability.Reachable {
			status = "reachable"
		}
		uri := st.Reachability.URI
		var errText string
		var latencyMS int64
		for _, res := range st.Reachability.Results {
			if uri == "" {
				uri = res.URI
			}
			if res.OK && res.Latency != "" {
				if d, err := time.ParseDuration(res.Latency); err == nil {
					latencyMS = d.Milliseconds()
				}
			}
			if !res.OK && errText == "" && res.Error != "" {
				errText = res.Error
			}
		}
		a.recordReachability("remote_access", status, uri, 0, latencyMS, errText)
	}
	if st.PlexReachable != nil {
		status := "unreachable"
		if *st.PlexReachable {
			status = "reachable"
		}
		a.recordReachability("external", status, st.PublicAddress, 0, 0, "")
	}
}

func (a *App) recordReachability(scope, toStatus, url string, statusCode int, latencyMS int64, errText string) {
	if a == nil || a.eventBus == nil || scope == "" || toStatus == "" {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return
	}
	h.reachMu.Lock()
	prev, existed := h.reachStates[scope]
	if existed && prev == toStatus {
		h.reachMu.Unlock()
		return
	}
	h.reachStates[scope] = toStatus
	h.reachMu.Unlock()

	if !existed {
		// Seed baseline when first observed as reachable; only emit immediately
		// if the very first observation is already unreachable.
		if toStatus == "reachable" {
			return
		}
		prev = "unknown"
	}
	a.publishEvent(events.EventSystemReachabilityChanged, events.SystemReachabilityChangedPayload{
		Scope:      scope,
		FromStatus: prev,
		ToStatus:   toStatus,
		URL:        url,
		StatusCode: statusCode,
		LatencyMS:  latencyMS,
		Error:      errText,
	}, events.WithSource("registrar"))
}

func (a *App) emitUpdateAvailable(currentVersion, newVersion, channel string, critical bool) {
	if a == nil || a.eventBus == nil {
		return
	}
	if channel == "" {
		channel = "stable"
	}
	a.publishEvent(events.EventSystemUpdateAvailable, events.SystemUpdateAvailablePayload{
		CurrentVersion: currentVersion,
		NewVersion:     newVersion,
		Channel:        channel,
		Critical:       critical,
	}, events.WithSource("updater"))
}

func (a *App) emitUpdateInstalled(fromVersion, toVersion, channel string, auto bool) {
	if a == nil || a.eventBus == nil {
		return
	}
	if channel == "" {
		channel = "stable"
	}
	a.publishEvent(events.EventSystemUpdateInstalled, events.SystemUpdateInstalledPayload{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		Channel:     channel,
		Auto:        auto,
	}, events.WithSource("updater"))
}

func (a *App) emitUpdateRolledBack(failedVersion, restoredVersion, reason string) {
	if a == nil || a.eventBus == nil {
		return
	}
	a.publishEvent(events.EventSystemUpdateRolledBack, events.SystemUpdateRolledBackPayload{
		FailedVersion:   failedVersion,
		RestoredVersion: restoredVersion,
		Reason:          reason,
	}, events.WithSource("updater"))
}

func (a *App) onBackupEvent(ev backup.BackupEvent) {
	if a == nil || a.eventBus == nil {
		return
	}
	kind := ev.Kind
	if kind != "manual" {
		kind = "scheduled"
	}
	if ev.Err != nil {
		a.publishEvent(events.EventSystemBackupFailed, events.SystemBackupFailedPayload{
			Kind:                kind,
			Error:               ev.Err.Error(),
			ConsecutiveFailures: ev.ConsecutiveFailures,
		}, events.WithSource("backup"))
		return
	}
	a.publishEvent(events.EventSystemBackupCompleted, events.SystemBackupCompletedPayload{
		Kind:        kind,
		SizeBytes:   ev.SizeBytes,
		DurationMS:  ev.DurationMS,
		Destination: ev.Destination,
		Encrypted:   ev.Encrypted,
	}, events.WithSource("backup"))
}

func (a *App) checkDiskSpace() {
	if a == nil || a.eventBus == nil {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil || h.diskFunc == nil {
		return
	}
	type volumeCheck struct {
		name string
		path string
	}
	var checks []volumeCheck
	if a.opts.DataDir != "" {
		checks = append(checks, volumeCheck{name: "data", path: a.opts.DataDir})
	}
	if a.cfg != nil {
		cacheDir := a.cfg.Read().EffectiveCache().Dir
		if cacheDir == "" && a.opts.DataDir != "" {
			cacheDir = filepath.Join(a.opts.DataDir, "cache")
		}
		if cacheDir != "" && cacheDir != a.opts.DataDir {
			checks = append(checks, volumeCheck{name: "cache", path: cacheDir})
		}
	}
	for _, vc := range checks {
		total, free, err := h.diskFunc(vc.path)
		if err != nil || total <= 0 {
			continue
		}
		a.evaluateDiskVolume(vc.name, vc.path, total, free)
	}
}

func (a *App) evaluateDiskVolume(volume, path string, totalBytes, freeBytes int64) {
	if a == nil || a.eventBus == nil || totalBytes <= 0 {
		return
	}
	h := a.ensureSystemEvents()
	if h == nil {
		return
	}
	if freeBytes < 0 {
		freeBytes = 0
	}
	if freeBytes > totalBytes {
		freeBytes = totalBytes
	}
	percentUsed := float64(totalBytes-freeBytes) / float64(totalBytes) * 100.0
	isLow := percentUsed >= diskLowPercentThreshold || freeBytes < diskLowMinFreeBytes
	recovered := percentUsed < diskLowRecoverPercent && freeBytes >= diskLowMinFreeBytes*2

	h.diskMu.Lock()
	wasLow := h.diskLow[volume]
	if isLow {
		h.diskLow[volume] = true
	} else if recovered {
		h.diskLow[volume] = false
	}
	h.diskMu.Unlock()

	if isLow && !wasLow {
		a.publishEvent(events.EventSystemDiskLow, events.SystemDiskLowPayload{
			Volume:      volume,
			Path:        path,
			FreeBytes:   freeBytes,
			TotalBytes:  totalBytes,
			PercentUsed: percentUsed,
		}, events.WithSource("disk"))
		a.observeSystemHealth("disk:"+volume, "degraded", "available disk space below threshold")
	} else if recovered && wasLow {
		a.observeSystemHealth("disk:"+volume, "ok", "disk space recovered")
	}
}

func (a *App) emitDigestScheduled(period string, windowStart, windowEnd time.Time, totalPlays, uniqueViewers int) {
	if a == nil || a.eventBus == nil {
		return
	}
	if period == "" {
		period = "weekly"
	}
	a.publishEvent(events.EventFlowsDigestScheduled, events.FlowsDigestScheduledPayload{
		Period:        period,
		WindowStart:   windowStart.UTC(),
		WindowEnd:     windowEnd.UTC(),
		TotalPlays:    totalPlays,
		UniqueViewers: uniqueViewers,
	}, events.WithSource("stats"))
}

// --- Stateful bounded helpers ---

type libraryBatchKey struct {
	LibraryID string
	MediaType string
	ShowKey   string
	Season    int
}

type libraryBatchItem struct {
	LibraryID   string
	LibraryName string
	MediaType   string
	ShowKey     string
	ShowTitle   string
	Season      int
	TitleName   string
	PosterURL   string
}

type libraryBatchGroup struct {
	key          libraryBatchKey
	libraryName  string
	showTitle    string
	posterURL    string
	itemCount    int
	episodeCount int
	titleNames   []string
	timer        *time.Timer
}

// libraryBatcher debounces bursts of library.title.added items into grouped
// library.batch.added events without blocking callers or leaking memory.
type libraryBatcher struct {
	mu     sync.Mutex
	window time.Duration
	emit   func(events.LibraryBatchAddedPayload)
	groups map[libraryBatchKey]*libraryBatchGroup
	closed bool
}

func newLibraryBatcher(window time.Duration, emit func(events.LibraryBatchAddedPayload)) *libraryBatcher {
	if window <= 0 {
		window = defaultLibraryBatchWindow
	}
	return &libraryBatcher{
		window: window,
		emit:   emit,
		groups: make(map[libraryBatchKey]*libraryBatchGroup),
	}
}

func (b *libraryBatcher) Add(item libraryBatchItem) {
	if b == nil {
		return
	}
	key := libraryBatchKey{
		LibraryID: item.LibraryID,
		MediaType: item.MediaType,
		ShowKey:   item.ShowKey,
		Season:    item.Season,
	}
	var evicted *events.LibraryBatchAddedPayload

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	g, ok := b.groups[key]
	if !ok {
		if len(b.groups) >= maxLibraryBatchGroups {
			// Flush one existing group synchronously outside the lock so pending groups stay bounded.
			for k, existing := range b.groups {
				if existing.timer != nil {
					existing.timer.Stop()
				}
				p := b.toPayloadLocked(existing)
				evicted = &p
				delete(b.groups, k)
				break
			}
		}
		g = &libraryBatchGroup{
			key:         key,
			libraryName: item.LibraryName,
			showTitle:   item.ShowTitle,
			posterURL:   item.PosterURL,
		}
		b.groups[key] = g
	}
	g.itemCount++
	if item.MediaType == "episode" {
		g.episodeCount++
	}
	if g.showTitle == "" && item.ShowTitle != "" {
		g.showTitle = item.ShowTitle
	}
	if g.posterURL == "" && item.PosterURL != "" {
		g.posterURL = item.PosterURL
	}
	if item.TitleName != "" && len(g.titleNames) < maxLibraryBatchSample {
		g.titleNames = append(g.titleNames, item.TitleName)
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	g.timer = time.AfterFunc(b.window, func() { b.flushKey(key) })
	b.mu.Unlock()

	if evicted != nil && b.emit != nil {
		b.emit(*evicted)
	}
}

func (b *libraryBatcher) flushKey(key libraryBatchKey) {
	b.mu.Lock()
	g, ok := b.groups[key]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.groups, key)
	if g.timer != nil {
		g.timer.Stop()
	}
	payload := b.toPayloadLocked(g)
	b.mu.Unlock()

	if b.emit != nil {
		b.emit(payload)
	}
}

// Flush immediately emits all pending batches in deterministic order.
func (b *libraryBatcher) Flush() {
	if b == nil {
		return
	}
	b.mu.Lock()
	payloads := b.drainLocked()
	b.mu.Unlock()

	if b.emit != nil {
		for _, p := range payloads {
			b.emit(p)
		}
	}
}

// Close flushes pending batches and prevents further timers from starting.
func (b *libraryBatcher) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	payloads := b.drainLocked()
	b.mu.Unlock()

	if b.emit != nil {
		for _, p := range payloads {
			b.emit(p)
		}
	}
}

func (b *libraryBatcher) drainLocked() []events.LibraryBatchAddedPayload {
	if len(b.groups) == 0 {
		return nil
	}
	keys := make([]libraryBatchKey, 0, len(b.groups))
	for k := range b.groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].LibraryID != keys[j].LibraryID {
			return keys[i].LibraryID < keys[j].LibraryID
		}
		if keys[i].MediaType != keys[j].MediaType {
			return keys[i].MediaType < keys[j].MediaType
		}
		if keys[i].ShowKey != keys[j].ShowKey {
			return keys[i].ShowKey < keys[j].ShowKey
		}
		return keys[i].Season < keys[j].Season
	})
	out := make([]events.LibraryBatchAddedPayload, 0, len(keys))
	for _, k := range keys {
		g := b.groups[k]
		if g.timer != nil {
			g.timer.Stop()
		}
		out = append(out, b.toPayloadLocked(g))
		delete(b.groups, k)
	}
	return out
}

func (b *libraryBatcher) toPayloadLocked(g *libraryBatchGroup) events.LibraryBatchAddedPayload {
	windowSec := int(b.window.Seconds())
	if windowSec <= 0 {
		windowSec = 1
	}
	return events.LibraryBatchAddedPayload{
		LibraryID:    g.key.LibraryID,
		LibraryName:  g.libraryName,
		MediaType:    g.key.MediaType,
		ShowKey:      g.key.ShowKey,
		ShowTitle:    g.showTitle,
		Season:       g.key.Season,
		EpisodeCount: g.episodeCount,
		ItemCount:    g.itemCount,
		TitleNames:   slices.Clone(g.titleNames),
		WindowSec:    windowSec,
		PosterURL:    g.posterURL,
	}
}

// componentHealthTracker is edge-triggered: it emits only on transitions.
type componentHealthTracker struct {
	mu     sync.Mutex
	states map[string]string
	emit   func(component, fromStatus, toStatus, summary string)
}

func newComponentHealthTracker(emit func(component, fromStatus, toStatus, summary string)) *componentHealthTracker {
	return &componentHealthTracker{
		states: make(map[string]string),
		emit:   emit,
	}
}

func (t *componentHealthTracker) Observe(component, toStatus, summary string) {
	if t == nil || component == "" || toStatus == "" {
		return
	}
	t.mu.Lock()
	prev, existed := t.states[component]
	if existed && prev == toStatus {
		t.mu.Unlock()
		return
	}
	t.states[component] = toStatus
	t.mu.Unlock()

	if !existed {
		// Initial "ok" seeds the baseline without emitting a spurious transition.
		if toStatus == "ok" {
			return
		}
		prev = "ok"
	}
	if t.emit != nil {
		t.emit(component, prev, toStatus, summary)
	}
}

// viewerIPTracker tracks recently seen IPs per viewer with strict memory bounds.
// The first IP observed for a viewer establishes a baseline and does not fire;
// subsequent previously unseen IPs return (previousIPs, true).
type viewerIPTracker struct {
	mu      sync.Mutex
	byUser  map[int64]*viewerIPHistory
	userLRU []int64
}

type viewerIPHistory struct {
	seen  map[string]struct{}
	order []string
}

func newViewerIPTracker() *viewerIPTracker {
	return &viewerIPTracker{
		byUser: make(map[int64]*viewerIPHistory),
	}
}

func (t *viewerIPTracker) Observe(viewerID int64, ip string) (previous []string, isNew bool) {
	if t == nil || viewerID <= 0 || ip == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	h, ok := t.byUser[viewerID]
	if !ok {
		if len(t.byUser) >= maxTrackedViewerIPUsers && len(t.userLRU) > 0 {
			evict := t.userLRU[0]
			t.userLRU = t.userLRU[1:]
			delete(t.byUser, evict)
		}
		h = &viewerIPHistory{
			seen:  map[string]struct{}{ip: {}},
			order: []string{ip},
		}
		t.byUser[viewerID] = h
		t.userLRU = append(t.userLRU, viewerID)
		return nil, false
	}
	if _, known := h.seen[ip]; known {
		return nil, false
	}
	// Collect up to maxTrackedPreviousIPs most recent previous IPs.
	start := 0
	if len(h.order) > maxTrackedPreviousIPs {
		start = len(h.order) - maxTrackedPreviousIPs
	}
	previous = slices.Clone(h.order[start:])

	if len(h.order) >= maxTrackedIPsPerViewer {
		oldest := h.order[0]
		h.order = h.order[1:]
		delete(h.seen, oldest)
	}
	h.seen[ip] = struct{}{}
	h.order = append(h.order, ip)
	return previous, true
}
