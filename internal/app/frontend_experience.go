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

// newSurfaceExperience builds an explicitly surface-bound service. A shared
// situation is inserted by surfaceExperience.Rows before ranking and receipt issuance.
func (a *App) newSurfaceExperience(protocol core.Protocol, artwork ...frontend.Artwork) (frontend.Experience, error) {
	if a.users == nil || a.catalog == nil || a.safety == nil || a.state == nil || a.personalize == nil || a.favorites == nil || a.playlists == nil || a.db == nil {
		return nil, errors.New("experience: dependencies unavailable")
	}
	libs := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}}
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
	source := experienceRowSource{kind: a.personalize.SemanticRowKind, load: func(ctx context.Context, req frontend.RowRequest) ([]core.Hub, error) {
		return a.productionExperienceRows(ctx, req, libs, vis)
	}, additional: func(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
		return a.experienceAdditionalRows(ctx, req, a.playlists, vis)
	}}
	allowEntry := func(ctx context.Context, u *core.User, kind, id string) bool {
		switch frontend.RowEntryKind(kind) {
		case frontend.RowEntryTitle:
			return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: id}, vis)
		case frontend.RowEntryCollection:
			return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindCollection, ID: id}, vis)
		case frontend.RowEntryPlaylist:
			k, e := core.ParseRatingKey(id)
			if e != nil {
				return false
			}
			_, e = a.playlists.Get(ctx, u.ID, k)
			return e == nil
		case frontend.RowEntryLiveChannel:
			return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindChannel, ID: id}, vis)
		default:
			return false
		}
	}
	allowedSubject := func(ctx context.Context, u *core.User, subject experience.ReceiptSubject) bool {
		if imp := subject.Impression; imp != nil {
			if len(imp.Entries) == 0 || imp.Kind == "" {
				return false
			}
			if imp.Collection != 0 && !a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindCollection, ID: imp.Collection.String()}, vis) {
				return false
			}
			if imp.Collection != 0 && !rowViewCollectionMembers(ctx, a.collections, u, imp.Collection, imp.Entries) {
				return false
			}
			switch frontend.Placement(imp.Placement) {
			case frontend.PlacementHome:
			case frontend.PlacementLibrary:
				id, err := strconv.Atoi(imp.ViewID)
				valid := false
				for _, lib := range libs {
					if lib.ID == id {
						valid = true
						break
					}
				}
				if err != nil || !valid || !experience.SectionAllowed(u, id) {
					return false
				}
			case frontend.PlacementDetail:
				if imp.Item == 0 {
					return false
				}
				t, err := a.catalog.Get(ctx, imp.Item)
				if err != nil || !vis.Visible(ctx, u, t) {
					return false
				}
			default:
				return false
			}
			for _, entry := range imp.Entries {
				if !allowEntry(ctx, u, entry.Kind, entry.ID) {
					return false
				}
			}
			if subject.Kind == "row" {
				return true
			}
			i := strings.LastIndex(subject.ID, "/"+subject.Kind+"/")
			if i < 0 {
				return false
			}
			id := subject.ID[i+len(subject.Kind)+2:]
			for _, entry := range imp.Entries {
				if entry.Kind == subject.Kind && entry.ID == id {
					return true
				}
			}
			return false
		}
		return false
	}
	receipts, err := experience.NewReceipts(experience.ReceiptOptions{DB: a.db, Active: a.users.ActiveUser, TTL: 30 * time.Minute, MaxPerDevice: 10000, Allowed: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject) bool {
		return allowedSubject(ctx, u, subject)
	}, AllowedAction: func(_ context.Context, _ *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) bool {
		return (kind == frontend.RowImpression && subject.Kind == "row" && subject.Impression != nil) || (kind == frontend.RowClick && subject.Kind == string(frontend.RowEntryTitle))
	}, Record: func(ctx context.Context, u *core.User, _ core.Device, _ core.Protocol, subject experience.ReceiptSubject, kind frontend.RowInteractionKind) error {
		if kind == frontend.RowImpression && subject.Impression != nil {
			a.personalize.RecordNeutralImpression(u, *subject.Impression)
			return nil
		}
		if kind == frontend.RowClick && subject.Kind == string(frontend.RowEntryTitle) {
			idx := strings.LastIndex(subject.ID, "/title/")
			if idx < 0 {
				return experience.ErrReceipt
			}
			key, err := core.ParseRatingKey(subject.ID[idx+len("/title/"):])
			if err != nil {
				return err
			}
			title, err := a.catalog.Get(ctx, key)
			if err != nil || !vis.Visible(ctx, u, title) {
				return experience.ErrReceipt
			}
			a.personalize.RecordDetailOpen(ctx, u, title)
			return nil
		}
		return errors.New("experience: unsupported row interaction")
	}})
	if err != nil {
		return nil, err
	}
	rows, err := experience.NewRowsService(experience.RowServiceDeps{Source: source, Visibility: vis, Receipts: receipts, RankedSource: true, Active: func(ctx context.Context, u *core.User) bool {
		v, e := a.users.ActiveUser(ctx, u.ID)
		return e == nil && v != nil
	}, AllowRow: func(ctx context.Context, u *core.User, row frontend.Row) bool {
		if row.Collection != 0 {
			return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindCollection, ID: row.Collection.String()}, vis)
		}
		return true
	}, AllowEntry: func(ctx context.Context, u *core.User, e frontend.RowEntry) bool {
		switch e.Kind {
		case frontend.RowEntryTitle:
			return e.Title != nil && vis.Visible(ctx, u, e.Title)
		case frontend.RowEntryCollection:
			return e.Collection != nil && a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindCollection, ID: e.Collection.Key.String()}, vis)
		case frontend.RowEntryPlaylist:
			if e.Playlist == nil {
				return false
			}
			_, err := a.playlists.Get(ctx, u.ID, e.Playlist.Key)
			return err == nil
		case frontend.RowEntryLiveChannel:
			return e.LiveChannel != nil && a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindChannel, ID: e.LiveChannel.ID}, vis)
		}
		return false
	}, Layout: func(ctx context.Context, req frontend.RowRequest, rows []frontend.Row) ([]frontend.Row, error) {
		sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
		if !ok {
			return nil, errors.New("experience: missing situation")
		}
		section := "home"
		if req.Placement == frontend.PlacementLibrary {
			section = "movies"
			for _, l := range libs {
				if strconv.Itoa(l.ID) == req.ViewID && l.Type == core.TypeShow {
					section = "shows"
				}
			}
		}
		if req.Placement == frontend.PlacementDetail {
			return rows, nil
		}
		candidates := make([]core.NeutralHomeRow, 0, len(rows))
		byID := make(map[string]frontend.Row, len(rows))
		for _, row := range rows {
			n := core.NeutralHomeRow{ID: row.ID, Title: row.Title, Kind: row.Kind, Purpose: row.Purpose, ItemCount: len(row.Entries)}
			for _, entry := range row.Entries {
				if entry.Title != nil {
					n.Items = append(n.Items, *entry.Title)
					if n.Type == "" {
						n.Type = entry.Title.Type
					}
				}
			}
			candidates = append(candidates, n)
			byID[row.ID] = row
		}
		ordered, err := a.personalize.FinalizeNeutralHome(ctx, req.Viewer, sit, section, candidates)
		if err != nil {
			return nil, err
		}
		out := make([]frontend.Row, 0, len(ordered))
		for i, n := range ordered {
			row := byID[n.ID]
			row.Rank = i
			row.Layout.Order = i
			row.Layout.Style = row.Style
			out = append(out, row)
		}
		return out, nil
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
	deps := experience.ServiceDeps{NotInterestedSource: func(ctx context.Context, u *core.User) ([]frontend.NotInterestedEntry, error) {
		entries, _, err := a.notInterested.Snapshot(ctx, u)
		if err != nil {
			return nil, err
		}
		out := make([]frontend.NotInterestedEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, frontend.NotInterestedEntry{Key: entry.Key, AddedAt: entry.AddedAt})
		}
		return out, nil
	}, ByExternalID: a.catalog.ByExternalID, Active: a.users.ActiveUser, Libraries: libs, Visibility: vis, State: a.state, Rows: rows, Protocol: protocol, Browse: a.catalog.Browse, SimilarSource: a.catalog.Similar, FilterRecommendations: a.personalize.FilterRecommendations, Collections: a.collections, Playlists: a.playlists, Search: experience.Search{Catalog: a.catalog.Search}, CachedGet: a.catalog.CachedGet, Get: func(ctx context.Context, u *core.User, k core.RatingKey) (*core.Title, error) {
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
			versions, err := a.resolver.Variants(ctx, core.ResolveRequest{Title: title, User: u, Client: caps}, 4)
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
	}, IsNotInterested: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		return a.notInterested.Has(ctx, u, k)
	}, SetNotInterestedSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
		_, err := a.notInterested.Set(ctx, u, k, on)
		return err
	}, IsWatchlisted: func(ctx context.Context, u *core.User, k core.RatingKey) (bool, error) {
		if a.watchlist == nil {
			return false, core.ErrNotAvailable
		}
		return a.watchlist.Contains(ctx, u.ID, k)
	}, SetWatchlistSource: func(ctx context.Context, u *core.User, k core.RatingKey, on bool) error {
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

// versionAddedAt prefers the actual release publication time. When absent,
// use Plex's title AddedAt rule: UpdatedAt, then its original release date.
func versionAddedAt(title *core.Title, published time.Time) time.Time {
	if !published.IsZero() {
		return published
	}
	if title == nil {
		return time.Time{}
	}
	if !title.UpdatedAt.IsZero() {
		return title.UpdatedAt
	}
	return title.Released
}
