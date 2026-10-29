package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// experienceHubRows converts trusted recommender candidates to neutral rows;
// wire contexts stay on the producer side. Authorization and pagination are
// performed by experience.RowsService before returning these candidates.
func experienceHubRows(hubs []core.Hub) []frontend.Row {
	out := make([]frontend.Row, 0, len(hubs))
	for _, h := range hubs {
		purpose := frontend.RowRecommendation
		id := strings.ToLower(h.ID)
		switch {
		case strings.Contains(id, "continue") || strings.Contains(id, "inprogress"):
			purpose = frontend.RowContinue
		case strings.Contains(id, "upnext") || strings.Contains(id, "ondeck"):
			purpose = frontend.RowNextUp
		case strings.Contains(id, "watchlist"):
			purpose = frontend.RowWatchlist
		case h.Key != 0 || strings.Contains(id, "collection"):
			purpose = frontend.RowCollection
		case strings.Contains(id, "recentlyreleased"):
			purpose = frontend.RowRecentlyReleased
		case strings.Contains(id, "livetv"):
			purpose = frontend.RowLive
		}
		row := frontend.Row{ID: h.ID, Title: h.Title, Purpose: purpose, Style: h.Style, Promoted: h.Promoted, More: h.More, Collection: h.Key}
		for i := range h.Items {
			item := h.Items[i]
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
		}
		out = append(out, row)
	}
	return out
}

type experienceRowSource struct {
	load       func(context.Context, frontend.RowRequest) ([]core.Hub, error)
	additional func(context.Context, frontend.RowRequest) ([]frontend.Row, error)
	kind       func(string) string
}

func (s experienceRowSource) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	hubs, err := s.load(ctx, req)
	if err != nil {
		return nil, err
	}
	rows := experienceHubRows(hubs)
	if s.additional != nil {
		extra, err := s.additional(ctx, req)
		if err != nil {
			return nil, err
		}
		rows = append(rows, extra...)
	}
	for i := range rows {
		if s.kind != nil {
			rows[i].Kind = s.kind(rows[i].ID)
		}
		rows[i].Rank = i
		rows[i].Layout.Style = rows[i].Style
	}
	return rows, nil
}

// productionExperienceRows obtains real ranked candidate hubs from the shared
// personalization engine. Protocol adapters must not call the engine directly.
func (a *App) productionExperienceRows(ctx context.Context, req frontend.RowRequest, libraries []experience.Library, visibility experience.Visibility) ([]core.Hub, error) {
	if req.Viewer == nil || a.personalize == nil {
		return nil, core.ErrNotFound
	}
	sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
	if !ok {
		return nil, core.ErrNotFound
	}
	hc := core.HubContext{Device: sit.Device, Daypart: sit.Daypart, Weekend: sit.Weekend, Now: sit.Now, SectionID: sit.LibraryID, Product: sit.Product, Platform: sit.Platform, SessionID: sit.SessionID, ClientID: sit.ClientID}
	restricted := !req.Viewer.IsOwner && (req.Viewer.MaxContentRating != "" || len(req.Viewer.AllowedSections) > 0 || a.safety.Restricted(req.Viewer))
	allow := func(ctx context.Context, u *core.User, t *core.Title) bool { return visibility.Visible(ctx, u, t) }
	switch req.Placement {
	case frontend.PlacementHome:
		var hubs []core.Hub
		var err error
		if restricted {
			hubs, err = a.personalize.HomeCandidatesWithPolicy(ctx, req.Viewer, hc, req.Limit, allow)
		} else {
			hubs, err = a.personalize.HomeCandidatesFor(ctx, req.Viewer, hc, req.Limit)
		}
		if err != nil {
			return nil, err
		}
		resume, err := a.experienceContinue(ctx, req.Viewer, libraries, req.Limit)
		if err != nil {
			return nil, err
		}
		inserted := false
		for i := range hubs {
			if hubs[i].ID == "home.continue" {
				hubs[i].Items = resume
				inserted = true
			}
		}
		if !inserted {
			hubs = append([]core.Hub{{ID: "home.continue", Title: "Continue Watching", Style: "hero", Promoted: true, Items: resume}}, hubs...)
		}
		return hubs, nil
	case frontend.PlacementLibrary:
		id, err := strconv.Atoi(req.ViewID)
		if err != nil {
			return nil, core.ErrNotFound
		}
		var kind core.MediaType
		for _, library := range libraries {
			if library.ID == id {
				kind = library.Type
				break
			}
		}
		if kind == "" {
			return nil, core.ErrNotFound
		}
		if !experience.SectionAllowed(req.Viewer, id) {
			return nil, core.ErrNotFound
		}
		hc.SectionID = id
		if restricted {
			return a.personalize.SectionCandidatesWithPolicy(ctx, req.Viewer, kind, hc, req.Limit, allow)
		}
		return a.personalize.SectionCandidatesFor(ctx, req.Viewer, kind, hc, req.Limit)
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
		return experienceDetailRows(ctx, req.Viewer, req.Item, req.Limit, visibility, a.catalog.NextEpisode, postPlay,
			func(ctx context.Context, limit int) ([]core.Hub, error) {
				return a.personalize.Related(ctx, req.Viewer, req.Item, limit)
			})
	default:
		return nil, core.ErrNotFound
	}
}

// experienceDetailRows assembles the same ordered sources used by post-play,
// filtering each candidate before deduplication and row limits.
func experienceDetailRows(ctx context.Context, viewer *core.User, key core.RatingKey, limit int, visibility experience.Visibility,
	next func(context.Context, core.RatingKey) (*core.Title, error), postPlay func(context.Context, int) []core.PostPlayPick,
	related func(context.Context, int) ([]core.Hub, error)) ([]core.Hub, error) {
	if limit <= 0 {
		limit = 12
	}
	seen := map[core.RatingKey]bool{key: true}
	var hubs []core.Hub
	if next != nil {
		row := experience.NextUp(ctx, viewer, key, next, visibility)
		if len(row.Items) > 0 && !seen[row.Items[0].Key] {
			seen[row.Items[0].Key] = true
			hubs = append(hubs, core.Hub{ID: "tv.upnext", Title: "Up Next", Type: core.TypeEpisode, Items: row.Items})
		}
	}
	if postPlay != nil {
		row := experience.LoadPostPlay(ctx, viewer, visibility, limit, seen, postPlay)
		if len(row.Items) > 0 {
			prefix := "movie"
			if key.Kind() == core.KindShow || key.Kind() == core.KindSeason || key.Kind() == core.KindEpisode {
				prefix = "tv"
			}
			hubs = append(hubs, core.Hub{ID: prefix + ".postplay", Title: "Recommended For You", Style: "shelf", Items: row.Items})
		}
	}
	if related == nil {
		return hubs, nil
	}
	var candidates []core.Hub
	rows, err := experience.LoadRelated(ctx, viewer, visibility, limit, seen, func(ctx context.Context, fetch int) ([]experience.RelatedCandidate, error) {
		var err error
		candidates, err = related(ctx, fetch)
		if err != nil {
			return nil, err
		}
		out := make([]experience.RelatedCandidate, len(candidates))
		for i, h := range candidates {
			out[i] = experience.RelatedCandidate{Index: i, Items: h.Items}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		hub := candidates[row.Index]
		hub.Items = row.Items
		hubs = append(hubs, hub)
	}
	return hubs, nil
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
	visibility := experienceVisibility(a.safety, a.catalog.Get, filter, sessions, libraries)
	getTitles := func(ctx context.Context, keys []core.RatingKey) []core.Title {
		out := make([]core.Title, 0, len(keys))
		for _, key := range keys {
			if t, err := a.catalog.Get(ctx, key); err == nil && t != nil {
				out = append(out, *t)
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
	c := experience.Continue{State: a.state, Now: time.Now, FavoriteShows: a.favoriteShows, GetTitles: getTitles, Grandchildren: grandchildren, NextEpisode: a.catalog.NextEpisode, Visible: visibility.Visible,
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
	items, _ := c.InProgress(ctx, viewer, "", limit)
	if a.discoveryPersonal != nil {
		items = a.discoveryPersonal.FilterContinue(ctx, viewer, items)
	}
	return items, loadErr
}

// experienceAdditionalRows supplies the viewer's real playlist, watchlist and
// live rows; the shared row service applies policy before delivery.
func (a *App) experienceAdditionalRows(ctx context.Context, req frontend.RowRequest, playlists core.Playlists) ([]frontend.Row, error) {
	if req.Placement != frontend.PlacementHome || req.Viewer == nil {
		return nil, nil
	}
	var rows []frontend.Row
	if playlists != nil {
		entries, more, err := experience.PlaylistEntries(ctx, req.Viewer, playlists, req.Limit)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "playlists", Title: "Recent Playlists", Purpose: core.RowPurpose("playlist"), Entries: entries, More: more})
		}
	}
	if a.watchlist != nil {
		hubs, err := a.watchlist.Hubs(ctx, req.Viewer, req.Limit)
		if err != nil {
			return nil, err
		}
		rows = append(rows, experienceHubRows(hubs)...)
	}
	if a.discoveryPersonal != nil {
		hubs, err := a.discoveryPersonal.Hubs(ctx, req.Viewer, req.Limit)
		if err != nil {
			return nil, err
		}
		rows = append(rows, experienceHubRows(hubs)...)
	}
	if a.livetv != nil {
		now := time.Now()
		entries, err := experience.LiveEntries(ctx, req.Viewer, a.livetv, now, now.Add(24*time.Hour), nil)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "live", Title: "On Now", Purpose: frontend.RowLive, Entries: entries})
		}
	}
	return rows, nil
}
