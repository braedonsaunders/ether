package plex

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// parseKeys parses a {key} path parameter that may be a comma list. Lists
// stop at maxExpand keys: every key costs a catalogue lookup (possibly a
// TMDB fetch), and hub keys never list more than a hub's 200 items.
func parseKeys(s string) ([]core.RatingKey, error) {
	var out []core.RatingKey
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(out) == maxExpand {
			break
		}
		k, err := core.ParseRatingKey(p)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, errors.New("no keys")
	}
	return out, nil
}

func keyParam(r *http.Request) (core.RatingKey, error) {
	return core.ParseRatingKey(chi.URLParam(r, "key"))
}

// itemContainer is the container for a single-section item response.
func (s *Server) itemContainer(t core.MediaType) *model.MediaContainer {
	mc := libraryContainer()
	if sec, ok := s.sectionFor(t); ok {
		mc.LibrarySectionID = model.Ptr(sec.ID)
		mc.LibrarySectionTitle = sec.Title
		mc.LibrarySectionUUID = sec.UUID
	}
	return mc
}

// titleVisible reports whether the user may see the title's section.
func (s *Server) titleVisible(u *core.User, t *core.Title) bool {
	return s.visibility().TitleAllowed(u, t)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	keys, err := parseKeys(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	var mc *model.MediaContainer
	for _, k := range keys {
		var md model.Metadata
		switch k.Kind() {
		case core.KindCollection, core.KindSmart:
			c, err := s.collection(ctx, pr.User, k)
			if err != nil {
				if len(keys) == 1 {
					s.writeErr(w, r, err)
					return
				}
				continue
			}
			md = s.collectionMetadata(c)
			if mc == nil {
				mc = s.itemContainer(c.Type)
			}
		case core.KindPlaylist:
			pl, err := s.playlist(ctx, pr.User, k)
			if err != nil {
				if len(keys) == 1 {
					s.writeErr(w, r, err)
					return
				}
				continue
			}
			md = s.playlistMetadata(ctx, pl)
			if mc == nil {
				mc = libraryContainer()
			}
		case core.KindExtra:
			emd, ok := s.extraMetadataByKey(ctx, pr, k)
			if !ok {
				if len(keys) == 1 {
					writeHTTPError(w, http.StatusNotFound)
					return
				}
				continue
			}
			md = emd
			if mc == nil {
				mc = libraryContainer()
			}
		default:
			t, err := s.metadataTitle(ctx, pr, k)
			if err == nil && ((!t.Released.IsZero() && t.Released.After(s.now())) || !s.visibleTitle(ctx, pr.User, t)) {
				err = core.ErrNotFound
			}
			if err != nil && len(keys) == 1 {
				if t, ck, e := s.lookupPlaceholder(ctx, pr, k, q.Get("collection")); e == nil {
					md = s.placeholderMetadata(ctx, pr, t, ck)
					mc = s.itemContainer(t.Type)
					mc.Metadata = append(mc.Metadata, md)
					continue
				}
			}
			if err != nil {
				if len(keys) == 1 {
					s.writeErr(w, r, err)
					return
				}
				continue
			}
			if len(keys) == 1 && isPlayable(t.Type) {
				warmed := false
				if warm, ok := s.d.Playback.(core.PlaybackPreparer); ok {
					warmed = warm.PreResolve(ctx, pr.User, t.Key, pr.ClientCaps(s.cfg().Transcode.RemoteMaxBitrateKbps))
				}
				// The warm-up fetches subtitle sidecars for the release it
				// resolves; without it, fetch them on their own so the play
				// queue can list them (cached ones are listed already).
				if !warmed {
					s.prefetchSidecars(ctx, pr, t)
				}
			}
			if len(keys) == 1 {
				// Live session intent (#32): a single-item detail open.
				s.recordDetailIntent(ctx, pr, t)
			}
			// renderTitles uses verified cached GUIDs and queues missing identities.
			// Remote identity enrichment must not hold detail or Play requests.

			md = s.detail(ctx, pr, t, q.Get("includeChildren") == "1", q.Get("includeOnDeck") == "1", q.Get("includeRelated") == "1", q.Get("includeExtras") == "1", q.Get("extraTypes"))
			if mc == nil {
				mc = s.itemContainer(t.Type)
			}
		}
		mc.Metadata = append(mc.Metadata, md)
	}
	if mc == nil {
		mc = libraryContainer()
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// detail renders a full item page.
func (s *Server) detail(ctx context.Context, pr *Request, t *core.Title, children, onDeck, related, extras bool, extraTypes string) model.Metadata {
	s.rememberTitle(t)
	memo := showEpisodes{}
	ts := []core.Title{*t}
	items := s.renderTitles(ctx, pr, ts, renderOpts{detail: true, media: true})
	s.applyLeafCounts(ctx, pr, items, ts, memo)
	md := items[0]
	// Per-viewer decision header (#31); detail pages only, never lists.
	s.decorateDetail(ctx, pr, t, &md)
	if children && (t.Type == core.TypeShow || t.Type == core.TypeSeason) {
		if kids, err := s.metadataChildren(ctx, pr, t.Key); err == nil {
			rendered := s.renderTitles(ctx, pr, kids, renderOpts{media: true})
			if t.Type == core.TypeShow {
				s.applyLeafCounts(ctx, pr, rendered, kids, memo)
			}
			md.Children = &model.Children{Size: len(rendered), Metadata: rendered}
		}
	}
	if onDeck && t.Type == core.TypeShow {
		if ep, st := s.showOnDeck(ctx, pr, t.Key, memo); ep != nil {
			if items := s.renderTitles(ctx, pr, []core.Title{*ep}, renderOpts{media: true, state: st}); len(items) > 0 {
				md.OnDeck = &model.OnDeck{Metadata: items[0]}
			}
			// The show page's Play button plays this episode: resolve,
			// open and probe it while the viewer reads the page.
			if warm, ok := s.d.Playback.(core.PlaybackPreparer); ok {
				warm.PreResolve(ctx, pr.User, ep.Key, pr.ClientCaps(s.cfg().Transcode.RemoteMaxBitrateKbps))
			}
		}
	}
	if related && s.d.Recommender != nil {
		if hubs, err := s.d.Recommender.Related(ctx, pr.User, t.Key, 12); err == nil {
			md.Related = &model.Related{Hub: s.renderHubs(ctx, pr, hubs, hubCtx{related: t.Key})}
		} else {
			s.log.Debug("related hubs", "key", t.Key, "err", err)
		}
	}
	if extras {
		list := s.extrasFor(ctx, pr, t, extraTypes)
		ex := &model.Extras{Size: len(list)}
		for _, e := range list {
			ex.Metadata = append(ex.Metadata, s.extraMetadata(e, t, true))
		}
		md.Extras = ex
		if len(list) > 0 {
			md.PrimaryExtraKey = "/library/metadata/" + list[0].Key.String()
		}
	}
	return md
}

// sameDayUnavailable is a local visibility lookup. Merely browsing must never
// spend search quota trying to discover an upload.
func (s *Server) sameDayUnavailable(ctx context.Context, pr *Request, t *core.Title) bool {
	if t == nil || t.Type != core.TypeEpisode || t.Released.IsZero() {
		return false
	}
	if s.d.FilterAvailable != nil {
		return len(s.d.FilterAvailable(ctx, []core.Title{*t})) == 0
	}
	if age := s.now().Sub(t.Released); age < 0 || age >= 24*time.Hour {
		return false
	}
	if av, ok := s.d.Variants.(core.AvailabilitySource); ok {
		known, available := av.Available(ctx, t.Key)
		return known && !available
	}
	return false
}

// showOnDeck picks the episode to resume or start next.
func (s *Server) showOnDeck(ctx context.Context, pr *Request, show core.RatingKey, memo showEpisodes) (*core.Title, map[core.RatingKey]core.WatchStatus) {
	eps, err := s.episodesOfShow(ctx, pr, memo, show)
	if err != nil || len(eps) == 0 {
		return nil, nil
	}
	st := s.stateFor(ctx, pr, eps)
	lastWatched := -1
	for i, e := range eps {
		ws := st[e.Key]
		if ws.ViewOffset > 0 && ws.ViewCount == 0 {
			return &eps[i], st
		}
		if ws.ViewCount > 0 {
			lastWatched = i
		}
	}
	for i := lastWatched + 1; i < len(eps); i++ {
		if eps[i].Season > 0 && st[eps[i].Key].ViewCount == 0 && !s.sameDayUnavailable(ctx, pr, &eps[i]) {
			return &eps[i], st
		}
	}
	if lastWatched < 0 {
		for i := range eps {
			if eps[i].Season > 0 && !s.sameDayUnavailable(ctx, pr, &eps[i]) {
				return &eps[i], st
			}
		}
	}
	return nil, nil
}

func (s *Server) handleChildren(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	switch k.Kind() {
	case core.KindCollection, core.KindSmart:
		s.collectionItems(w, r, k)
		return
	case core.KindPlaylist:
		s.handlePlaylistItems(w, r)
		return
	}
	parent, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, parent) {
		err = core.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.rememberTitle(parent) // seasons render the show's studio/year/logo
	kids, err := s.metadataChildren(ctx, pr, k)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(parent.Type)
	mc.Key = parent.Key.String()
	mc.Nocache = model.Ptr(true)
	mc.Thumb = thumbURL(parent)
	mc.Art = artURL(parent)
	var allLeaves *model.Directory
	switch parent.Type {
	case core.TypeShow:
		mc.Summary = parent.Summary
		mc.ParentIndex = model.Ptr(1)
		mc.ParentTitle = parent.Title
		mc.ParentYear = parent.Year
		mc.Title1 = mc.LibrarySectionTitle
		mc.Title2 = parent.Title
		mc.ViewGroup = "season"
		mc.ViewMode = 65593
		if r.URL.Query().Get("excludeAllLeaves") != "1" {
			allLeaves = &model.Directory{LeafCount: model.Ptr(parent.LeafCount), Thumb: thumbURL(parent), ViewedLeafCount: model.Ptr(0),
				Key: "/library/metadata/" + parent.Key.String() + "/allLeaves", Title: "All episodes"}
		}
	case core.TypeSeason:
		show := parent.ShowKey
		if show == 0 {
			show = parent.Key.Parent()
		}
		mc.ParentIndex = model.Ptr(parent.Season)
		mc.ParentTitle = ""
		// PMS always sends parentTitle here, empty.
		mc.Extra = append(mc.Extra, model.A("parentTitle", ""))
		if st, err := s.d.Catalog.Get(ctx, show); err == nil {
			s.rememberTitle(st)
			if st.ContentRating != "" {
				mc.Extra = append(mc.Extra, model.A("grandparentContentRating", st.ContentRating))
			}
			if len(st.Studios) > 0 {
				mc.Extra = append(mc.Extra, model.A("grandparentStudio", st.Studios[0]))
			}
		}
		mc.GrandparentRatingKey = int64(show)
		mc.GrandparentTitle = parent.ShowTitle
		mc.GrandparentThumb = "/library/metadata/" + show.String() + "/thumb/" + imageStamp(parent)
		if mc.Art == "" {
			mc.Art = "/library/metadata/" + show.String() + "/art/" + imageStamp(parent)
		}
		mc.Title1 = parent.ShowTitle
		mc.Title2 = seasonTitle(parent)
		mc.ViewGroup = "episode"
		mc.ViewMode = viewModeList
	default:
		mc.ViewMode = viewModeList
	}
	kids = s.visibleTitles(pr.User, kids)
	lo, hi := pr.window(len(kids))
	page := kids[lo:hi]
	mc.Metadata = s.renderTitles(ctx, pr, page, renderOpts{media: true, inSection: true})
	if parent.Type == core.TypeShow {
		s.applyLeafCounts(ctx, pr, mc.Metadata, page, nil)
		if allLeaves != nil {
			viewed, leaves := 0, 0
			for _, m := range mc.Metadata {
				if m.ViewedLeafCount != nil {
					viewed += *m.ViewedLeafCount
				}
				if m.LeafCount != nil {
					leaves += *m.LeafCount
				}
			}
			allLeaves.ViewedLeafCount = model.Ptr(viewed)
			if lo == 0 && hi == len(kids) {
				// Every season is on this page: total the same per-season
				// counts the viewed total comes from.
				allLeaves.LeafCount = model.Ptr(leaves)
			}
			mc.Directory = []model.Directory{*allLeaves}
		}
	}
	mc.Size = len(mc.Metadata) + len(mc.Directory)
	if pr.HasPage {
		mc.SetPage(lo, len(kids))
	}
	s.write(w, r, mc)
}

func (s *Server) handleAllLeaves(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	parent, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, parent) {
		err = core.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var eps []core.Title
	switch parent.Type {
	case core.TypeShow:
		eps, err = s.metadataGrandchildren(ctx, pr, k)
	case core.TypeSeason:
		eps, err = s.metadataChildren(ctx, pr, k)
	default:
		eps = []core.Title{*parent}
	}
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(parent.Type)
	mc.Key = parent.Key.String()
	mc.Title1 = parent.Title
	mc.Title2 = "All Episodes"
	mc.ViewGroup = "episode"
	mc.ViewMode = viewModeList
	mc.Mixed = model.Ptr(true)
	// Filter before paging, as handleChildren does, so the window and
	// totalSize count only episodes the viewer may see.
	eps = s.visibleTitles(pr.User, eps)
	lo, hi := pr.window(len(eps))
	mc.Metadata = s.renderTitles(ctx, pr, eps[lo:hi], renderOpts{media: true, inSection: true})
	mc.Size = len(mc.Metadata)
	if pr.HasPage {
		mc.SetPage(lo, len(eps))
	}
	s.write(w, r, mc)
}

func (s *Server) handleParent(w http.ResponseWriter, r *http.Request) {
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	up := k.Parent()
	if s.d.EpisodeOrder != nil && k.Kind() == core.KindEpisode {
		if n, _, _, ok := s.d.EpisodeOrder.Position(r.Context(), FromContext(r.Context()).User, k); ok {
			up = core.SeasonKey(k.TMDB(), n)
		}
	}
	if strings.HasSuffix(r.URL.Path, "/grandparent") {
		up = up.Parent()
	}
	if up == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.renderSingle(w, r, up)
}

// renderSingle writes one item as a detail container.
func (s *Server) renderSingle(w http.ResponseWriter, r *http.Request, k core.RatingKey) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, err := s.metadataTitle(ctx, pr, k)
	if err == nil && !s.visibleTitle(ctx, pr.User, t) {
		err = core.ErrNotFound
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(t.Type)
	mc.Metadata = []model.Metadata{s.detail(ctx, pr, t, false, false, false, false, "")}
	mc.Size = 1
	s.write(w, r, mc)
}

func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	limit := 20
	if n, _ := strconv.Atoi(r.URL.Query().Get("count")); n > 0 {
		limit = min(n, 100)
	}
	items, err := s.d.Catalog.Similar(ctx, k, limit)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	mc := libraryContainer()
	mc.Metadata = s.renderTitles(ctx, pr, s.filterAllowed(pr.User, items), renderOpts{media: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleItemOnDeck is /library/metadata/{show}/onDeck.
func (s *Server) handleItemOnDeck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	mc := libraryContainer()
	if k.Kind() == core.KindShow {
		show, err := s.metadataTitle(ctx, pr, k)
		if err == nil && !s.visibleTitle(ctx, pr.User, show) {
			err = core.ErrNotFound
		}
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if ep, st := s.showOnDeck(ctx, pr, k, nil); ep != nil {
			mc.Metadata = s.renderTitles(ctx, pr, []core.Title{*ep}, renderOpts{media: true, state: st})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Collections

func (s *Server) collection(ctx context.Context, u *core.User, k core.RatingKey) (*core.Collection, error) {
	c, err := s.collectionRaw(ctx, u, k)
	if err != nil {
		return nil, err
	}
	if c.Upcoming {
		items, err := s.scopedCollectionItems(ctx, u, k)
		if err != nil {
			return nil, err
		}
		c.ChildCount = len(items)
	}
	return c, nil
}

func (s *Server) collectionRaw(ctx context.Context, u *core.User, k core.RatingKey) (*core.Collection, error) {
	if s.d.Collections == nil {
		return nil, core.ErrNotFound
	}
	c, err := s.d.Collections.Get(ctx, u, k)
	if err != nil {
		return nil, err
	}
	if !s.collectionVisible(u, c) {
		return nil, core.ErrNotFound
	}
	return c, nil
}

func (s *Server) scopedCollectionItems(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
	pr := FromContext(ctx)
	p, ok := s.d.Collections.(core.CollectionPlaceholder)
	if !ok || !p.AllowsPlaceholders(ctx, u, k, core.ClassifyDevice(pr.Product, pr.Platform, pr.Device)) {
		return nil, core.ErrNotFound
	}
	page, err := s.d.Collections.Items(ctx, u, k, 0, 5000)
	if err != nil {
		return nil, err
	}
	return s.visibility().UpcomingItems(ctx, u, page.Items, s.now), nil
}

func (s *Server) lookupPlaceholder(ctx context.Context, pr *Request, key core.RatingKey, raw string) (*core.Title, core.RatingKey, error) {
	p, ok := s.d.Collections.(core.CollectionPlaceholder)
	if !ok {
		return nil, 0, core.ErrNotFound
	}
	device := core.ClassifyDevice(pr.Product, pr.Platform, pr.Device)
	var title *core.Title
	var ck core.RatingKey
	var err error
	if raw != "" {
		ck, err = core.ParseRatingKey(raw)
		if err == nil {
			title, err = p.Placeholder(ctx, pr.User, ck, key, device)
		}
	} else {
		title, ck, err = p.FindPlaceholder(ctx, pr.User, key, device)
	}
	if err != nil || title == nil || !s.titleVisible(pr.User, title) {
		return nil, 0, core.ErrNotFound
	}
	if _, err = s.collectionRaw(ctx, pr.User, ck); err != nil {
		return nil, 0, core.ErrNotFound
	}
	return title, ck, nil
}

func (s *Server) scopedCollectionCounts(ctx context.Context, u *core.User, cs []core.Collection) []core.Collection {
	out := make([]core.Collection, 0, len(cs))
	for _, c := range cs {
		if c.Upcoming {
			items, err := s.scopedCollectionItems(ctx, u, c.Key)
			if err != nil || len(items) == 0 {
				continue
			}
			c.ChildCount = len(items)
		}
		out = append(out, c)
	}
	return out
}

// collectionVisible reports whether u may see the section a movie or show
// collection belongs to (mixed collections have no single section).
func (s *Server) collectionVisible(u *core.User, c *core.Collection) bool {
	return s.visibility().CollectionAllowed(u, c)
}

func (s *Server) collectionMetadata(c *core.Collection) model.Metadata {
	rk := c.Key.String()
	m := model.Metadata{
		RatingKey:  rk,
		Key:        "/library/collections/" + rk + "/children",
		GUID:       "collection://" + rk,
		Type:       "collection",
		Title:      c.Title,
		Subtype:    string(c.Type),
		Summary:    c.Summary,
		Index:      model.Ptr(0),
		Thumb:      "/library/metadata/" + rk + "/thumb/" + collectionStamp(c),
		ChildCount: model.Ptr(c.ChildCount),
		AddedAt:    s.now().Unix(),
		UpdatedAt:  s.now().Unix(),
	}
	if c.BackdropPath != "" {
		m.Art = "/library/metadata/" + rk + "/art/" + collectionStamp(c)
	}
	// PMS reports a collection's smart flag as the string "1"/"0".
	smart := "0"
	if c.Kind != "user" && c.Kind != "franchise" {
		smart = "1"
	}
	m.Extra = []model.Attr{model.A("smart", smart)}
	if sec, ok := s.sectionFor(c.Type); ok {
		m.LibrarySectionID = sec.ID
		m.LibrarySectionKey = "/library/sections/" + strconv.Itoa(sec.ID)
		m.LibrarySectionTitle = sec.Title
	}
	m.Image = []model.Image{{Alt: c.Title, Type: "coverPoster", URL: m.Thumb}}
	if m.Art != "" {
		m.Image = append(m.Image, model.Image{Alt: c.Title, Type: "background", URL: m.Art})
	}
	m.UltraBlurColors = s.ultraBlurFor(c.Key, c.BackdropPath)
	return m
}

func (s *Server) sectionCollections(w http.ResponseWriter, r *http.Request, sec Section, sub string) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := s.sectionContainer(sec)
	_, mc.Title2 = sectionTypeTitle(sec)
	mc.ViewGroup = string(sec.Type)
	var all []core.Collection
	if s.d.Collections != nil {
		cs, err := s.d.Collections.List(ctx, pr.User, sec.Type)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		all = cs
	}
	all = s.scopedCollectionCounts(ctx, pr.User, all)
	lo, hi := pr.window(len(all))
	for i := lo; i < hi; i++ {
		mc.Metadata = append(mc.Metadata, s.collectionMetadata(&all[i]))
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(lo, len(all))
	s.write(w, r, mc)
}

// handleCollections is /library/collections (all sections).
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := libraryContainer()
	if s.d.Collections != nil {
		for _, sec := range s.userSections(pr.User) {
			cs, err := s.d.Collections.List(ctx, pr.User, sec.Type)
			if err != nil {
				s.writeErr(w, r, err)
				return
			}
			cs = s.scopedCollectionCounts(ctx, pr.User, cs)
			for i := range cs {
				mc.Metadata = append(mc.Metadata, s.collectionMetadata(&cs[i]))
			}
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	c, err := s.collection(ctx, FromContext(ctx).User, k)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(c.Type)
	mc.Metadata = []model.Metadata{s.collectionMetadata(c)}
	mc.Size = 1
	s.write(w, r, mc)
}

func (s *Server) handleCollectionItems(w http.ResponseWriter, r *http.Request) {
	k, err := keyParam(r)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.collectionItems(w, r, k)
}

func (s *Server) collectionItems(w http.ResponseWriter, r *http.Request, k core.RatingKey) {
	ctx := r.Context()
	pr := FromContext(ctx)
	c, err := s.collectionRaw(ctx, pr.User, k)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	offset, size := pr.pageOf(0)
	var page core.Page
	if c.Upcoming {
		items, e := s.scopedCollectionItems(ctx, pr.User, k)
		if e != nil {
			s.writeErr(w, r, e)
			return
		}
		start := min(offset, len(items))
		end := min(start+size, len(items))
		page = core.Page{Items: items[start:end], Total: len(items)}
	} else {
		page, err = s.d.Collections.Items(ctx, pr.User, k, offset, size)
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.itemContainer(c.Type)
	mc.Key = k.String()
	mc.Title1 = c.Title
	mc.Title2 = c.Title
	mc.Summary = c.Summary
	mc.Thumb = "/library/metadata/" + k.String() + "/thumb/" + collectionStamp(c)
	mc.ViewGroup = string(c.Type)
	mc.ViewMode = viewModeList
	if !c.Upcoming {
		mc.Metadata = s.renderTitles(ctx, pr, s.filterAllowed(pr.User, page.Items), renderOpts{media: true})
	} else {
		for i := range page.Items {
			t := &page.Items[i]
			if !t.Released.IsZero() && t.Released.After(s.now()) {
				mc.Metadata = append(mc.Metadata, s.placeholderMetadata(ctx, pr, t, k))
			} else {
				mc.Metadata = append(mc.Metadata, s.renderTitles(ctx, pr, []core.Title{*t}, renderOpts{media: true})...)
			}
		}
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, page.Total)
	s.write(w, r, mc)
}

func (s *Server) placeholderMetadata(ctx context.Context, pr *Request, t *core.Title, collection core.RatingKey) model.Metadata {
	md := s.metadata(ctx, pr, t, renderOpts{})
	md.Key = "/library/metadata/" + t.Key.String() + "?collection=" + collection.String()
	// Future placeholders use base TMDB artwork through the collection-scoped
	// image handler; overlays and custom poster providers require released titles.
	md.Thumb = thumbURL(t)
	md.Art = artURL(t)
	if md.Thumb != "" {
		md.Thumb += "?collection=" + collection.String()
	}
	if md.Art != "" {
		md.Art += "?collection=" + collection.String()
	}
	md.Image = nil
	md.Summary = "Scheduled release: " + t.Released.Format("2006-01-02") + ". " + md.Summary
	return md
}
