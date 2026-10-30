package experience

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
)

// Library is a host-configured, protocol-neutral library.
type Library struct {
	ID   int
	Name string
	Type core.MediaType
	// Virtual is true only when the host asserts this library has no physical
	// locations. False means the host did not assert virtual origin.
	Virtual bool
}

// ServiceDeps contains trusted host dependencies, never provided by a frontend.
type ServiceDeps struct {
	// BeginPeopleRead supplies fresh, fenced policy facts for the optional People API.
	BeginPeopleRead     func(context.Context, *core.User) (PeopleRead, error)
	NotInterestedSource func(context.Context, *core.User) ([]frontend.NotInterestedEntry, error)
	ByExternalID        func(context.Context, string, string) (*core.Title, error)
	Active              func(context.Context, int64) (*core.User, error)
	Libraries           []Library
	// LiveLibraries, when set, is read on each surface call so configured
	// names stay current. IDs and media types stay stable. Nil uses Libraries.
	LiveLibraries func() []Library
	Visibility    Visibility
	Get           func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	// CachedGet applies catalogue visibility without a network refresh.
	CachedGet func(context.Context, core.RatingKey) (*core.Title, error)
	// CachedGetMany reads current cached rows in bounded batches without refresh.
	CachedGetMany func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	// ParentTitles resolves distinct parents using the same semantics as GetTitle.
	ParentTitles func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error)
	// CachedVirtualSeason resolves a viewer's already-computed episode layout.
	CachedVirtualSeason func(context.Context, *core.User, core.RatingKey) (*core.Title, error)
	Children            func(context.Context, *core.User, core.RatingKey) ([]core.Title, error)
	Grandchildren       func(context.Context, *core.User, core.RatingKey) ([]core.Title, error)
	Browse              func(context.Context, core.BrowseQuery) (core.Page, error)
	// CreditItems enumerates cached credit-title keys without browsing or hydration.
	CreditItems           func(context.Context, core.RatingKey, int) ([]core.RatingKey, error)
	AiredEpisodes         func(context.Context, core.RatingKey, int, int) (core.Page, error)
	AddedTimes            func(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error)
	SimilarSource         func(context.Context, core.RatingKey, int) ([]core.Title, error)
	FilterRecommendations func(context.Context, *core.User, []core.Title) ([]core.Title, error)
	Collections           core.Collections
	Playlists             core.Playlists
	State                 core.State
	Search                Search
	Rows                  *RowsService
	Protocol              core.Protocol
	// Favorites manages the stable, per-viewer Favorites playlist mapping.
	Favorites               func(context.Context, *core.User, core.RatingKey, bool) (bool, error)
	IsFavorite              func(context.Context, *core.User, core.RatingKey) (bool, error)
	IsFavoriteMany          func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error)
	FavoriteMembership      func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error)
	IsNotInterested         func(context.Context, *core.User, core.RatingKey) (bool, error)
	SetNotInterestedSource  func(context.Context, *core.User, core.RatingKey, bool) error
	IsWatchlisted           func(context.Context, *core.User, core.RatingKey) (bool, error)
	WatchlistMembership     func(context.Context, *core.User, core.RatingKey) (frontend.Membership, error)
	SetWatchlistSource      func(context.Context, *core.User, core.RatingKey, bool) error
	RemoveContinue          func(context.Context, *core.User, core.RatingKey) error
	CanFavoriteEntity       func(context.Context, *core.User, frontend.FavoriteRef) bool
	SetEntityFavoriteSource func(context.Context, *core.User, frontend.FavoriteRef, bool) error
	EntityFavoritesSource   func(context.Context, *core.User, []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error)

	// Enrich must use cached or estimated data; it must never resolve releases.
	Enrich func(context.Context, *core.User, core.Device, *core.Title) (frontend.ItemDetail, error)
}

type Service struct{ d ServiceDeps }

func NewService(d ServiceDeps) (*Service, error) {
	if d.Active == nil || d.Get == nil || d.Children == nil || d.State == nil || d.Rows == nil || d.Protocol == "" || d.Visibility.GetTitle == nil || d.Visibility.SectionFor == nil || d.Search.Catalog == nil {
		return nil, errors.New("experience: incomplete service dependencies")
	}
	d.Libraries = append([]Library(nil), d.Libraries...)
	d.Search.Visibility = d.Visibility
	return &Service{d: d}, nil
}

func (s *Service) libraries() []Library {
	if s.d.LiveLibraries != nil {
		if live := s.d.LiveLibraries(); live != nil {
			return live
		}
	}
	return s.d.Libraries
}
func (s *Service) viewer(ctx context.Context, v *core.User) (*core.User, error) {
	if v == nil || v.ID <= 0 {
		return nil, core.ErrNotFound
	}
	active, err := s.d.Active(ctx, v.ID)
	if err != nil || active == nil || active.ID != v.ID {
		return nil, core.ErrNotFound
	}
	return active, nil
}
func (s *Service) visible(ctx context.Context, v *core.User, title *core.Title) bool {
	if title == nil || s.d.Visibility.SectionFor == nil {
		return false
	}
	id, ok := s.d.Visibility.SectionFor(title.Type)
	return ok && SectionAllowed(v, id) && s.d.Visibility.Visible(ctx, v, title)
}
func (s *Service) playable(ctx context.Context, v *core.User, key core.RatingKey) (*core.Title, error) {
	t, err := s.d.Get(ctx, v, key)
	if err != nil || t == nil || t.Key != key || s.d.Visibility.SectionFor == nil {
		return nil, core.ErrNotFound
	}
	id, ok := s.d.Visibility.SectionFor(t.Type)
	if !ok || !SectionAllowed(v, id) || !s.d.Visibility.Playable(ctx, v, t) {
		return nil, core.ErrNotFound
	}
	return t, nil
}
func (s *Service) item(ctx context.Context, v *core.User, key core.RatingKey) (*core.Title, error) {
	t, err := s.d.Get(ctx, v, key)
	if err != nil || t == nil || t.Key != key || !s.visible(ctx, v, t) {
		return nil, core.ErrNotFound
	}
	return t, nil
}
func (s *Service) Views(ctx context.Context, viewer *core.Viewer) ([]frontend.View, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	libs := s.libraries()
	out := make([]frontend.View, 0, len(libs))
	for _, lib := range libs {
		if SectionAllowed(v, lib.ID) {
			out = append(out, frontend.View{ID: strconv.Itoa(lib.ID), Name: lib.Name, Type: lib.Type, Virtual: lib.Virtual})
		}
	}
	return out, nil
}

// LibraryItems scans catalogue pages and filters before applying the requested page.
func (s *Service) LibraryItems(ctx context.Context, viewer *core.Viewer, device core.Device, viewID string, q frontend.ItemQuery) (core.Page, error) {
	if q.StartAt != 0 && q.AdjacentTo != 0 {
		return core.Page{}, errors.New("experience: incompatible item anchors")
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return core.Page{}, err
	}
	if viewID == "" {
		if q.Sort == "" {
			q.Sort = core.SortPopularity
			q.Desc = true
		}
		all := make([]core.Title, 0)
		inner := q
		inner.Offset = 0
		inner.Limit = 0
		inner.AdjacentTo = 0
		inner.StartAt = 0
		inner.Sort = ""
		inner.Desc = false
		for _, lib := range s.libraries() {
			if !SectionAllowed(v, lib.ID) {
				continue
			}
			page, e := s.LibraryItems(ctx, v, device, strconv.Itoa(lib.ID), inner)
			if e != nil {
				return core.Page{}, e
			}
			all = append(all, page.Items...)
		}
		if err = s.sortTitles(ctx, all, q); err != nil {
			return core.Page{}, err
		}
		if q.AdjacentTo != 0 {
			return adjacentTitles(all, q)
		}
		if q.StartAt != 0 {
			return startingTitles(all, q)
		}
		total := len(all)
		start := min(max(q.Offset, 0), total)
		end := total
		if q.Limit > 0 {
			end = min(start+q.Limit, total)
		}
		return core.Page{Items: all[start:end], Total: total}, nil
	}
	id, err := strconv.Atoi(viewID)
	if err != nil {
		return core.Page{}, core.ErrNotFound
	}
	libs := s.libraries()
	var library *Library
	for i := range libs {
		if libs[i].ID == id {
			library = &libs[i]
			break
		}
	}
	if library == nil || !SectionAllowed(v, id) || s.d.Browse == nil {
		return core.Page{}, core.ErrNotFound
	}
	// Recursive show browsing can produce the show, its seasons, and its episodes.
	// Skip the library when the requested types cannot appear in that tree.
	if q.Recursive && library.Type == core.TypeShow && len(q.Types) > 0 {
		possible := false
		for _, typ := range q.Types {
			if typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode {
				possible = true
				break
			}
		}
		if !possible {
			if q.StartAt != 0 || q.AdjacentTo != 0 {
				return core.Page{}, core.ErrNotFound
			}
			return core.Page{Items: []core.Title{}, Total: 0}, nil
		}
	}
	wantShowChildren := q.Recursive && (len(q.Types) == 0 || containsMediaType(q.Types, core.TypeSeason))
	wantShowEpisodes := q.Recursive && (len(q.Types) == 0 || containsMediaType(q.Types, core.TypeEpisode))
	var permitted []core.Title
	totalMatched := 0
	bounded := !q.Recursive && q.AdjacentTo == 0 && q.StartAt == 0 && q.Limit > 0
	expand := q.Recursive && library.Type == core.TypeShow
	if expand && wantShowEpisodes && s.d.Grandchildren == nil {
		return core.Page{}, errors.New("experience: recursive library unavailable")
	}
	if expand && wantShowChildren && s.d.Children == nil {
		return core.Page{}, errors.New("experience: recursive library unavailable")
	}
	// Recursive episode queries keep at most one bounded batch of visible,
	// statically matching candidates in addition to the existing results.
	var recursiveBatch []core.Title
	flushRecursive := func() error {
		if len(recursiveBatch) == 0 {
			return nil
		}
		chunk := recursiveBatch
		var watched map[core.RatingKey]core.WatchStatus
		if q.Watched != nil {
			keys := make([]core.RatingKey, len(chunk))
			for i := range chunk {
				keys[i] = chunk[i].Key
			}
			var e error
			watched, e = s.d.State.GetMany(ctx, v.ID, keys)
			if e != nil {
				return e
			}
		}
		// Compact the owned batch in place. Writes only target earlier slots,
		// and no title pointers escape the batch.
		eligible := chunk[:0]
		for _, title := range chunk {
			if q.Watched == nil || (watched[title.Key].ViewCount > 0) == *q.Watched {
				eligible = append(eligible, title)
			}
		}
		var favorites map[core.RatingKey]bool
		if q.Favorite != nil && s.d.IsFavoriteMany != nil && len(eligible) > 0 {
			keys := make([]core.RatingKey, len(eligible))
			for i := range eligible {
				keys[i] = eligible[i].Key
			}
			var e error
			favorites, e = s.d.IsFavoriteMany(ctx, v, keys)
			if e != nil {
				return e
			}
		}
		for _, title := range eligible {
			if q.Favorite != nil {
				var yes bool
				if s.d.IsFavoriteMany != nil {
					yes = favorites[title.Key]
				} else {
					if s.d.IsFavorite == nil {
						return errors.New("experience: favorites unavailable")
					}
					var e error
					yes, e = s.d.IsFavorite(ctx, v, title.Key)
					if e != nil {
						return e
					}
				}
				if yes != *q.Favorite {
					continue
				}
			}
			permitted = append(permitted, title)
		}
		clear(recursiveBatch)
		recursiveBatch = recursiveBatch[:0]
		return nil
	}
	for offset := 0; ; {
		filter := q.Filter
		if expand {
			filter = core.Filter{}
		}
		filter.Type = library.Type
		if v.MaxContentRating != "" && (filter.MaxContentRating == "" || catalog.RatingRank(v.MaxContentRating) < catalog.RatingRank(filter.MaxContentRating)) {
			filter.MaxContentRating = v.MaxContentRating
		}
		page, err := s.d.Browse(ctx, core.BrowseQuery{Filter: filter, Sort: q.Sort, Desc: q.Desc, Offset: offset, Limit: 500})
		if err != nil {
			return core.Page{}, err
		}
		var candidatesPage []core.Title
		accept := func(title core.Title, vis Visibility) error {
			id, ok := vis.SectionFor(title.Type)
			if !ok || !SectionAllowed(v, id) || !vis.Visible(ctx, v, &title) {
				return nil
			}
			if !expand {
				candidatesPage = append(candidatesPage, title)
				return nil
			}
			staticQuery := q
			staticQuery.Watched, staticQuery.Favorite = nil, nil
			ok, e := s.matchesUserQuery(ctx, v, title, staticQuery)
			if e != nil {
				return e
			}
			if ok {
				recursiveBatch = append(recursiveBatch, title)
				if len(recursiveBatch) == 400 {
					return flushRecursive()
				}
			}
			return nil
		}
		for i := range page.Items {
			parent := &page.Items[i]
			if !s.visible(ctx, v, parent) {
				continue
			}
			var children []core.Title
			if expand && parent.Type == core.TypeShow && wantShowEpisodes {
				var e error
				children, e = s.d.Grandchildren(ctx, v, parent.Key)
				if e != nil {
					return core.Page{}, e
				}
			}
			if expand && parent.Type == core.TypeShow && wantShowChildren {
				seasons, e := s.d.Children(ctx, v, parent.Key)
				if e != nil {
					return core.Page{}, e
				}
				children = append(children, seasons...)
			}
			// Copy only accepted values into the bounded batch; do not duplicate
			// or retain the catalogue's full per-show children slice.
			if e := accept(*parent, s.d.Visibility); e != nil {
				return core.Page{}, e
			}
			for start := 0; start < len(children); start += 400 {
				chunk := children[start:min(start+400, len(children))]
				vis := s.d.Visibility
				if s.d.ParentTitles != nil {
					keys := make([]core.RatingKey, 0, len(chunk))
					seen := make(map[core.RatingKey]bool)
					for _, child := range chunk {
						if child.ShowKey != 0 && !seen[child.ShowKey] {
							seen[child.ShowKey] = true
							keys = append(keys, child.ShowKey)
						}
					}
					parents, e := s.d.ParentTitles(ctx, keys)
					if e != nil {
						return core.Page{}, e
					}
					vis.GetTitle = func(_ context.Context, key core.RatingKey) (*core.Title, error) {
						title, ok := parents[key]
						if !ok || title.Key != key {
							return nil, core.ErrNotFound
						}
						return &title, nil
					}
				}
				for j := range chunk {
					if e := accept(chunk[j], vis); e != nil {
						return core.Page{}, e
					}
				}
			}
		}
		for start := 0; start < len(candidatesPage); start += 400 {
			chunk := candidatesPage[start:min(start+400, len(candidatesPage))]
			var watched map[core.RatingKey]core.WatchStatus
			if q.Watched != nil {
				keys := make([]core.RatingKey, len(chunk))
				for i := range chunk {
					keys[i] = chunk[i].Key
				}
				watched, err = s.d.State.GetMany(ctx, v.ID, keys)
				if err != nil {
					return core.Page{}, err
				}
			}
			query := q
			query.Watched = nil
			var favorites map[core.RatingKey]bool
			if q.Favorite != nil && s.d.IsFavoriteMany != nil {
				keys := make([]core.RatingKey, len(chunk))
				for i := range chunk {
					keys[i] = chunk[i].Key
				}
				favorites, err = s.d.IsFavoriteMany(ctx, v, keys)
				if err != nil {
					return core.Page{}, err
				}
				query.Favorite = nil
			}
			for _, title := range chunk {
				if q.Watched != nil && (watched[title.Key].ViewCount > 0) != *q.Watched {
					continue
				}
				if q.Favorite != nil && s.d.IsFavoriteMany != nil && favorites[title.Key] != *q.Favorite {
					continue
				}
				ok, e := s.matchesUserQuery(ctx, v, title, query)
				if e != nil {
					return core.Page{}, e
				}
				if !ok {
					continue
				}
				if !bounded || (totalMatched >= max(q.Offset, 0) && len(permitted) < q.Limit) {
					permitted = append(permitted, title)
				}
				totalMatched++
			}
		}
		offset += len(page.Items)
		if offset >= page.Total {
			break
		}
		if len(page.Items) == 0 {
			return core.Page{}, errors.New("experience: incomplete catalogue page")
		}
		if err = ctx.Err(); err != nil {
			return core.Page{}, err
		}
	}
	if expand {
		if err := flushRecursive(); err != nil {
			return core.Page{}, err
		}
	}
	if bounded {
		return core.Page{Items: permitted, Total: totalMatched}, nil
	}
	if expand {
		if err := s.sortTitles(ctx, permitted, q); err != nil {
			return core.Page{}, err
		}
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(permitted, q)
	}
	if q.StartAt != 0 {
		return startingTitles(permitted, q)
	}
	total := len(permitted)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: permitted[start:end], Total: total}, nil
}

func (s *Service) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	v, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return nil, err
	}
	req.Viewer = v
	return s.d.Rows.Rows(ctx, req, s.d.Protocol)
}
func (s *Service) Item(ctx context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
	v, err := s.viewer(ctx, req.Viewer)
	if err != nil {
		return frontend.ItemDetail{}, err
	}
	if req.Key.Kind() == core.KindCollection || req.Key.Kind() == core.KindSmart {
		if s.d.Collections == nil {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		c, err := s.d.Collections.Get(ctx, v, req.Key)
		if err != nil || c == nil || c.Key != req.Key || !s.d.Visibility.CollectionAllowed(v, c) {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		items, err := s.collectionItems(ctx, v, req.Key)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		copy := *c
		copy.Filter = nil // Smart rules may contain titles or people hidden from this viewer.
		copy.ChildCount = len(items)
		detail := frontend.ItemDetail{Collection: &copy}
		ref := frontend.FavoriteRef{Kind: frontend.FavoriteCollection, ID: req.Key.String()}
		if s.d.EntityFavoritesSource != nil && s.entityAllowed(ctx, v, ref) {
			values, e := s.d.EntityFavoritesSource(ctx, v, []frontend.FavoriteRef{ref})
			if e != nil {
				return frontend.ItemDetail{}, e
			}
			detail.UserData.IsFavorite = values[ref]
		}
		return detail, nil
	}
	if req.Key.Kind() == core.KindPlaylist {
		if s.d.Playlists == nil {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		p, err := s.d.Playlists.Get(ctx, v.ID, req.Key)
		if err != nil || p == nil || p.UserID != v.ID || p.Key != req.Key {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		copy := *p
		copy.Items = nil
		for _, key := range p.Items {
			if _, err := s.item(ctx, v, key); err == nil {
				copy.Items = append(copy.Items, key)
			}
		}
		return frontend.ItemDetail{Playlist: &copy}, nil
	}
	title, err := s.item(ctx, v, req.Key)
	if err != nil {
		return frontend.ItemDetail{}, err
	}
	// Older cached child records may omit their series label even though the
	// parent show is known. Fill it only on this neutral detail path, using a
	// cache-only lookup and the same viewer policy as the requested item.
	if title.ShowKey != 0 && title.ShowTitle == "" && s.d.CachedGet != nil {
		show, e := s.d.CachedGet(ctx, title.ShowKey)
		visibility := s.d.Visibility
		visibility.GetTitle = s.d.CachedGet
		if e == nil && show != nil && show.Key == title.ShowKey && visibility.SectionFor != nil {
			section, ok := visibility.SectionFor(show.Type)
			if ok && SectionAllowed(v, section) && visibility.Visible(ctx, v, show) {
				copy := *title
				copy.ShowTitle = show.Title
				if copy.ShowYear == 0 {
					copy.ShowYear = show.Year
				}
				title = &copy
			}
		}
	}
	d := frontend.ItemDetail{Title: *title}
	if s.d.Enrich != nil {
		d, err = s.d.Enrich(ctx, v, req.Device, title)
		if err != nil {
			return frontend.ItemDetail{}, err
		}
		d.Title = *title
		extras := d.Extras[:0:0]
		for _, extra := range d.Extras {
			if extra.ParentKey == title.Key {
				extras = append(extras, extra)
			}
		}
		d.Extras = extras
	}
	d.UserData, err = s.d.State.Get(ctx, v.ID, title.Key)
	if err != nil {
		return frontend.ItemDetail{}, err
	}
	if s.d.IsFavorite != nil {
		d.UserData.IsFavorite, err = s.d.IsFavorite(ctx, v, title.Key)
	}
	if err == nil && s.d.IsNotInterested != nil {
		d.UserData.IsNotInterested, err = s.d.IsNotInterested(ctx, v, title.Key)
	}
	if err == nil && s.d.IsWatchlisted != nil {
		d.UserData.InWatchlist, err = s.d.IsWatchlisted(ctx, v, title.Key)
	}
	return d, err
}

// AuthorizeArtwork applies the same active-viewer and item policy as Item,
// without running playback or track enrichment for a grid of images.
func (s *Service) AuthorizeArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (*core.Viewer, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	if key.Kind() == core.KindCollection || key.Kind() == core.KindSmart || key.Kind() == core.KindPlaylist {
		if _, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Device: device, Key: key}); err != nil {
			return nil, err
		}
	} else if s.d.CachedGet != nil {
		title, err := s.d.CachedGet(ctx, key)
		if errors.Is(err, core.ErrNotFound) && s.d.CachedVirtualSeason != nil {
			title, err = s.d.CachedVirtualSeason(ctx, v, key)
		}
		visibility := s.d.Visibility
		visibility.GetTitle = s.d.CachedGet
		if err != nil {
			return nil, err
		}
		if title == nil || title.Key != key || visibility.SectionFor == nil {
			return nil, core.ErrNotFound
		}
		section, ok := visibility.SectionFor(title.Type)
		if !ok || !SectionAllowed(v, section) || !visibility.Visible(ctx, v, title) {
			return nil, core.ErrNotFound
		}
	} else if _, err := s.item(ctx, v, key); err != nil {
		return nil, err
	}
	return v, nil
}
func (s *Service) Children(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
	if q.StartAt != 0 && q.AdjacentTo != 0 {
		return core.Page{}, errors.New("experience: incompatible item anchors")
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return core.Page{}, err
	}
	if key.Kind() == core.KindCollection || key.Kind() == core.KindSmart || key.Kind() == core.KindPlaylist {
		detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Device: device, Key: key})
		if err != nil {
			return core.Page{}, err
		}
		var items []core.Title
		if detail.Collection != nil {
			items, err = s.collectionItems(ctx, v, key)
		} else if detail.Playlist != nil {
			for _, k := range detail.Playlist.Items {
				if t, e := s.item(ctx, v, k); e == nil {
					items = append(items, *t)
				}
			}
		}
		if err != nil {
			return core.Page{}, err
		}
		filtered := make([]core.Title, 0, len(items))
		for _, title := range items {
			ok, e := s.matchesUserQuery(ctx, v, title, q)
			if e != nil {
				return core.Page{}, e
			}
			if ok {
				filtered = append(filtered, title)
			}
		}
		items = filtered
		if err = s.sortTitles(ctx, items, q); err != nil {
			return core.Page{}, err
		}
		if q.AdjacentTo != 0 {
			return adjacentTitles(items, q)
		}
		if q.StartAt != 0 {
			return startingTitles(items, q)
		}
		total := len(items)
		start := min(max(q.Offset, 0), total)
		end := total
		if q.Limit > 0 {
			end = min(start+q.Limit, total)
		}
		return core.Page{Items: items[start:end], Total: total}, nil
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return core.Page{}, err
	}

	var items []core.Title
	if q.Recursive && key.Kind() == core.KindShow {
		if s.d.Grandchildren == nil {
			return core.Page{}, errors.New("experience: recursive children unavailable")
		}
		items, err = s.d.Grandchildren(ctx, v, key)
	} else {
		items, err = s.d.Children(ctx, v, key)
	}
	if err != nil {
		return core.Page{}, err
	}
	out := make([]core.Title, 0, len(items))
	for i := range items {
		if s.visible(ctx, v, &items[i]) {
			ok, e := s.matchesUserQuery(ctx, v, items[i], q)
			if e != nil {
				return core.Page{}, e
			}
			if ok {
				out = append(out, items[i])
			}
		}
	}
	if err := s.sortTitles(ctx, out, q); err != nil {
		return core.Page{}, err
	}
	if q.AdjacentTo != 0 {
		return adjacentTitles(out, q)
	}
	if q.StartAt != 0 {
		return startingTitles(out, q)
	}
	total := len(out)
	start := min(max(q.Offset, 0), total)
	end := total
	if q.Limit > 0 {
		end = min(start+q.Limit, total)
	}
	return core.Page{Items: out[start:end], Total: total}, nil
}
func (s *Service) Search(ctx context.Context, viewer *core.Viewer, device core.Device, query string, q frontend.ItemQuery) (frontend.SearchPage, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	var types []core.MediaType
	for _, lib := range s.libraries() {
		if SectionAllowed(v, lib.ID) {
			types = append(types, lib.Type)
		}
	}
	offset := max(q.Offset, 0)
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if offset >= 1000 {
		return frontend.SearchPage{Continuation: frontend.ContinuationUnknown}, nil
	}
	end := min(offset+limit, 1000)
	candidates, err := s.d.Search.Find(ctx, v, query, types, 1000)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	items := make([]core.Title, 0, len(candidates))
	for _, title := range candidates {
		if !s.visible(ctx, v, &title) {
			continue
		}
		ok, e := s.matchesUserQuery(ctx, v, title, q)
		if e != nil {
			return frontend.SearchPage{}, e
		}
		if ok {
			items = append(items, title)
		}
	}
	if err = s.sortTitles(ctx, items, q); err != nil {
		return frontend.SearchPage{}, err
	}
	start := min(offset, len(items))
	stop := min(end, len(items))
	continuation := frontend.ContinuationUnknown
	if len(items) > end {
		continuation = frontend.ContinuationMore
	}
	return frontend.SearchPage{Page: core.Page{Items: items[start:stop], Total: len(items)}, TotalExact: false, Continuation: continuation}, nil
}
func (s *Service) UserData(ctx context.Context, viewer *core.Viewer, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	allowed := make([]core.RatingKey, 0, len(keys))
	for _, key := range keys {
		if _, err := s.item(ctx, v, key); err == nil {
			allowed = append(allowed, key)
		}
	}
	data, err := s.d.State.GetMany(ctx, v.ID, allowed)
	if err != nil {
		return nil, err
	}
	filtered := make(map[core.RatingKey]core.WatchStatus, len(allowed))
	for _, key := range allowed {
		status := data[key]
		status.Key = key
		filtered[key] = status
	}
	data = filtered
	if s.d.IsFavorite != nil {
		for _, key := range allowed {
			favorite, err := s.d.IsFavorite(ctx, v, key)
			if err != nil {
				return nil, err
			}
			status := data[key]
			status.Key = key
			status.IsFavorite = favorite
			data[key] = status
		}
	}
	if s.d.IsNotInterested != nil {
		for _, key := range allowed {
			value, e := s.d.IsNotInterested(ctx, v, key)
			if e != nil {
				return nil, e
			}
			status := data[key]
			status.IsNotInterested = value
			data[key] = status
		}
	}
	if s.d.IsWatchlisted != nil {
		for _, key := range allowed {
			value, e := s.d.IsWatchlisted(ctx, v, key)
			if e != nil {
				return nil, e
			}
			status := data[key]
			status.InWatchlist = value
			data[key] = status
		}
	}
	return data, nil
}

var _ frontend.Experience = (*Service)(nil)

func (s *Service) mutate(ctx context.Context, viewer *core.User, key core.RatingKey, fn func(*core.User) error) error {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	if _, err = s.playable(ctx, v, key); err != nil {
		return err
	}
	return fn(v)
}
func (s *Service) setPlayed(ctx context.Context, v *core.Viewer, k core.RatingKey, played bool) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	viewer, err := s.viewer(ctx, v)
	if err != nil {
		return err
	}
	if _, err = s.playable(ctx, viewer, k); err != nil {
		return err
	}
	keys := []core.RatingKey{k}
	if k.Kind() == core.KindShow || k.Kind() == core.KindSeason {
		var children []core.Title
		if k.Kind() == core.KindShow {
			if s.d.Grandchildren == nil {
				return errors.New("experience: episode expansion unavailable")
			}
			children, err = s.d.Grandchildren(ctx, viewer, k)
		} else {
			children, err = s.d.Children(ctx, viewer, k)
		}
		if err != nil {
			return err
		}
		keys = nil
		for _, child := range children {
			if child.Type != core.TypeEpisode {
				continue
			}
			if _, err = s.playable(ctx, viewer, child.Key); err != nil {
				return err
			}
			keys = append(keys, child.Key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if len(keys) == 1 {
		if played {
			return s.d.State.Scrobble(ctx, viewer.ID, keys[0])
		}
		return s.d.State.Unscrobble(ctx, viewer.ID, keys[0])
	}
	batch, ok := s.d.State.(core.BatchWatchState)
	if !ok {
		return errors.New("experience: atomic batch watch updates unavailable")
	}
	if played {
		return batch.ScrobbleMany(ctx, viewer.ID, keys)
	}
	return batch.UnscrobbleMany(ctx, viewer.ID, keys)
}
func (s *Service) SetPlayed(ctx context.Context, v *core.Viewer, k core.RatingKey) error {
	return s.setPlayed(ctx, v, k, true)
}
func (s *Service) SetUnplayed(ctx context.Context, v *core.Viewer, k core.RatingKey) error {
	return s.setPlayed(ctx, v, k, false)
}
func (s *Service) SetNotInterested(ctx context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.SetNotInterestedSource == nil {
			return core.ErrNotAvailable
		}
		return s.d.SetNotInterestedSource(ctx, u, k, on)
	})
}
func (s *Service) SetFavorite(ctx context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.Favorites == nil {
			return core.ErrNotFound
		}
		_, err := s.d.Favorites(ctx, u, k, on)
		return err
	})
}
func (s *Service) SetWatchlist(ctx context.Context, v *core.Viewer, k core.RatingKey, on bool) error {
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.SetWatchlistSource == nil {
			return core.ErrNotAvailable
		}
		return s.d.SetWatchlistSource(ctx, u, k, on)
	})
}
func (s *Service) SetRating(ctx context.Context, v *core.Viewer, k core.RatingKey, rating float64) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, v, k, func(u *core.User) error { return s.d.State.Rate(ctx, u.ID, k, rating) })
}
func (s *Service) RemoveFromContinue(ctx context.Context, v *core.Viewer, k core.RatingKey) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, v, k, func(u *core.User) error {
		if s.d.RemoveContinue == nil {
			return core.ErrNotFound
		}
		return s.d.RemoveContinue(ctx, u, k)
	})
}

func (s *Service) sortTitles(ctx context.Context, items []core.Title, q frontend.ItemQuery) error {
	if q.Sort == "" {
		return nil
	}
	if q.Sort == core.SortAdded && s.d.AddedTimes != nil {
		keys := make([]core.RatingKey, len(items))
		for i := range items {
			keys[i] = items[i].Key
		}
		dates, err := s.d.AddedTimes(ctx, keys)
		if err != nil {
			return err
		}
		for i := range items {
			if date, known := dates[items[i].Key]; known {
				items[i].AddedAt = date
			}
		}
	}
	switch q.Sort {
	case core.SortTitle, core.SortReleased, core.SortAdded, core.SortRating, core.SortPopularity:
	default:
		return errors.New("experience: unsupported sort")
	}
	now := time.Now()
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		cmp := 0
		switch q.Sort {
		case core.SortTitle:
			at, bt := a.SortTitle, b.SortTitle
			if at == "" {
				at = a.Title
			}
			if bt == "" {
				bt = b.Title
			}
			cmp = strings.Compare(strings.ToLower(at), strings.ToLower(bt))
		case core.SortAdded:
			ad, bd := a.DateAdded(now), b.DateAdded(now)
			if ad.Before(bd) {
				cmp = -1
			} else if ad.After(bd) {
				cmp = 1
			}
		case core.SortReleased:
			if a.Released.Before(b.Released) {
				cmp = -1
			} else if a.Released.After(b.Released) {
				cmp = 1
			}
		case core.SortRating:
			if a.Rating < b.Rating {
				cmp = -1
			} else if a.Rating > b.Rating {
				cmp = 1
			}
		case core.SortPopularity:
			if a.Popularity < b.Popularity {
				cmp = -1
			} else if a.Popularity > b.Popularity {
				cmp = 1
			}
		}
		if cmp == 0 {
			return a.Key < b.Key
		}
		if q.Desc {
			return cmp > 0
		}
		return cmp < 0
	})
	return nil
}

// Similar applies policy before pagination and marks truncated candidate sets unknown.
func (s *Service) Similar(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, q frontend.ItemQuery) (frontend.SearchPage, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	if _, err = s.item(ctx, v, key); err != nil {
		return frontend.SearchPage{}, err
	}
	if s.d.SimilarSource == nil {
		return frontend.SearchPage{}, core.ErrNotFound
	}
	candidates, err := s.d.SimilarSource(ctx, key, 1000)
	if err != nil {
		return frontend.SearchPage{}, err
	}
	if s.d.FilterRecommendations != nil {
		candidates, err = s.d.FilterRecommendations(ctx, v, candidates)
		if err != nil {
			return frontend.SearchPage{}, err
		}
	}
	items := make([]core.Title, 0, len(candidates))
	for i := range candidates {
		if s.visible(ctx, v, &candidates[i]) {
			ok, e := s.matchesUserQuery(ctx, v, candidates[i], q)
			if e != nil {
				return frontend.SearchPage{}, e
			}
			if ok {
				items = append(items, candidates[i])
			}
		}
	}
	if err = s.sortTitles(ctx, items, q); err != nil {
		return frontend.SearchPage{}, err
	}
	offset := min(max(q.Offset, 0), len(items))
	end := len(items)
	if q.Limit > 0 {
		end = min(offset+q.Limit, end)
	}
	// Existing catalogue Similar may use only one upstream page even when
	// fewer than requested are returned; exhaustion is not knowable here.
	continuation := frontend.ContinuationUnknown
	exact := false
	if end < len(items) {
		continuation = frontend.ContinuationMore
	}
	return frontend.SearchPage{Page: core.Page{Items: items[offset:end], Total: len(items)}, TotalExact: exact, Continuation: continuation}, nil
}

// Ancestors returns only authorized parents, failing closed if a parent is hidden.
func (s *Service) Ancestors(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]core.Title, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	t, err := s.item(ctx, v, key)
	if err != nil {
		return nil, err
	}
	var keys []core.RatingKey
	if t.ShowKey != 0 {
		keys = append(keys, t.ShowKey)
		if t.Type == core.TypeEpisode && t.Season > 0 {
			keys = append(keys, core.SeasonKey(t.Key.TMDB(), t.Season))
		}
	}
	out := make([]core.Title, 0, len(keys))
	for _, k := range keys {
		parent, err := s.item(ctx, v, k)
		if err != nil {
			return nil, err
		}
		out = append(out, *parent)
	}
	return out, nil
}

// Facets are computed only from titles the viewer may see.
func (s *Service) Facets(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, name string) ([]core.FacetValue, error) {
	page, err := s.LibraryItems(ctx, viewer, device, viewID, frontend.ItemQuery{})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	labels := map[string]string{}
	for _, t := range page.Items {
		var values []string
		switch name {
		case "person":
			for _, group := range [][]core.Person{t.Cast, t.Directors, t.Writers, t.Crew} {
				for _, person := range group {
					if person.TMDB > 0 && person.Name != "" {
						key := strconv.FormatInt(person.TMDB, 10)
						values = append(values, key)
						if old := labels[key]; old == "" || person.Name < old {
							labels[key] = person.Name
						}
					}
				}
			}
		case "genre":
			values = t.Genres
		case "year":
			if t.Year > 0 {
				values = []string{strconv.Itoa(t.Year)}
			}
		case "decade":
			if t.Year > 0 {
				values = []string{strconv.Itoa(t.Year / 10 * 10)}
			}
		case "studio":
			values = t.Studios
		case "country":
			values = t.Countries
		case "contentRating":
			if t.ContentRating != "" {
				values = []string{t.ContentRating}
			}
		default:
			return nil, errors.New("experience: unsupported facet")
		}
		seen := map[string]bool{}
		for _, v := range values {
			if v != "" && !seen[v] {
				counts[v]++
				seen[v] = true
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]core.FacetValue, 0, len(keys))
	seenIDs := make(map[string]string, len(keys))
	for _, k := range keys {
		id := facetID(name, k)
		if old, ok := seenIDs[id]; ok && old != k {
			return nil, errors.New("experience: facet identifier collision")
		}
		seenIDs[id] = k
		title := k
		if label := labels[k]; label != "" {
			title = label
		}
		out = append(out, core.FacetValue{ID: id, Key: k, Title: title, Count: counts[k]})
	}
	return out, nil
}

func (s *Service) collectionItems(ctx context.Context, v *core.User, key core.RatingKey) ([]core.Title, error) {
	var out []core.Title
	for offset := 0; ; {
		page, err := s.d.Collections.Items(ctx, v, key, offset, 500)
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			if s.visible(ctx, v, &page.Items[i]) {
				out = append(out, page.Items[i])
			}
		}
		offset += len(page.Items)
		if offset >= page.Total {
			return out, nil
		}
		if len(page.Items) == 0 {
			return nil, errors.New("experience: incomplete collection page")
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (s *Service) entityAllowed(ctx context.Context, v *core.User, ref frontend.FavoriteRef) bool {
	if ref.ID == "" {
		return false
	}
	switch ref.Kind {
	case frontend.FavoriteTitle:
		key, err := core.ParseRatingKey(ref.ID)
		if err != nil {
			return false
		}
		_, err = s.item(ctx, v, key)
		return err == nil
	case frontend.FavoritePerson, frontend.FavoriteCollection, frontend.FavoriteChannel:
		return s.d.CanFavoriteEntity != nil && s.d.CanFavoriteEntity(ctx, v, ref)
	default:
		return false
	}
}
func (s *Service) SetEntityFavorite(ctx context.Context, viewer *core.Viewer, ref frontend.FavoriteRef, value bool) error {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return err
	}
	if !s.entityAllowed(ctx, v, ref) {
		return core.ErrNotFound
	}
	if s.d.SetEntityFavoriteSource == nil {
		return core.ErrNotFound
	}
	return s.d.SetEntityFavoriteSource(ctx, v, ref, value)
}
func (s *Service) EntityFavorites(ctx context.Context, viewer *core.Viewer, refs []frontend.FavoriteRef) (map[frontend.FavoriteRef]bool, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	allowed := make([]frontend.FavoriteRef, 0, len(refs))
	for _, ref := range refs {
		if s.entityAllowed(ctx, v, ref) {
			allowed = append(allowed, ref)
		}
	}
	if s.d.EntityFavoritesSource == nil {
		return nil, core.ErrNotFound
	}
	values, err := s.d.EntityFavoritesSource(ctx, v, allowed)
	if err != nil {
		return nil, err
	}
	out := make(map[frontend.FavoriteRef]bool, len(allowed))
	for _, ref := range allowed {
		out[ref] = values[ref]
	}
	return out, nil
}

func containsMediaType(types []core.MediaType, want core.MediaType) bool {
	for _, typ := range types {
		if typ == want {
			return true
		}
	}
	return false
}

func (s *Service) matchesUserQuery(ctx context.Context, v *core.User, t core.Title, q frontend.ItemQuery) (bool, error) {
	if !matchesFilter(t, q.Filter) {
		return false, nil
	}
	if q.Season != nil && t.Season != *q.Season {
		return false, nil
	}
	if len(q.Types) > 0 {
		found := false
		for _, typ := range q.Types {
			if t.Type == typ {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	if q.Watched != nil {
		st, err := s.d.State.Get(ctx, v.ID, t.Key)
		if err != nil {
			return false, err
		}
		if (st.ViewCount > 0) != *q.Watched {
			return false, nil
		}
	}
	if q.Favorite != nil {
		if s.d.IsFavorite == nil {
			return false, errors.New("experience: favorites unavailable")
		}
		favorite, err := s.d.IsFavorite(ctx, v, t.Key)
		if err != nil {
			return false, err
		}
		if favorite != *q.Favorite {
			return false, nil
		}
	}
	return true, nil
}

func matchesFilter(t core.Title, f core.Filter) bool {
	if f.MaxContentRating != "" && catalog.RatingExceeds(t.ContentRating, f.MaxContentRating) {
		return false
	}
	for _, key := range f.ExcludeKeys {
		if t.Key == key {
			return false
		}
	}
	if f.Type != "" && t.Type != f.Type {
		return false
	}
	if f.YearFrom > 0 && t.Year < f.YearFrom {
		return false
	}
	if f.YearTo > 0 && t.Year > f.YearTo {
		return false
	}
	if f.MinRating > 0 && t.Rating < f.MinRating {
		return false
	}
	if f.MinRuntime > 0 && t.Runtime < f.MinRuntime {
		return false
	}
	if f.MaxRuntime > 0 && t.Runtime > f.MaxRuntime {
		return false
	}
	intersects := func(a, b []string) bool {
		if len(b) == 0 {
			return true
		}
		for _, x := range a {
			for _, y := range b {
				if strings.EqualFold(x, y) {
					return true
				}
			}
		}
		return false
	}
	if !intersects(t.Genres, f.Genres) || !intersects([]string{t.Language}, f.Languages) || !intersects(t.Countries, f.Countries) || !intersects([]string{t.ContentRating}, f.ContentRatings) {
		return false
	}
	if f.Collection != 0 && t.CollectionID != f.Collection {
		return false
	}
	if f.Studio != "" && !intersects(t.Studios, []string{f.Studio}) {
		return false
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(f.Query)) && !strings.Contains(strings.ToLower(t.OriginalTitle), strings.ToLower(f.Query)) {
		return false
	}
	if f.Person != 0 {
		found := false
		for _, group := range [][]core.Person{t.Cast, t.Directors, t.Writers, t.Crew} {
			for _, p := range group {
				if p.TMDB == f.Person {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SetLikes delegates to the state's atomic shared-rating operation.
func (s *Service) SetLikes(ctx context.Context, viewer *core.Viewer, key core.RatingKey, likes *bool) error {
	ctx = state.WithOrigin(ctx, state.Origin{Surface: s.d.Protocol})
	return s.mutate(ctx, viewer, key, func(v *core.User) error {
		state, ok := s.d.State.(core.LikesState)
		if !ok {
			return errors.New("experience: atomic likes unavailable")
		}
		return state.SetLikes(ctx, v.ID, key, likes)
	})
}

func facetID(kind, value string) string {
	sum := sha256.Sum256([]byte("ether/facet/v1\x00" + kind + "\x00" + value))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return hex.EncodeToString(sum[:16])
}

// ResolveFacet only resolves identifiers among the viewer's current visible facets.
func (s *Service) ResolveFacet(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, kind, id string) (core.FacetValue, error) {
	facets, err := s.Facets(ctx, viewer, device, viewID, kind)
	if err != nil {
		return core.FacetValue{}, err
	}
	var found *core.FacetValue
	for i := range facets {
		if facets[i].ID == id {
			if found != nil {
				return core.FacetValue{}, errors.New("experience: facet identifier collision")
			}
			found = &facets[i]
		}
	}
	if found == nil {
		return core.FacetValue{}, core.ErrNotFound
	}
	return *found, nil
}
