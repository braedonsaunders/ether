package app

import (
	"context"
	"io"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
)

func (a *App) buildSessionEvents(context.Context, config.Config) error {
	a.sessionEvents = sessions.NewEventBus(a.clusterNode(), a.users.ActiveUser)
	a.closers = append(a.closers, a.sessionEvents.Close)
	return nil
}

func (a *App) newSessionBroker(events *sessions.EventBus) (*sessions.Broker, error) {
	var tc core.Transcoder
	if a.transcoder != nil {
		tc = a.transcoder
	}
	visibility := experience.Visibility{Policy: a.safety, GetTitle: a.catalog.Get, SectionFor: func(t core.MediaType) (int, bool) {
		want := core.TypeMovie
		switch t {
		case core.TypeShow, core.TypeSeason, core.TypeEpisode:
			want = core.TypeShow
		}
		for _, section := range a.plexServer.LibrarySections() {
			if section.Type == want {
				return section.ID, true
			}
		}
		return 0, false
	}}
	return sessions.NewBroker(sessions.BrokerDeps{
		DB: a.db, Cluster: a.clusterNode(),
		Lookup: func(ctx context.Context, v *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			title, err := a.catalog.Get(ctx, key)
			if err != nil {
				return nil, err
			}
			if title == nil || !visibility.Playable(ctx, v, title) {
				return nil, core.ErrNotFound
			}
			return title, nil
		},
		Playback: a.playback, Transcoder: tc, Progress: a.state,
		ActiveViewer: func(ctx context.Context, id int64) (*core.Viewer, error) {
			v, err := a.users.ActiveUser(ctx, id)
			if err != nil || v == nil {
				return v, err
			}
			return a.safety.UserPolicy(v), nil
		},
		CanPlay: a.safety.CanPlay, BitrateLimit: a.safety.BitrateLimit,
		RemoteBitrateLimit: func() int { return a.cfg.Get().Transcode.RemoteMaxBitrateKbps },
		OnTimeline: func(v *core.Viewer, key core.RatingKey, pos, dur time.Duration, playState string) {
			if playState == "playing" && a.personalize != nil {
				a.personalize.NotePlayback(v, key)
			}
			if a.prefetch != nil {
				a.prefetch.OnTimeline(v, key, pos, dur, playState)
			}
		},
		Events:       events,
		OpenSubtitle: a.subtitles.OpenFormat,
		ExtractSubtitle: func(ctx context.Context, input string, index int, codec string, w io.Writer) error {
			if a.transcoder == nil {
				return core.ErrNotAvailable
			}
			return a.transcoder.ExtractSubtitle(ctx, input, index, codec, w)
		},
		OnEventError: func(err error) { a.log.Warn("session event publication failed", "err", err) },
	})
}

// subscribeSessionState publishes committed state invalidations once and keeps
// Plex's own scrobble notifications on their existing path.
func subscribeSessionState(watch *state.State, events *sessions.EventBus, plexRefresh func(int64, core.RatingKey), onError func(error)) func() {
	return watch.Subscribe(func(ev state.Event) {
		if ev.Kind == state.EventProgress && ev.Completed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := events.Publish(ctx, frontend.Event{Type: "UserDataChanged", ViewerID: ev.UserID, Item: ev.Key, OriginProtocol: ev.Origin.Surface, OriginSessionID: ev.Origin.SessionID, OriginClientID: ev.Origin.ClientID}); err != nil && onError != nil {
			onError(err)
		}
		if ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble || ev.Kind == state.EventCompleted {
			if err := events.Publish(ctx, frontend.Event{Type: "WatchStateChanged", ViewerID: ev.UserID, Item: ev.Key, OriginProtocol: ev.Origin.Surface, OriginSessionID: ev.Origin.SessionID, OriginClientID: ev.Origin.ClientID}); err != nil && onError != nil {
				onError(err)
			}
		}
		if ev.Origin.Surface != "plex" && (ev.Kind == state.EventScrobble || ev.Kind == state.EventUnscrobble) && plexRefresh != nil {
			plexRefresh(ev.UserID, ev.Key)
		}
	})
}
