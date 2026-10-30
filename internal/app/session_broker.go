package app

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

func (a *App) buildSessionEvents(context.Context, config.Config) error {
	a.sessionCoordination = sessions.NewCoordination()
	a.sessionEvents = sessions.NewEventBus(a.clusterNode(), a.users.ActiveUser)
	a.closers = append(a.closers, a.sessionEvents.Close)
	return nil
}

func (a *App) newSessionBroker(events *sessions.EventBus) (*sessions.Broker, error) {
	var tc core.Transcoder
	if a.transcoder != nil {
		tc = a.transcoder
	}
	var live sessions.LiveBackend
	if a.livetv != nil {
		live = sessionLive{service: a.livetv, cached: a.catalog.CachedGet}
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
		Coordination: a.sessionCoordination,
		DB:           a.db, Cluster: a.clusterNode(), Live: live,
		Variants: func(ctx context.Context, v *core.Viewer, t *core.Title, caps core.ClientCaps) ([]core.ReleaseVariant, error) {
			if a.resolver == nil {
				return nil, core.ErrNotAvailable
			}
			return scorecardVariants(ctx, a, v, t, caps)
		},
		SelectScopedTracks: func(ctx context.Context, viewer *core.Viewer, device core.Device, title *core.Title, info core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool) (core.DecideOptions, error) {
			if a.trackprefs == nil {
				return opts, nil
			}
			scope := trackprefs.Scope{Device: device.ID}
			if title.ShowKey != 0 {
				scope.Series = strconv.FormatInt(int64(title.ShowKey), 10)
			} else if title.Key.Kind() == core.KindEpisode || title.Key.Kind() == core.KindSeason || title.Key.Kind() == core.KindShow {
				scope.Series = strconv.FormatInt(int64(core.ShowKey(title.Key.TMDB())), 10)
			}
			if section, ok := visibility.SectionFor(title.Type); ok {
				scope.Library = strconv.Itoa(section)
			}
			rules, _, err := a.trackprefs.ScopedRules(ctx, viewer.ID, scope)
			if err != nil {
				return opts, err
			}
			opts.MaxBitrateKbps = trackprefs.LimitBitrate(opts.MaxBitrateKbps, rules.MaxBitrateKbps)
			opts.MaxHeight = trackprefs.LimitBitrate(opts.MaxHeight, rules.MaxHeight)
			prefs := experience.TrackPreferences{Source: scopedTrackSource{Service: a.trackprefs, scope: scope}, GetTitle: a.catalog.Get}
			return selectBrokerTracks(ctx, prefs, viewer, title, info, opts, audioSet, subtitleSet, func(err error) { a.log.Warn("track preferences unavailable", "err", err) })
		},
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
		MediaLookup: func(ctx context.Context, v *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return cachedSessionTitle(ctx, v, key, a.catalog.CachedGet, visibility)
		},
		Playback: a.playback, Transcoder: tc, Progress: a.state, SourceHistory: a.state,
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

// cachedSessionTitle checks both the item and inherited show using only the
// local mirror. Missing facts fail closed; it never refreshes remote metadata.
func cachedSessionTitle(ctx context.Context, viewer *core.Viewer, key core.RatingKey, cached func(context.Context, core.RatingKey) (*core.Title, error), visibility experience.Visibility) (*core.Title, error) {
	title, err := cached(ctx, key)
	if err != nil {
		return nil, err
	}
	visibility.GetTitle = cached
	if title == nil || title.Key != key || !visibility.Playable(ctx, viewer, title) {
		return nil, core.ErrNotFound
	}
	return title, nil
}

// subscribeSessionState publishes protocol-neutral committed state invalidations.
func subscribeSessionState(watch *state.State, events *sessions.EventBus, onError func(error)) func() {
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
	})
}
