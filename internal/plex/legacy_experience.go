package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) legacyInProgressTitles(ctx context.Context, pr *Request, t core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	if s.d.State == nil || pr.User == nil {
		return nil, nil
	}
	sts, err := s.d.State.InProgress(ctx, pr.User.ID, limit*2+10)
	if err != nil {
		s.log.Warn("load in-progress", "err", err)
		return nil, nil
	}
	keys := make([]core.RatingKey, 0, len(sts))
	state := make(map[core.RatingKey]core.WatchStatus, len(sts))
	for _, st := range sts {
		keys = append(keys, st.Key)
		state[st.Key] = st
	}
	titles := s.getTitles(ctx, keys)
	// Resume unfinished media and advance recently completed series. A
	// completed episode has no offset, so InProgress alone loses the show.
	if t != core.TypeMovie {
		titles = s.legacyAppendNextContinue(ctx, pr, titles, state, limit)
	}
	sort.SliceStable(titles, func(i, j int) bool {
		a, b := state[titles[i].Key].LastViewedAt, state[titles[j].Key].LastViewedAt
		if !a.Equal(b) {
			return a.After(b)
		}
		return titles[i].Key < titles[j].Key
	})
	if s.d.FavoriteShows != nil {
		if favorites, err := s.d.FavoriteShows(ctx, pr.User); err == nil {
			experience.PrioritizeFavoriteShows(titles, favorites)
		}
	}
	seenShows := map[int64]bool{}
	out := titles[:0:0]
	for _, ti := range titles {
		if t != "" {
			want := t
			if t == core.TypeShow {
				want = core.TypeEpisode
			}
			if ti.Type != want {
				continue
			}
		}
		sec, ok := s.sectionFor(ti.Type)
		if ok && !s.canSeeSection(pr.User, sec.ID) {
			continue
		}
		if ti.Type == core.TypeEpisode {
			if seenShows[ti.Key.TMDB()] {
				continue
			}
			seenShows[ti.Key.TMDB()] = true
		}
		out = append(out, ti)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, state
}

// getTitles fetches titles concurrently, preserving order and skipping
// failures.

func (s *Server) legacySectionRecentlyViewed(w http.ResponseWriter, r *http.Request, sec Section) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := s.sectionContainer(sec)
	mc.Title2 = "Recently Viewed"
	if s.d.State != nil && pr.User != nil {
		hist, err := s.d.State.History(ctx, pr.User.ID, time.Time{}, 100)
		if err == nil {
			seen := map[core.RatingKey]bool{}
			var keys []core.RatingKey
			for _, h := range hist {
				if !seen[h.Key] {
					seen[h.Key] = true
					keys = append(keys, h.Key)
				}
			}
			var items []core.Title
			for _, t := range s.getTitles(ctx, keys) {
				if sct, ok := s.sectionFor(t.Type); ok && sct.ID == sec.ID {
					items = append(items, t)
				}
			}
			mc.Metadata = s.renderTitles(ctx, pr, items, renderOpts{media: true})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) legacyAppendNextContinue(ctx context.Context, pr *Request, titles []core.Title, state map[core.RatingKey]core.WatchStatus, limit int) []core.Title {
	history, err := s.d.State.History(ctx, pr.User.ID, s.now().Add(-90*24*time.Hour), 200)
	if err != nil {
		return titles
	}
	seen := map[int64]bool{}
	for _, t := range titles {
		if t.Type == core.TypeEpisode {
			seen[t.Key.TMDB()] = true
		}
	}
	var recent []core.HistoryEntry
	var keys, shows []core.RatingKey
	for _, h := range history {
		if !h.Completed || h.Key.Kind() != core.KindEpisode || seen[h.Key.TMDB()] {
			continue
		}
		seen[h.Key.TMDB()] = true
		recent = append(recent, h)
		keys = append(keys, h.Key)
		shows = append(shows, core.ShowKey(h.Key.TMDB()))
		if len(recent) >= min(max(limit, 1), 50) {
			break
		}
	}
	statuses, err := s.d.State.GetMany(ctx, pr.User.ID, keys)
	if err != nil {
		return titles
	}
	grouped := map[core.RatingKey]bool{}
	if s.d.EpisodeOrder != nil {
		grouped, err = s.d.EpisodeOrder.Grouped(ctx, pr.User, shows)
		if err != nil {
			return titles
		}
	}
	for _, h := range recent {
		if ctx.Err() != nil {
			break
		}
		if statuses[h.Key].ViewCount == 0 {
			continue
		} // subsequently marked unwatched
		current := h.Key
		var ordered []core.Title
		if grouped[core.ShowKey(current.TMDB())] {
			eps, e := s.metadataGrandchildren(ctx, pr, core.ShowKey(current.TMDB()))
			if e != nil {
				continue
			}
			for i := range eps {
				if eps[i].Key == current {
					ordered = eps[i+1:]
					break
				}
			}
		}
		for n := 0; n < 32; n++ {
			var next *core.Title
			if grouped[core.ShowKey(current.TMDB())] {
				if n >= len(ordered) {
					break
				}
				next = &ordered[n]
			} else {
				next, err = s.d.Catalog.NextEpisode(ctx, current)
				if err != nil || next == nil || next.Key == current {
					break
				}
			}
			current = next.Key
			if next.Season == 0 {
				continue
			}
			if !next.Released.IsZero() && next.Released.After(s.now()) {
				break
			}
			if !s.visibleTitle(ctx, pr.User, next) || s.sameDayUnavailable(ctx, pr, next) {
				break
			}
			st, e := s.d.State.Get(ctx, pr.User.ID, next.Key)
			if e != nil {
				break
			}
			if st.ViewCount > 0 && st.ViewOffset <= 0 {
				continue
			}
			st.Key = next.Key
			if st.LastViewedAt.Before(h.ViewedAt) {
				st.LastViewedAt = h.ViewedAt
			}
			state[next.Key] = st
			titles = append(titles, *next)
			break
		}
	}
	return titles
}

func (s *Server) legacyRecentlyAddedHub(ctx context.Context, pr *Request, sec Section, count int) (hubSpec, error) {
	q := core.BrowseQuery{Filter: core.Filter{Type: sec.Type}, Sort: core.SortAdded, Desc: true, Limit: count + 1}
	if pr.User != nil {
		q.Filter.MaxContentRating = pr.User.MaxContentRating
	}
	page, err := s.d.Catalog.Browse(ctx, q)
	if err != nil {
		return hubSpec{}, err
	}
	id := strconv.Itoa(sec.ID)
	hs := hubSpec{Hub: core.Hub{Title: "Recently Added in " + sec.Title, Type: sec.Type, Style: "shelf", Promoted: true, Items: page.Items}}
	if sec.Type == core.TypeShow {
		hs.ID, hs.Context, hs.Type = "tv.recentlyadded."+id, "hub.tv.recentlyadded", ""
		hs.key = "/hubs/home/recentlyAdded?type=2&sectionID=" + id
	} else {
		hs.ID, hs.Context = "movie.recentlyadded."+id, "hub.movie.recentlyadded"
		hs.key = "/library/sections/" + id + "/all?sort=addedAt:desc"
	}
	if len(hs.Items) > count {
		hs.Items, hs.More = hs.Items[:count], true
	}
	return hs, nil
}

// parseSectionList parses "1,2" (contentDirectoryID).

func (s *Server) legacyHandlePostplay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	count := countParam(r, 12)
	mc := hubContainer()
	seen := map[core.RatingKey]bool{k: true}
	if h, ok := s.upNextHub(ctx, pr, k); ok {
		mc.Hub = append(mc.Hub, s.renderHub(ctx, pr, h, hubCtx{related: k}))
		seen[h.Items[0].Key] = true
	}
	if picks := s.postPlayPicks(ctx, pr, k, count); len(picks) > 0 {
		// Skip whatever Up Next already covers so the queue never
		// repeats the next episode as a "recommendation".
		kept := picks[:0:0]
		for _, p := range picks {
			if !seen[p.Title.Key] {
				seen[p.Title.Key] = true
				kept = append(kept, p)
			}
		}
		if mh, ok := s.legacyPostPlayHub(ctx, pr, k, kept); ok {
			mc.Hub = append(mc.Hub, mh)
		}
	}
	s.legacyRelatedAfterPostPlay(ctx, pr, mc, k, count, seen)
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Search

func (s *Server) legacyRunSearch(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]core.Title, error) {
	types := s.searchTypes(pr.User, sectionID)
	if strings.TrimSpace(query) == "" || len(types) == 0 {
		return nil, nil
	}
	if items, ok := s.discoverySearch(ctx, pr, query, limit); ok {
		items = s.visibleTitles(pr.User, items)
		allowed := map[core.MediaType]bool{}
		for _, typ := range types {
			allowed[typ] = true
		}
		filtered := items[:0:0]
		for _, item := range items {
			if allowed[item.Type] {
				filtered = append(filtered, item)
			}
		}
		return filtered, nil
	}
	res, err := s.d.Catalog.Search(ctx, query, types, limit)
	if err != nil {
		return nil, err
	}
	return s.filterAllowed(pr.User, res), nil
}

func (s *Server) legacyVibeSearch(ctx context.Context, pr *Request, query string) (*core.Collection, []core.Title, bool, error) {
	searcher := s.d.VibeSearch
	if searcher == nil {
		searcher, _ = s.d.Collections.(core.VibeSearcher)
	}
	if searcher == nil || pr == nil {
		return nil, nil, false, nil
	}
	c, items, ok, err := searcher.VibeCollection(ctx, pr.User, query)
	if err != nil || !ok {
		return c, items, ok, err
	}
	return c, s.filterAllowed(pr.User, items), true, nil
}

func (s *Server) legacyPostPlayHub(ctx context.Context, pr *Request, key core.RatingKey, picks []core.PostPlayPick) (model.Hub, bool) {
	items := make([]core.Title, 0, len(picks))
	for _, p := range picks {
		items = append(items, p.Title)
	}
	items = s.filterAllowed(pr.User, items)
	if len(items) == 0 {
		return model.Hub{}, false
	}
	prefix := "movie"
	if k := key.Kind(); k == core.KindShow || k == core.KindSeason || k == core.KindEpisode {
		prefix = "tv"
	}
	h := core.Hub{
		ID: prefix + ".postplay", Title: "Recommended For You",
		Context: "hub." + prefix + ".postplay", Style: "shelf", Items: items,
	}
	return s.renderHub(ctx, pr, hubSpec{Hub: h}, hubCtx{related: key}), true
}

// relatedAfterPostPlay appends the plain Related rows behind the hook
// queue, dropping titles the queue already shows, as post-play screens
// scroll from the personal pick into the familiar rows.

func (s *Server) legacyRelatedAfterPostPlay(ctx context.Context, pr *Request, mc *model.MediaContainer, key core.RatingKey, count int, seen map[core.RatingKey]bool) {
	if s.d.Recommender == nil {
		return
	}
	hubs, err := s.d.Recommender.Related(ctx, pr.User, key, count)
	if err != nil {
		s.log.Debug("postplay related hubs", "key", key, "err", err)
		return
	}
	for _, h := range hubs {
		items := h.Items[:0:0]
		for _, t := range h.Items {
			if !seen[t.Key] {
				seen[t.Key] = true
				items = append(items, t)
			}
		}
		if len(items) == 0 {
			continue
		}
		h.Items = items
		mc.Hub = append(mc.Hub, s.renderHub(ctx, pr, hubSpec{Hub: h}, hubCtx{related: key}))
	}
}

// decorateDetail adds the viewer's micro-reason decision header (#31) to
// a single-item detail response. It only touches the rendered copy: the
// header prefixes Summary (every app shows it on the detail page), and
// when the title has no tagline of its own it takes the Tagline slot
// instead — the line living-room apps put directly under the title —
// rather than appearing twice. Original text is kept intact.

func (s *Server) legacyDiscoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	d := s.d.Discovery
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(hubs []core.Hub) {
		for _, h := range hubs {
			h.Items = s.visibleTitles(pr.User, h.Items)
			if len(h.Items) > 0 {
				out = append(out, hubSpec{Hub: h})
			}
		}
	}
	if d.Watchlist != nil {
		h, _ := d.Watchlist.Hubs(ctx, pr.User, limit)
		add(h)
	}
	if d.Personal != nil {
		h, _ := d.Personal.Hubs(ctx, pr.User, limit)
		add(h)
	}
	return out
}

func (s *Server) legacyScopedCollectionItems(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
	pr := FromContext(ctx)
	p, ok := s.d.Collections.(core.CollectionPlaceholder)
	if !ok || !p.AllowsPlaceholders(ctx, u, k, core.ClassifyDevice(pr.Product, pr.Platform, pr.Device)) {
		return nil, core.ErrNotFound
	}
	page, err := s.d.Collections.Items(ctx, u, k, 0, 5000)
	if err != nil {
		return nil, err
	}
	allowed := make([]core.Title, 0, len(page.Items))
	for _, t := range page.Items {
		if !s.titleVisible(u, &t) || t.Released.IsZero() {
			continue
		}
		if t.Released.After(s.now()) {
			if _, ok := s.d.Collections.(core.CollectionPlaceholder); !ok {
				continue
			}
		} else if len(s.availableForViewer(ctx, u, []core.Title{t})) == 0 {
			continue
		}
		allowed = append(allowed, t)
	}
	return allowed, nil
}
