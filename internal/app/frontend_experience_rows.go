package app

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// appCandidateRanker translates the neutral situation into the existing
// personalization engine's request context without reassembling ranked rows.
type appCandidateRanker struct{ app *App }

func candidateHubContext(sit core.HomeSituation) core.HubContext {
	return core.HubContext{Device: sit.Device, Daypart: sit.Daypart, Weekend: sit.Weekend, Now: sit.Now, SectionID: sit.LibraryID, Product: sit.Product, Platform: sit.Platform, SessionID: sit.SessionID, ClientID: sit.ClientID}
}
func rankedCandidates(hubs []core.Hub, err error) ([]experience.RankedCandidate, error) {
	if err != nil {
		return nil, err
	}
	if hubs == nil {
		return nil, nil
	}
	out := make([]experience.RankedCandidate, len(hubs))
	for i, h := range hubs {
		out[i] = experience.RankedCandidate{ID: h.ID, Purpose: experience.CandidatePurpose(h), Title: h.Title, Type: h.Type, Style: h.Style, Promoted: h.Promoted, Items: h.Items, More: h.More, Key: h.Key, ItemCount: h.ItemCount}
	}
	return out, nil
}
func (r appCandidateRanker) Home(ctx context.Context, viewer *core.User, sit core.HomeSituation, limit int, allow func(context.Context, *core.User, *core.Title) bool) ([]experience.RankedCandidate, error) {
	if allow != nil {
		return rankedCandidates(r.app.personalize.HomeCandidatesWithPolicy(ctx, viewer, candidateHubContext(sit), limit, allow))
	}
	return rankedCandidates(r.app.personalize.HomeCandidatesFor(ctx, viewer, candidateHubContext(sit), limit))
}
func (r appCandidateRanker) Library(ctx context.Context, viewer *core.User, kind core.MediaType, sit core.HomeSituation, limit int, allow func(context.Context, *core.User, *core.Title) bool) ([]experience.RankedCandidate, error) {
	if allow != nil {
		return rankedCandidates(r.app.personalize.SectionCandidatesWithPolicy(ctx, viewer, kind, candidateHubContext(sit), limit, allow))
	}
	return rankedCandidates(r.app.personalize.SectionCandidatesFor(ctx, viewer, kind, candidateHubContext(sit), limit))
}

type appResumeCandidates struct {
	app       *App
	libraries []experience.Library
}

func (r appResumeCandidates) Continue(ctx context.Context, viewer *core.User, limit int) ([]core.Title, error) {
	return r.app.experienceContinue(ctx, viewer, r.libraries, limit)
}

// productionExperienceRows obtains real ranked candidate hubs from the shared
// personalization engine. Protocol adapters must not call the engine directly.
func (a *App) productionExperienceRows(ctx context.Context, req frontend.RowRequest, libraries []experience.Library, visibility experience.Visibility) ([]experience.RankedCandidate, error) {
	if req.Viewer == nil || a.personalize == nil {
		return nil, core.ErrNotFound
	}
	if req.Purpose == frontend.RowNextUp && !a.personalize.ModelReady(req.Viewer.ID) {
		limit := req.Limit
		if limit <= 0 {
			limit = 20
		}
		items, err := a.experienceContinue(ctx, req.Viewer, libraries, limit)
		if err != nil {
			return nil, err
		}
		return []experience.RankedCandidate{{ID: "home.upnext", Purpose: frontend.RowNextUp, Items: items}}, nil
	}
	sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
	if !ok {
		return nil, core.ErrNotFound
	}
	hc := core.HubContext{Device: sit.Device, Daypart: sit.Daypart, Weekend: sit.Weekend, Now: sit.Now, SectionID: sit.LibraryID, Product: sit.Product, Platform: sit.Platform, SessionID: sit.SessionID, ClientID: sit.ClientID}
	switch req.Placement {
	case frontend.PlacementHome, frontend.PlacementLibrary:
		ranker := appCandidateRanker{app: a}
		composer := experience.CandidateComposer{Ranker: ranker, Resume: appResumeCandidates{a, libraries}, Libraries: libraries, Visibility: visibility, Restricted: a.safety.Restricted}
		return composer.Rows(ctx, req, sit)
	case frontend.PlacementDetail:
		if a.catalog == nil || req.Item == 0 {
			return nil, core.ErrNotFound
		}
		title, err := a.catalog.Get(ctx, req.Item)
		if err != nil || title == nil || !visibility.Visible(ctx, req.Viewer, title) {
			return nil, core.ErrNotFound
		}
		var postPlay func(context.Context, int) []core.PostPlayPick
		if a.cfg.Get().Personalize.PostPlay {
			postPlay = func(ctx context.Context, limit int) []core.PostPlayPick {
				picks, err := a.personalize.PostPlay(ctx, req.Viewer, req.Item, hc, limit)
				if err != nil {
					a.log.Debug("postplay picks", "key", req.Item, "err", err)
				}
				return picks
			}
		}
		return experience.DetailRows(ctx, req.Viewer, req.Item, req.Limit, visibility, a.catalog.NextEpisode, postPlay,
			func(ctx context.Context, limit int) ([]experience.RankedCandidate, error) {
				return rankedCandidates(a.personalize.Related(ctx, req.Viewer, req.Item, limit))
			})
	default:
		return nil, core.ErrNotFound
	}
}

// experienceContinue supplies the shared continue/next-up service with real app dependencies.
func (a *App) experienceContinue(ctx context.Context, viewer *core.User, libraries []experience.Library, limit int) ([]core.Title, error) {
	if a.state == nil || a.catalog == nil || a.safety == nil {
		return nil, core.ErrNotFound
	}
	var filter func(context.Context, []core.Title) []core.Title
	if a.availability != nil {
		filter = a.availability.FilterDiscovery
	}
	var sessions func() []*core.PlaySession
	if a.playback != nil {
		sessions = a.playback.Sessions
	}
	visibility := experienceVisibility(a.safety, a.catalog.CachedGet, filter, sessions, libraries)
	getTitles := func(ctx context.Context, keys []core.RatingKey) []core.Title {
		if len(keys) == 0 {
			return nil
		}
		found, err := a.catalog.CachedGetMany(ctx, keys)
		if err != nil {
			return nil
		}
		out := make([]core.Title, 0, len(found))
		seen := map[core.RatingKey]bool{}
		for _, key := range keys {
			if seen[key] {
				continue
			}
			if t, ok := found[key]; ok && t.Key == key {
				seen[key] = true
				out = append(out, t)
			}
		}
		return out
	}
	grandchildren := func(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Grandchildren(ctx, viewer, key)
		}
		return a.catalog.Grandchildren(ctx, key)
	}
	var loadErr error
	c := experience.Continue{State: a.state, Now: time.Now, FavoriteShows: a.favoriteShows, GetTitles: getTitles, Grandchildren: grandchildren, NextEpisode: a.catalog.NextStoredEpisode, Visible: visibility.Visible,
		SameDayUnavailable: func(ctx context.Context, title *core.Title) bool {
			var source core.AvailabilitySource
			if a.resolver != nil {
				source = a.resolver
			}
			return experience.SameDayUnavailable(ctx, title, time.Now(), filter, source)
		}, SectionAllowed: func(u *core.User, kind core.MediaType) bool {
			id, ok := visibility.SectionFor(kind)
			return !ok || experience.SectionAllowed(u, id)
		},
		OnError: func(err error) { loadErr = err },
	}
	if a.episodeorder != nil {
		c.EpisodeOrder = a.episodeorder
	}
	if a.discoveryPersonal != nil {
		c.Filter = a.discoveryPersonal.FilterContinue
	}
	items, _ := c.Row(ctx, viewer, "", limit)
	return items, loadErr
}

// hubDiscoverySource translates existing engine hubs at the dependency boundary;
// renderer-specific contexts do not enter the shared composer.
type hubDiscoverySource struct {
	load func(context.Context, *core.User, int) ([]core.Hub, error)
}

func (s hubDiscoverySource) Rows(ctx context.Context, viewer *core.User, limit int) ([]experience.RankedCandidate, error) {
	return rankedCandidates(s.load(ctx, viewer, limit))
}

func (a *App) experienceAdditionalRows(ctx context.Context, req frontend.RowRequest, playlists core.Playlists, visibility experience.Visibility) ([]frontend.Row, error) {
	composer := experience.AdditionalRows{Visibility: visibility, Restricted: a.safety.Restricted, Now: time.Now}
	if playlists != nil {
		composer.Playlists = playlists
	}
	if a.watchlist != nil {
		composer.Watchlist = hubDiscoverySource{a.watchlist.Hubs}
	}
	if a.discoveryPersonal != nil {
		composer.Personal = hubDiscoverySource{a.discoveryPersonal.Hubs}
	}
	if a.livetv != nil {
		composer.Live = a.livetv
	}
	return composer.Rows(ctx, req)
}

// experienceHomeFallback loads the viewer's exclusions and asks Experience for
// the cold Home shelves. Plex does not use this path.
func (a *App) experienceHomeFallback(ctx context.Context, req frontend.RowRequest, libraries []experience.Library, visibility experience.Visibility) ([]frontend.Row, error) {
	if req.Viewer == nil || a.catalog == nil || a.notInterested == nil {
		return nil, core.ErrNotAvailable
	}
	excluded, _, err := a.notInterested.Snapshot(ctx, req.Viewer)
	if err != nil {
		return nil, err
	}
	keys := make([]core.RatingKey, 0, len(excluded))
	for _, entry := range excluded {
		keys = append(keys, entry.Key)
	}
	return experience.ColdHomeRows(ctx, req.Viewer, libraries, visibility, req.Limit, keys, a.catalog.Browse)
}
