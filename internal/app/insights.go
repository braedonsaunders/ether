package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/hoststat"
	"github.com/braedonsaunders/ether/internal/notify"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/stats"
)

func (a *App) buildInsights() {
	a.notifications = notify.New(a.db, notify.Options{Sealer: a.cfg, Providers: a.fetcher.Stats, CacheUsage: a.cache.Usage, NodeID: a.clusterNodeID(), Logger: a.log.With("component", "notifications")})
	// Per-node Dashboard rings: a clustered client reads the node it reached.
	a.hostResources = hoststat.NewResources(hoststat.Options{})
	a.stats = stats.New(a.db, stats.Options{NodeID: a.clusterNodeID(), Now: parityClock(), Availability: a.dbAvail, Playback: a.playback, SessionExists: func(id string) bool { _, ok := (ownerPlayback{Playback: a.playback, app: a}).Session(id); return ok }, Providers: a.fetcher.Stats, Indexers: a.indexers.Remaining, Logger: a.log.With("component", "stats"), StopSession: a.stopOwnerSession, MetadataType: func(key core.RatingKey) int {
		if a.catalog == nil {
			return 0
		}
		t, err := a.catalog.Get(context.Background(), key)
		if err != nil || t == nil {
			return 0
		}
		switch t.Type {
		case core.TypeMovie:
			return 1
		case core.TypeShow:
			return 2
		case core.TypeSeason:
			return 3
		case core.TypeEpisode:
			return 4
		default:
			return 0
		}
	}, AccountName: func(id int64) string {
		if a.users == nil {
			return ""
		}
		u, err := a.users.User(context.Background(), id)
		if err != nil || u == nil {
			return ""
		}
		return u.Username
	}, Newsletter: func(ctx context.Context, html string) error {
		return a.notifications.Emit(ctx, notify.Event{Kind: "newsletter.weekly", Title: "Your week on Ether", Message: "Weekly viewing summary", HTML: html})
	}})
	a.closers = append(a.closers, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.stats.Close(ctx); err != nil {
			a.log.Warn("stats: play records lost at shutdown", "err", err)
		}
	})
	notifyPlayback := a.notifications.ObservePlayback
	if a.clustered() {
		// Playback events fire on the stream's own path (start, failover,
		// stop); queueing a notification in the shared database must not
		// stall a failover while the database is unreachable.
		notifyPlayback = func(ev playback.Event) { go a.notifications.ObservePlayback(ev) }
	}
	a.closers = append(a.closers, a.playback.Subscribe(a.stats.ObservePlayback), a.playback.Subscribe(notifyPlayback), a.state.Subscribe(a.stats.ObserveState), a.indexers.Subscribe(a.stats.ObserveIndexer), a.indexers.Subscribe(a.notifications.ObserveIndexer))
}

func (a *App) widgetStatus(context.Context) any {
	groups := map[string]bool{}
	for _, s := range (ownerPlayback{Playback: a.playback, app: a}).Sessions() {
		id := s.PlaybackGroup
		if id == "" {
			id = s.ID
		}
		groups[id] = true
	}
	providers := a.fetcher.Stats()
	healthy := 0
	for _, p := range providers {
		if p.State == "ok" {
			healthy++
		}
	}
	used, budget := a.cache.Usage()
	return map[string]any{"server": a.cfg.Read().Server.FriendlyName, "version": a.opts.Version, "streams": len(groups), "providers": len(providers), "healthyProviders": healthy, "cacheUsed": used, "cacheBudget": budget}
}

// WatchNotifications is enabled by the executable, never by embedded/test apps.
func (a *App) WatchNotifications(executable string) (func(bool) error, error) {
	return a.notifications.WatchProcess(executable)
}
func (a *App) notifyLifecycle(kind, title, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.notifications.Emit(ctx, notify.Event{Kind: kind, Title: title, Message: message}); err != nil {
		a.log.Warn("server notification could not be queued")
	}
	_ = a.notifications.Drain(ctx)
}
