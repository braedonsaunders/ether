package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type experienceSituationKey struct{}

func situationFor(req frontend.RowRequest) core.HomeSituation {
	h := core.NewHubContext(time.Now().Local(), req.Device.Product, req.Device.Platform, req.Device.DeviceName)
	h.ClientID = req.Device.ID
	h.SectionID, _ = strconv.Atoi(req.ViewID)
	return core.HomeSituation{Device: h.Device, Daypart: h.Daypart, Weekend: h.Weekend, Now: h.Now, LibraryID: h.SectionID, Product: h.Product, Platform: h.Platform, ClientID: h.ClientID, SessionID: h.SessionID}
}

// configuredLibraries is the live Movies / TV Shows pair. IDs stay 1 and 2 so
// saved ParentId and section UUIDs do not change when the owner renames them.
func (a *App) configuredLibraries() []experience.Library {
	movies, shows := "Movies", "TV Shows"
	if a.cfg != nil {
		c := a.cfg.Get()
		if n := strings.TrimSpace(c.Server.MoviesLibraryName); n != "" {
			movies = n
		}
		if n := strings.TrimSpace(c.Server.TVShowsLibraryName); n != "" {
			shows = n
		}
	}
	return []experience.Library{{ID: 1, Name: movies, Type: core.TypeMovie, Virtual: true}, {ID: 2, Name: shows, Type: core.TypeShow, Virtual: true}}
}

// newSurfaceExperience builds an explicitly surface-bound service. A shared
// situation is inserted by surfaceExperience.Rows before ranking and receipt issuance.
func (a *App) newSurfaceExperience(protocol core.Protocol, artwork ...frontend.Artwork) (frontend.Experience, error) {
	if a.users == nil || a.catalog == nil || a.safety == nil || a.state == nil || a.personalize == nil || a.favorites == nil || a.playlists == nil || a.db == nil {
		return nil, errors.New("experience: dependencies unavailable")
	}
	libs := a.configuredLibraries()
	filter := func(ctx context.Context, ts []core.Title) []core.Title {
		if a.availability == nil {
			return ts
		}
		return a.availability.FilterDiscovery(ctx, ts)
	}
	sessions := func() []*core.PlaySession {
		if a.playback == nil {
			return nil
		}
		return a.playback.Sessions()
	}
	vis := experienceVisibility(a.safety, a.catalog.Get, filter, sessions, libs)
	source := experience.CandidateRowSource{Kind: a.personalize.SemanticRowKind, Load: func(ctx context.Context, req frontend.RowRequest) ([]experience.RankedCandidate, error) {
		return a.productionExperienceRows(ctx, req, libs, vis)
	}, Additional: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		return a.experienceAdditionalRows(ctx, req, a.playlists, vis)
	}}
	entryPolicy := experience.RowEntryPolicy{Visibility: vis, Favorite: func(ctx context.Context, u *core.User, subject experience.FavoriteSubject) bool {
		return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.Kind(subject.Kind), ID: subject.ID}, vis)
	}, Playlist: func(ctx context.Context, u *core.User, key core.RatingKey) bool {
		_, err := a.playlists.Get(ctx, u.ID, key)
		return err == nil
	}}
	interactions := experience.RowInteractionPolicy{Libraries: libs, Visibility: vis, Get: a.catalog.Get, AllowEntry: entryPolicy.Subject, CollectionMembers: func(ctx context.Context, u *core.User, key core.RatingKey, entries []core.NeutralEntryRef) bool {
		return rowViewCollectionMembers(ctx, a.collections, u, key, entries)
	}, Impression: a.personalize.RecordNeutralImpression, DetailOpen: a.personalize.RecordDetailOpen}
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: a.db, PolicyLease: a.cfg.AcquirePolicy, Active: a.users.PolicyUser, TTL: 30 * time.Minute, MaxPerDevice: 10000, Allowed: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject) bool {
		return interactions.Allowed(ctx, u, subject)
	}, AllowedAction: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) bool {
		return (kind == frontend.RowImpression && subject.Kind == "row" && subject.Impression != nil) || (kind == frontend.RowClick && subject.Kind == string(frontend.RowEntryTitle))
	}, Record: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) error {
		return interactions.Record(ctx, u, subject, kind)
	}})
	if err != nil {
		return nil, err
	}
	rows, err := experience.NewRowsService(experience.RowServiceDeps{Source: source, Visibility: vis, Receipts: receipts, RankedSource: true, HomeFallback: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		return a.experienceHomeFallback(ctx, req, libs, vis)
	}, Active: func(ctx context.Context, u *core.User) bool {
		v, e := a.users.ActiveUser(ctx, u.ID)
		return e == nil && v != nil
	}, AllowRow: entryPolicy.Row, AllowEntry: entryPolicy.Entry, Layout: func(ctx context.Context, req frontend.RowRequest, rows []frontend.Row) ([]frontend.Row, error) {
		sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return nil, errors.New("experience: missing situation")
		}
		return experience.LayoutRows(ctx, a.personalize, libs, req, sit, rows)
	}, Situation: func(ctx context.Context, req frontend.RowRequest) (core.HomeSituation, error) {
		v, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return core.HomeSituation{}, errors.New("experience: missing situation")
		}
		return v, nil
	}})
	if err != nil {
		return nil, err
	}
	rowViews, err := a.newRowViews(protocol, rows, receipts, vis, libs)
	if err != nil {
		return nil, err
	}
	deps := experience.ServiceDeps{BeginPeopleRead: a.beginPeopleRead, NotInterestedSource: func(ctx context.Context, u *core.User) ([]frontend.NotInterestedEntry, error) {
		entries, _, err := a.notInterested.Snapshot(ctx, u)
		if err != nil {
			return nil, err
		}
		out := make([]frontend.NotInterestedEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, frontend.NotInterestedEntry{Key: entry.Key, AddedAt: entry.AddedAt})
		}
		return out, nil
	}, ByExternalID: a.catalog.ByExternalID, Active: a.users.PolicyUser, Libraries: libs, LiveLibraries: a.configuredLibraries, Visibility: vis, State: a.state, Rows: rows, Protocol: protocol, Browse: a.catalog.Browse, CreditItems: a.catalog.CachedCreditItems, AiredEpisodes: a.catalog.CachedAiredEpisodes, AddedTimes: a.catalog.CachedAddedTimes, SimilarSource: a.catalog.Similar, FilterRecommendations: a.personalize.FilterRecommendations, Collections: a.collections, Playlists: a.playlists, Search: experience.Search{Catalog: a.catalog.Search}, CachedGet: a.catalog.CachedGet, CachedGetMany: a.catalog.CachedGetMany, ParentTitles: func(ctx context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return a.catalog.ResolveShowParents(ctx, keys)
	}, Get: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Get(ctx, u, k)
		}
		return a.catalog.Get(ctx, k)
	}, CachedVirtualSeason: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
		if a.episodeorder == nil {
			return nil, core.ErrNotFound
		}
		return a.episodeorder.CachedVirtualSeason(ctx, u, k)
	}, Children: func(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Children(ctx, u, k)
		}
		return a.catalog.Children(ctx, k)
	}, Grandchildren: func(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
		if a.episodeorder != nil {
			return a.episodeorder.Grandchildren(ctx, u, k)
		}
		return a.catalog.Grandchildren(ctx, k)
	}, Enrich: func(ctx context.Context, u *core.User, device core.Device, title *core.Title) (frontend.ItemDetail, error) {
		result := frontend.ItemDetail{Title: *title}
		if title.PosterPath != "" {
			result.Images = append(result.Images, frontend.Image{Kind: "poster", Path: title.PosterPath})
		}
		if title.BackdropPath != "" {
			result.Images = append(result.Images, frontend.Image{Kind: "backdrop", Path: title.BackdropPath})
		}
		// The complete overlays-aware list is not yet available, so image grants remain disabled.
		if a.playback != nil {
			caps := core.ClientCaps{Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, ClientID: device.ID, Version: device.Version}
			info := a.playback.Estimate(ctx, title, caps)
			markerInfo := experience.MarkerMedia(u, device, title.Key, info, a.playback.SessionFor)
			if a.markers != nil {
				result.Markers = experience.ItemMarkers(ctx, a.markers, title.Key, markerInfo)
			} else {
				result.Markers = experience.ItemMarkers(ctx, nil, title.Key, markerInfo)
			}
			if a.subtitles != nil {
				info.Streams = append(info.Streams, (plexSubtitles{a}).CachedSidecars(ctx, title, u, info.Streams)...)
			}
			result.Tracks = info.Streams
			result.TracksEstimated = info.Estimated
		}
		if a.resolver != nil {
			caps := core.ClientCaps{Product: device.Product, Platform: device.Platform, DeviceName: device.DeviceName, ClientID: device.ID, Version: device.Version}
			versions, err := scorecardVariants(ctx, a, u, title, caps)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			for _, version := range versions {
				result.Versions = append(result.Versions, frontendVersion(title, version))
			}
			if len(versions) > 0 {
				ledger, e := experience.NewFirstObservedLedger(a.db)
				if e == nil {
					e = observeVersionOffers(ctx, ledger, title.Key, versions, result.Versions, time.Now())
				}
				if e != nil {
					a.log.Warn("version observation unavailable", "err", e)
				}
			}
		}
		if a.extras != nil {
			clips, err := a.extras.List(ctx, title, device.Language)
			if err != nil {
				return frontend.ItemDetail{}, err
			}
			for _, clip := range clips {
				result.Extras = append(result.Extras, frontend.Extra{Key: clip.Key, ParentKey: clip.ParentKey, Title: clip.Title, Kind: clip.Subtype, Source: clip.Site, SourceID: clip.SiteKey, Language: clip.Language, Official: clip.Official, PublishedAt: clip.PublishedAt, Duration: clip.Duration, Height: clip.Size})
			}
		}
		return result, nil
	}, Favorites: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) (bool, error) {
		return a.favorites.Set(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: k.String()}, on)
	}, IsFavorite: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.favorites.Has(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: k.String()})
	}, IsFavoriteMany: func(ctx context.Context, u *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		return a.favorites.HasTitles(ctx, u, keys)
	}, FavoriteMembership: a.favoriteMembership, IsNotInterested: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.notInterested.Has(ctx, u, k)
	}, SetNotInterestedSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		_, err := a.notInterested.Set(ctx, u, k, on)
		return err
	}, IsWatchlisted: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		if a.watchlist == nil {
			return false, core.ErrNotAvailable
		}
		return a.watchlist.Contains(ctx, u.ID, k)
	}, WatchlistMembership: a.watchlistMembership, SetWatchlistSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		if a.watchlist == nil {
			return core.ErrNotAvailable
		}
		var err error
		if on {
			err = a.watchlist.Remind(ctx, u, k)
		} else {
			err = a.watchlist.Remove(ctx, u.ID, k)
		}
		if err == nil && a.sessionEvents != nil {
			if e := a.sessionEvents.Publish(ctx, frontend.Event{Type: "UserDataChanged", ViewerID: u.ID, Item: k, OriginProtocol: protocol}); e != nil && a.log != nil {
				a.log.Warn("watchlist event publication failed", "err", e)
			}
		}
		return err
	}, RemoveContinue: func(ctx context.Context, u *core.User, k core.RatingKey) error {
		if a.discoveryPersonal != nil {
			if err := a.discoveryPersonal.RemoveContinue(ctx, u.ID, k); err != nil {
				return err
			}
		}
		return a.state.Progress(ctx, u.ID, k, 0, 0, "stopped")
	}, CanFavoriteEntity: func(ctx context.Context, u *core.User, r frontend.FavoriteRef) bool {
		return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID}, vis)
	}, SetEntityFavoriteSource: func(ctx context.Context, u *core.User, r frontend.FavoriteRef, on bool) error {
		_, err := a.favorites.Set(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID}, on)
		return err
	}, EntityFavoritesSource: func(ctx context.Context, u *core.User, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
		out := make(map[frontend.FavoriteRef]bool, len(refs))
		for _, r := range refs {
			yes, err := a.favorites.Has(ctx, u, favorites.Subject{Kind: favorites.Kind(r.Kind), ID: r.ID})
			if err != nil {
				return nil, err
			}
			out[r] = yes
		}
		return out, nil
	}}
	service, err := experience.NewService(deps)
	if err != nil {
		return nil, err
	}
	var artworkSource frontend.Artwork
	if len(artwork) > 0 {
		artworkSource = artwork[0]
	}
	return &surfaceExperience{Service: service, receipts: receipts, protocol: protocol, artwork: artworkSource, active: a.users.ActiveUser, rowViews: rowViews}, nil
}

type surfaceExperience struct {
	*experience.Service
	receipts    *experience.Receipts
	protocol    core.Protocol
	artwork     frontend.Artwork
	active      func(context.Context, int64) (*core.User, error)
	rowViews    *experience.RowViewService
	playability func(context.Context, frontend.ItemRequest) *bool
}

func (s *surfaceExperience) ListRowViews(ctx context.Context, req frontend.RowViewRequest) ([]frontend.RowView, error) {
	if req.Protocol != s.protocol || s.rowViews == nil {
		return nil, core.ErrUnauthorized
	}
	return s.rowViews.ListRowViews(ctx, req)
}

func (s *surfaceExperience) ResolveRowView(ctx context.Context, req frontend.RowViewPageRequest) (frontend.RowViewPage, error) {
	if req.Protocol != s.protocol || s.rowViews == nil {
		return frontend.RowViewPage{}, core.ErrUnauthorized
	}
	return s.rowViews.ResolveRowView(ctx, req)
}

func (s *surfaceExperience) ItemArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
	active, err := s.Service.AuthorizeArtwork(ctx, viewer, device, key)
	if err != nil {
		return nil, false, err
	}
	if s.artwork == nil {
		return nil, false, core.ErrNotAvailable
	}
	return s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: device, Item: key})
}

func (s *surfaceExperience) RelatedArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (frontend.RelatedArtwork, error) {
	service, err := experience.NewRelatedArtworkService(experience.RelatedArtworkDeps{Authorize: s.Service.AuthorizeArtwork, List: func(ctx context.Context, active *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
		if s.artwork == nil {
			return nil, false, core.ErrNotAvailable
		}
		return s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: device, Item: key})
	}})
	if err != nil {
		return frontend.RelatedArtwork{}, err
	}
	return service.RelatedArtwork(ctx, viewer, device, key)
}

func (s *surfaceExperience) Item(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	detail, err := s.Service.Item(ctx, req)
	if err == nil && detail.Title.Key != 0 && s.playability != nil {
		if detail.Title.Type == core.TypeShow || detail.Title.Type == core.TypeSeason {
			playable := false
			detail.CanPlay = &playable
		} else {
			detail.CanPlay = s.playability(ctx, req)
		}
	}
	if err != nil || s.artwork == nil {
		return detail, err
	}
	// The service has already authorized the item. Resolve the active user
	// again before passing it to the artwork policy; do not trust caller fields.
	detail.Images = nil
	detail.ImagesAuthoritative = false
	if req.Viewer == nil {
		return detail, nil
	}
	viewer, err := s.active(ctx, req.Viewer.ID)
	if err != nil || viewer == nil {
		return detail, nil
	}
	images, complete, err := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: req.Device, Item: req.Key})
	if err == nil {
		detail.Images, detail.ImagesAuthoritative = images, complete
	}
	return detail, nil
}

func (s *surfaceExperience) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	rows, err := s.Service.Rows(context.WithValue(ctx, experienceSituationKey{}, situationFor(req)), req)
	if err == nil {
		var active *core.User
		if req.Viewer != nil {
			active, _ = s.active(ctx, req.Viewer.ID)
		}
		for _, row := range rows {
			s.warmArtwork(row.Items, req.Device)
			for _, entry := range row.Entries {
				if entry.Title != nil {
					s.warmArtwork([]core.Title{*entry.Title}, req.Device)
				}
				if channel := entry.LiveChannel; channel != nil {
					// An upstream logo may carry provider credentials. Only return
					// a policy-scoped descriptor to non-Plex front ends.
					channel.Logo = ""
					if active != nil && s.artwork != nil {
						images, complete, e := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: active, Device: req.Device, Channel: channel.ID})
						if e == nil && complete {
							channel.Images = images
						}
					}
				}
			}
		}
	}
	return rows, err
}

func (s *surfaceExperience) warmArtwork(items []core.Title, device core.Device) {
	warmer, ok := s.artwork.(interface{ Warm(core.Title, core.Device) })
	if !ok {
		return
	}
	for _, item := range items {
		warmer.Warm(item, device)
	}
}

func (s *surfaceExperience) Children(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, query frontend.ItemQuery) (core.Page, error) {
	page, err := s.Service.Children(ctx, viewer, device, key, query)
	if err == nil {
		s.warmArtwork(page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) LibraryItems(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, query frontend.ItemQuery) (core.Page, error) {
	page, err := s.Service.LibraryItems(ctx, viewer, device, viewID, query)
	if err == nil {
		s.warmArtwork(page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) Search(ctx context.Context, viewer *core.Viewer, device core.Device, query string, options frontend.ItemQuery) (frontend.SearchPage, error) {
	page, err := s.Service.Search(ctx, viewer, device, query, options)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}

func (s *surfaceExperience) Similar(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, query frontend.ItemQuery) (frontend.SearchPage, error) {
	page, err := s.Service.Similar(ctx, viewer, device, key, query)
	if err == nil {
		s.warmArtwork(page.Page.Items, device)
	}
	return page, err
}
func (s *surfaceExperience) ReportRowInteraction(ctx context.Context, i frontend.RowInteraction) error {
	if i.Protocol != s.protocol {
		return experience.ErrReceipt
	}
	return s.receipts.Report(ctx, i)
}
