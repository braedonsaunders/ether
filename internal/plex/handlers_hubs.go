package plex

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// hubCtx says where a hub list came from, so each hub's "key" can fetch
// the full row again.
type hubCtx struct {
	section int
	related core.RatingKey
}

// hubSpec is a hub plus the PMS presentation details Ether generates for
// its own rows (key, promoted flag, watch state).
type hubSpec struct {
	core.Hub
	key        string // overrides the computed key
	noPromoted bool   // omit the promoted attribute (section rows)
	// included stamps includedAt on the items, as PMS's
	// /hubs/continueWatching does.
	included bool
	// plain keeps episodes in list form (the Home hero row); other rows
	// carry episodes in full, as PMS does.
	plain bool
	state map[core.RatingKey]core.WatchStatus
}

func hubItemsKey(h core.Hub, hc hubCtx) string {
	if h.Key != 0 {
		return "/library/collections/" + h.Key.String() + "/children"
	}
	k := "/hubs/items/" + url.PathEscape(h.ID)
	q := url.Values{}
	if hc.section > 0 {
		q.Set("contentDirectoryID", strconv.Itoa(hc.section))
	}
	if hc.related != 0 {
		q.Set("metadataID", hc.related.String())
	}
	if len(q) > 0 {
		k += "?" + q.Encode()
	}

	return k
}

// Apply source suitability to discovery, including mixed Home shelves. Personal
// saved lists and playback state must never disappear because sources change.
func discoveryAvailabilityHub(id string) bool {
	for _, keep := range []string{"continue", "inprogress", "ondeck", "upnext", "watchlist", "playlist", "collection"} {
		if strings.Contains(strings.ToLower(id), keep) {
			return false
		}
	}
	return strings.HasPrefix(id, "home.") || strings.HasPrefix(id, "movie.") || strings.HasPrefix(id, "availability.") || strings.Contains(id, "recommend") || strings.Contains(id, "trending") || strings.Contains(id, "popular")
}

func (s *Server) renderHub(ctx context.Context, pr *Request, hs hubSpec, hc hubCtx) model.Hub {
	h := hs.Hub
	items := s.filterAllowed(pr.User, h.Items)
	if s.d.PrioritizeAvailable != nil && discoveryAvailabilityHub(h.ID) {
		items = s.d.PrioritizeAvailable(ctx, items)
	}
	if strings.HasSuffix(h.ID, ".ondeck") || h.ID == "tv.upnext" {
		kept := items[:0:0]
		for i := range items {
			if !s.sameDayUnavailable(ctx, pr, &items[i]) {
				kept = append(kept, items[i])
			}
		}
		items = kept
	}
	// Related hubs (detail pages, postplay) carry Guid children and omit the
	// items' library section and the hub's promoted flag, like PMS.
	related := hc.related != 0
	md := s.renderTitles(ctx, pr, items, renderOpts{media: true, state: hs.state, inSection: related, guids: related, detailEpisodes: !hs.plain})
	if hs.included {
		stampIncluded(md)
	}
	keys := make([]string, 0, len(items))
	for _, t := range items {
		keys = append(keys, t.Key.String())
	}
	typ := string(h.Type)
	if typ == "" {
		typ = "mixed"
	}
	style := h.Style
	if style == "" {
		style = "shelf"
	}
	ctxName := h.Context
	if ctxName == "" {
		ctxName = "hub." + h.ID
	}
	key := hs.key
	if key == "" {
		key = hubItemsKey(h, hc)
	}
	mh := model.Hub{
		Key:           key,
		Title:         h.Title,
		Type:          typ,
		HubIdentifier: h.ID,
		Context:       ctxName,
		Size:          len(md),
		More:          h.More,
		Style:         style,
		Metadata:      md,
	}
	if !hs.noPromoted && !related {
		mh.Promoted = model.Ptr(h.Promoted)
	}
	if len(keys) > 0 {
		mh.HubKey = "/library/metadata/" + strings.Join(keys, ",")
	}
	return mh
}

// stampIncluded sets includedAt (when an item joined Continue Watching) to
// its last view, which is when Ether's in-progress list picked it up.
func stampIncluded(md []model.Metadata) {
	for i := range md {
		if md[i].LastViewedAt > 0 {
			md[i].IncludedAt = md[i].LastViewedAt
		}
	}
}

func (s *Server) renderHubs(ctx context.Context, pr *Request, hubs []core.Hub, hc hubCtx) []model.Hub {
	out := make([]model.Hub, 0, len(hubs))
	for _, h := range hubs {
		out = append(out, s.renderHub(ctx, pr, hubSpec{Hub: h}, hc))
	}
	return out
}

func countParam(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && n > 0 {
		return min(n, 200)
	}
	return def
}

// Continue Watching variants, as PMS names them.
type cwVariant int

const (
	cwHome    cwVariant = iota // /hubs, /hubs/promoted
	cwGlobal                   // /hubs/continueWatching
	cwSection                  // /hubs/sections/{id}
)

// continueHub builds a Continue Watching hub.
func (s *Server) continueHub(ctx context.Context, pr *Request, v cwVariant, sec Section, count int) hubSpec {
	t := core.MediaType("")
	if v == cwSection {
		t = sec.Type
	}
	items, state := s.inProgressTitles(ctx, pr, t, count+1)
	items = s.discoveryContinue(ctx, pr, items)
	hs := hubSpec{Hub: core.Hub{Title: "Continue Watching", Style: "hero", Promoted: true}, state: state}
	switch v {
	case cwHome:
		hs.ID, hs.Context, hs.key, hs.plain = "home.continue", "hub.home.continue", "/hubs/home/continueWatching", true
	case cwGlobal:
		hs.ID, hs.Context, hs.key, hs.noPromoted = "continueWatching", "hub.continueWatching", "/hubs/continueWatching/items", true
		hs.included = true
	case cwSection:
		id := strconv.Itoa(sec.ID)
		hs.Style, hs.noPromoted = "shelf", true
		hs.key = "/hubs/sections/" + id + "/continueWatching/items"
		if sec.Type == core.TypeShow {
			hs.ID, hs.Context, hs.Type = "tv.inprogress."+id, "hub.tv.inprogress", core.TypeEpisode
		} else {
			hs.ID, hs.Context, hs.Type = "movie.inprogress."+id, "hub.movie.inprogress", core.TypeMovie
		}
	}
	if len(items) > count {
		items, hs.More = items[:count], true
	}
	hs.Items = items
	return hs
}

func (s *Server) recentlyAddedHub(ctx context.Context, pr *Request, sec Section, count int) (hubSpec, error) {
	recent := experience.Recent{Visibility: s.visibility(), Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) }}
	items, more, err := recent.Added(ctx, pr.User, sec.Type, count, s.d.Catalog.Browse)
	if err != nil {
		return hubSpec{}, err
	}
	id := strconv.Itoa(sec.ID)
	hs := hubSpec{Hub: core.Hub{Title: "Recently Added in " + sec.Title, Type: sec.Type, Style: "shelf", Promoted: true, Items: items}}
	if sec.Type == core.TypeShow {
		hs.ID, hs.Context, hs.Type = "tv.recentlyadded."+id, "hub.tv.recentlyadded", ""
		hs.key = "/hubs/home/recentlyAdded?type=2&sectionID=" + id
	} else {
		hs.ID, hs.Context = "movie.recentlyadded."+id, "hub.movie.recentlyadded"
		hs.key = "/library/sections/" + id + "/all?sort=addedAt:desc"
	}
	hs.More = more
	return hs, nil
}

// parseSectionList parses "1,2" (contentDirectoryID).
func parseSectionList(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// hubInSections reports whether a hub's items belong to one of the
// sections (mixed hubs always match).
func (s *Server) hubInSections(h core.Hub, ids []int) bool {
	if len(ids) == 0 || h.Type == "" {
		return true
	}
	sec, ok := s.sectionFor(h.Type)
	if !ok {
		return true
	}
	return containsInt(ids, sec.ID)
}

func (s *Server) homeHubs(w http.ResponseWriter, r *http.Request, promotedOnly bool) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	count := countParam(r, 12)
	sections := parseSectionList(q.Get("contentDirectoryID"))
	hc := s.hubContext(pr, 0)
	mc := hubContainer()
	var hubs []core.Hub
	if s.d.Recommender != nil {
		var err error
		if hubs, err = s.recommendHome(ctx, pr, hc, count); err != nil {
			s.log.Warn("home hubs", "err", err)
		}
	}
	assembly := experience.HomeAssembly[model.Hub]{
		Render:      func(hub core.Hub) model.Hub { return s.renderHub(ctx, pr, hubSpec{Hub: hub}, hubCtx{}) },
		Identity:    func(row model.Hub) string { return row.HubIdentifier },
		InLibraries: func(hub core.Hub) bool { return s.hubInSections(hub, sections) },
		Continue: func() (core.Hub, model.Hub) {
			spec := s.continueHub(ctx, pr, cwHome, Section{}, count)
			return spec.Hub, s.renderHub(ctx, pr, spec, hubCtx{})
		},
		Playlists: func() (model.Hub, bool) { hub, _, ok := s.playlistsHubProjection(ctx, pr, count); return hub, ok },
		Discovery: func() []core.Hub {
			var hubs []core.Hub
			for _, spec := range s.discoveryHubs(ctx, pr, count) {
				hubs = append(hubs, spec.Hub)
			}
			return hubs
		},
		Live: func() (model.Hub, bool) {
			hub, _ := s.liveTVHubProjection(r)
			if hub == nil {
				return model.Hub{}, false
			}
			return *hub, true
		},
		Select: func(rows []model.Hub) []model.Hub {
			if ids := q["identifier"]; len(ids) > 0 {
				return filterHubs(rows, ids)
			}
			return rows
		},
		Layout: func(rows []model.Hub) []model.Hub { return s.finalizeHomeRows(ctx, pr, hc, "home", rows) },
	}
	rows, served := assembly.Rows(hubs, q.Get("excludeContinueWatching") != "1", promotedOnly, len(sections) == 0, homePurpose)
	mc.Hub = rows
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
	s.recordImpressions(pr, hc, mc.Hub, served)
}

// homePurpose translates producer identifiers at the Plex boundary; no wire
// context or hub identifier is included in the neutral candidate.
func homePurpose(h core.Hub) core.RowPurpose {
	id := strings.ToLower(h.ID)
	switch {
	case id == "home.continue":
		return "continue"
	case strings.Contains(id, "upnext") || strings.Contains(id, "ondeck"):
		return "nextUp"
	case strings.Contains(id, "watchlist"):
		return "watchlist"
	case h.Key != 0 || strings.Contains(id, "collection"):
		return "collection"
	case strings.Contains(id, "recentlyreleased") || strings.Contains(id, "recently.released"):
		return "recentlyReleased"
	case strings.Contains(id, "livetv"):
		return "live"
	default:
		return "recommendation"
	}
}

// hubContext is the personalization context of a hubs request: the
// device class from the client's X-Plex-* headers, and the daypart and
// weekend flag in server local time.
func (s *Server) hubContext(pr *Request, section int) core.HubContext {
	hc := core.NewHubContext(s.now().Local(), pr.Product, pr.Platform, pr.Device)
	hc.SectionID = section
	hc.ClientID = pr.ClientID
	hc.SessionID = pr.SessionID
	return hc
}

// recommendHome asks for Home rows, with the request context when the
// recommender adapts to it.
func (s *Server) recommendHome(ctx context.Context, pr *Request, hc core.HubContext, count int) ([]core.Hub, error) {
	return (experience.HomeSelection{Source: s.d.Recommender, Visibility: s.visibility(), Restricted: s.viewerRestricted(pr.User)}).Rows(ctx, pr.User, hc, "", count)
}

func (s *Server) recommendSection(ctx context.Context, pr *Request, t core.MediaType, hc core.HubContext, count int) ([]core.Hub, error) {
	return (experience.HomeSelection{Source: s.d.Recommender, Visibility: s.visibility(), Restricted: s.viewerRestricted(pr.User)}).Rows(ctx, pr.User, hc, t, count)
}

// recordImpressions hands the rows actually written, in response order, to
// the recommender's impression sink when it has one. Sinks record
// asynchronously; this never delays the response, which is already sent.
func (s *Server) recordImpressions(pr *Request, hc core.HubContext, written []model.Hub, served map[string]core.Hub) {
	sink, ok := s.d.Recommender.(core.ImpressionSink)
	if !ok || pr.User == nil || len(written) == 0 {
		return
	}
	out := make([]core.Hub, 0, len(written))
	for _, mh := range written {
		if h, ok := served[mh.HubIdentifier]; ok {
			h.Items = s.filterAllowed(pr.User, h.Items)
			out = append(out, h)
		}
	}
	if len(out) > 0 {
		sink.RecordImpressions(pr.User, hc, out)
	}
}

// playlistsHub is PMS's "Recent Playlists" Home row (home.playlists): the
// user's playlists, most recently updated first. Omitted when there are
// none.
func (s *Server) playlistsHub(ctx context.Context, pr *Request, count int) (model.Hub, bool) {
	h, _, ok := s.playlistsHubProjection(ctx, pr, count)
	return h, ok
}

func (s *Server) playlistsHubProjection(ctx context.Context, pr *Request, count int) (model.Hub, frontend.Row, bool) {
	pls, more, err := experience.RecentPlaylists(ctx, pr.User, s.d.Playlists, count)
	if err != nil {
		s.log.Warn("playlists hub", "err", err)
		return model.Hub{}, frontend.Row{}, false
	}
	if len(pls) == 0 {
		return model.Hub{}, frontend.Row{}, false
	}
	h := model.Hub{
		Key:           "/playlists/all?type=15&sort=lastViewedAt:desc&playlistType=video,audio",
		Title:         "Recent Playlists",
		Type:          "playlist",
		HubIdentifier: "home.playlists",
		Context:       "hub.home.playlists",
		More:          more,
		Style:         "shelf",
		Promoted:      model.Ptr(true),
	}
	row := frontend.Row{ID: "home.playlists", Title: "Recent Playlists", Purpose: core.RowPurpose("playlist"), Style: "shelf", Promoted: true, More: more}
	keys := make([]string, 0, len(pls))
	for i := range pls {
		h.Metadata = append(h.Metadata, s.playlistMetadata(ctx, &pls[i]))
		if pls[i].UserID == pr.User.ID {
			p := pls[i]
			p.Items = nil
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryPlaylist, Playlist: &p})
		}
		keys = append(keys, pls[i].Key.String())
	}
	h.Size = len(h.Metadata)
	h.HubKey = "/library/metadata/" + strings.Join(keys, ",")
	return h, row, true
}

func filterHubs(hs []model.Hub, ids []string) []model.Hub {
	want := map[string]bool{}
	for _, v := range ids {
		for _, id := range strings.Split(v, ",") {
			want[id] = true
		}
	}
	var out []model.Hub
	for _, h := range hs {
		if want[h.HubIdentifier] {
			out = append(out, h)
		}
	}
	return out
}

func (s *Server) handleHubs(w http.ResponseWriter, r *http.Request)         { s.homeHubs(w, r, false) }
func (s *Server) handlePromotedHubs(w http.ResponseWriter, r *http.Request) { s.homeHubs(w, r, true) }

func (s *Server) handleContinueWatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := hubContainer()
	mc.Hub = []model.Hub{s.renderHub(ctx, pr, s.continueHub(ctx, pr, cwGlobal, Section{}, countParam(r, 20)), hubCtx{})}
	mc.Size = 1
	s.write(w, r, mc)
}

// handleContinueWatchingItems is the flat item list of Continue Watching
// (/hubs/continueWatching/items, /hubs/home/continueWatching and
// /hubs/sections/{id}/continueWatching/items).
func (s *Server) handleContinueWatchingItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	v, sec := cwGlobal, Section{}
	if id, err := strconv.Atoi(chi.URLParam(r, "section")); err == nil {
		var ok bool
		if sec, ok = s.sectionByID(id); !ok || !s.canSeeSection(pr.User, id) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		v = cwSection
	}
	// A shelf refresh needs the total, not just the visible page. Plex Web
	// treats a missing totalSize as zero and removes the entire row.
	// Keep the existing bounded 200-item resume window, then page it.
	hs := s.continueHub(ctx, pr, v, sec, 200)
	mc := hubContainer()
	mc.Metadata = s.renderTitles(ctx, pr, hs.Items, renderOpts{media: true, state: hs.state, detailEpisodes: !hs.plain})
	if hs.included {
		stampIncluded(mc.Metadata)
	}
	total := len(mc.Metadata)
	start := min(max(pr.Start, 0), total)
	count := countParam(r, 50)
	if pr.HasPage && pr.Size >= 0 {
		count = min(pr.Size, 200)
	}
	end := start + min(count, total-start)
	mc.Metadata = mc.Metadata[start:end]
	mc.Size = len(mc.Metadata)
	mc.SetPage(start, total)
	s.write(w, r, mc)
}

func (s *Server) handleSectionHubs(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	count := countParam(r, 12)
	mc := hubContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.LibrarySectionTitle = sec.Title
	mc.LibrarySectionUUID = sec.UUID
	hc := hubCtx{section: sec.ID}
	pc := s.hubContext(pr, sec.ID)
	served := map[string]core.Hub{}
	add := func(hs hubSpec) {
		mh := s.renderHub(ctx, pr, hs, hc)
		mc.Hub = append(mc.Hub, mh)
		served[mh.HubIdentifier] = hs.Hub
	}
	add(s.continueHub(ctx, pr, cwSection, sec, count))
	if ra, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
		add(ra)
	} else {
		s.log.Warn("recently added hub", "section", sec.ID, "err", err)
	}
	if s.d.Recommender != nil {
		hubs, err := s.recommendSection(ctx, pr, sec.Type, pc, count)
		if err != nil {
			s.log.Warn("section hubs", "section", sec.ID, "err", err)
		}
		for _, h := range hubs {
			add(hubSpec{Hub: h})
		}
	}
	// Mixed discovery rows (watchlist, Rediscover) keep this section's
	// titles.
	for _, hs := range s.discoveryHubs(ctx, pr, count) {
		if h, ok := core.SectionView(hs.Hub, sec.Type); ok {
			hs.Hub = h
			add(hs)
		}
	}
	sectionName := "movies"
	if sec.Type == core.TypeShow {
		sectionName = "shows"
	}
	mc.Hub = s.finalizeHomeRows(ctx, pr, pc, sectionName, mc.Hub)
	s.log.Debug("section hubs rendered", "section", sec.ID, "user", pr.User.ID, "count", count, "rows", len(mc.Hub))
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
	s.recordImpressions(pr, pc, mc.Hub, served)
}

// handleHubItems is /hubs/items?identifier=...: one hub with more items.
func (s *Server) handleHubItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	id := chi.URLParam(r, "identifier")
	if id == "" {
		id = q.Get("identifier")
	}
	count := countParam(r, 50)
	if pr.HasPage && pr.Size > 0 {
		count = min(max(50, min(pr.Start, 200)+min(pr.Size, 200)), 200)
	}
	mc := hubContainer()
	sections := parseSectionList(q.Get("contentDirectoryID"))
	if hs, ok := s.findHub(ctx, pr, id, sections, q.Get("metadataID"), count); ok {
		hc := hubCtx{}
		if len(sections) == 1 {
			hc.section = sections[0]
		}
		if k, err := core.ParseRatingKey(q.Get("metadataID")); err == nil {
			hc.related = k
		}
		mc.Metadata = s.renderHub(ctx, pr, hs, hc).Metadata
	}
	total := len(mc.Metadata)
	start := min(max(pr.Start, 0), total)
	end := total
	if pr.HasPage && pr.Size > 0 && pr.Size < total-start {
		end = start + pr.Size
	}
	mc.Metadata = mc.Metadata[start:end]
	mc.Size = len(mc.Metadata)
	mc.SetPage(start, total)
	s.log.Debug("shelf items rendered", "id", id, "user", pr.User.ID, "start", pr.Start, "requested", pr.Size, "size", mc.Size, "total", total)
	s.write(w, r, mc)
}

// sectionFromHubID parses the trailing section id of "movie.inprogress.1".
func (s *Server) sectionFromHubID(id string) (Section, bool) {
	i := strings.LastIndexByte(id, '.')
	if i < 0 {
		return Section{}, false
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return Section{}, false
	}
	return s.sectionByID(n)
}

func (s *Server) findHub(ctx context.Context, pr *Request, id string, sections []int, metadataID string, count int) (hubSpec, bool) {
	switch {
	case id == "tv.upnext":
		k, err := core.ParseRatingKey(metadataID)
		if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
			return hubSpec{}, false
		}
		return s.upNextHub(ctx, pr, k)
	case id == "home.continue":
		return s.continueHub(ctx, pr, cwHome, Section{}, count), true
	case id == "continueWatching":
		return s.continueHub(ctx, pr, cwGlobal, Section{}, count), true
	case strings.HasPrefix(id, "movie.inprogress.") || strings.HasPrefix(id, "tv.inprogress."):
		if sec, ok := s.sectionFromHubID(id); ok && s.canSeeSection(pr.User, sec.ID) {
			return s.continueHub(ctx, pr, cwSection, sec, count), true
		}
		return hubSpec{}, false
	case strings.HasPrefix(id, "movie.recentlyadded.") || strings.HasPrefix(id, "tv.recentlyadded."):
		if sec, ok := s.sectionFromHubID(id); ok && s.canSeeSection(pr.User, sec.ID) {
			if hs, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
				return hs, true
			}
		}
		return hubSpec{}, false
	}
	for _, h := range s.discoveryHubs(ctx, pr, count) {
		if h.ID != id || !s.hubInSections(h.Hub, sections) {
			continue
		}
		if len(sections) == 1 {
			// A section's copy of a mixed row ("More" from a library).
			if sec, ok := s.sectionByID(sections[0]); ok {
				if v, ok := core.SectionView(h.Hub, sec.Type); ok {
					h.Hub = v
				}
			}
		}
		return h, true
	}
	if s.d.Recommender == nil {
		return hubSpec{}, false
	}
	if k, err := core.ParseRatingKey(metadataID); err == nil && s.relatedSourceVisible(ctx, pr, k) {
		if hubs, err := s.d.Recommender.Related(ctx, pr.User, k, count); err == nil {
			for _, h := range hubs {
				if h.ID == id {
					return hubSpec{Hub: h}, true
				}
			}
		}
	}
	if hubs, err := s.recommendHome(ctx, pr, s.hubContext(pr, 0), count); err == nil {
		for _, h := range hubs {
			if h.ID == id {
				return hubSpec{Hub: h}, true
			}
		}
	}
	for _, sec := range s.userSections(pr.User) {
		if len(sections) > 0 && !containsInt(sections, sec.ID) {
			continue
		}
		if hubs, err := s.recommendSection(ctx, pr, sec.Type, s.hubContext(pr, sec.ID), count); err == nil {
			for _, h := range hubs {
				if h.ID == id {
					return hubSpec{Hub: h}, true
				}
			}
		}
	}
	return hubSpec{}, false
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// relatedSourceVisible reports whether the viewer may see the title a
// related/post-play/similar request is about: its rows are built from the
// title's own credits and collection, and are not the viewer's to see when
// the title is hidden. Non-title keys are left to their own handlers.
func (s *Server) relatedSourceVisible(ctx context.Context, pr *Request, k core.RatingKey) bool {
	return s.detailRelated(ctx, pr).SourceVisible(ctx, pr.User, k)
}

func (s *Server) detailRelated(ctx context.Context, pr *Request) experience.DetailRelated {
	return experience.DetailRelated{Source: s.d.Recommender, Visibility: s.visibility(), Get: func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		return s.metadataTitle(ctx, pr, key)
	}}
}

// handleMetadataHubs is /hubs/metadata/{key}[/related] and
// /library/metadata/{key}/related.
func (s *Server) handleMetadataHubs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	mc := hubContainer()
	if s.d.Recommender != nil {
		hubs, err := s.detailRelated(ctx, pr).Rows(ctx, pr.User, k, countParam(r, 12))
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			s.log.Debug("related hubs", "key", k, "err", err)
		}
		mc.Hub = s.renderHubs(ctx, pr, hubs, hubCtx{related: k})
	}
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// upNextHub is shared by post-play and the advertised shelf-items URL so
// clients can reload the row or use it as a play-queue source.
func (s *Server) upNextHub(ctx context.Context, pr *Request, k core.RatingKey) (hubSpec, bool) {
	row := experience.NextUp(ctx, pr.User, k, func(ctx context.Context, key core.RatingKey) (*core.Title, error) {
		return s.d.Catalog.NextEpisode(ctx, key)
	}, s.visibility())
	if len(row.Items) == 0 {
		return hubSpec{}, false
	}
	return hubSpec{Hub: core.Hub{ID: "tv.upnext", Title: "Up Next", Type: core.TypeEpisode,
		Context: "hub.tv.upnext", Items: row.Items}}, true
}

// handlePostplay is /hubs/metadata/{key}/postplay. An episode with a
// next episode keeps Up Next first; everything else leads with the
// viewer's personalized hook queue (#30, the same #1 pick prefetch
// pre-buffered), then the related rows. Without a PostPlayer this is the
// old behaviour: Up Next, then related.
func (s *Server) handlePostplay(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	count := countParam(r, 12)
	mc := hubContainer()
	mc.Hub, err = experience.AssemblePostPlay(ctx, s.postPlaySelection(ctx, pr, k, !s.viewerRestricted(pr.User)), pr.User, k, count,
		func() (core.RatingKey, model.Hub, bool) {
			hub, ok := s.upNextHub(ctx, pr, k)
			if !ok {
				return 0, model.Hub{}, false
			}
			return hub.Items[0].Key, s.renderHub(ctx, pr, hub, hubCtx{related: k}), true
		}, func(row experience.PostPlayRow) (model.Hub, bool) { return s.postPlayHub(ctx, pr, k, row) },
		func(hub core.Hub) model.Hub { return s.renderHub(ctx, pr, hubSpec{Hub: hub}, hubCtx{related: k}) })
	if err != nil {
		s.log.Debug("postplay related hubs", "key", k, "err", err)
	}
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Search

func (s *Server) searchTypes(u *core.User, sectionID int) []core.MediaType {
	var out []core.MediaType
	for _, sec := range s.userSections(u) {
		if sectionID > 0 && sec.ID != sectionID {
			continue
		}
		out = append(out, sec.Type)
	}
	return out
}

func (s *Server) runSearch(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]core.Title, error) {
	search := experience.Search{
		Visibility:   s.visibility(),
		Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) },
		Natural: func(ctx context.Context, _ *core.User, query string, limit int) ([]core.Title, bool) {
			return s.discoverySearch(ctx, pr, query, limit)
		},
		Catalog: func(ctx context.Context, query string, types []core.MediaType, limit int) ([]core.Title, error) {
			return s.d.Catalog.Search(ctx, query, types, limit)
		},
	}
	return search.Find(ctx, pr.User, query, s.searchTypes(pr.User, sectionID), limit)
}

func (s *Server) vibeSearch(ctx context.Context, pr *Request, query string) (*core.Collection, []core.Title, bool, error) {
	searcher := s.d.VibeSearch
	if searcher == nil {
		searcher, _ = s.d.Collections.(core.VibeSearcher)
	}
	if searcher == nil || pr == nil {
		return nil, nil, false, nil
	}
	return (experience.VibeSearch{Source: searcher, Visibility: s.visibility(), Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) }}).Find(ctx, pr.User, query)
}

func (s *Server) collectionDirectory(ctx context.Context, c *core.Collection) model.Directory {
	m := s.collectionMetadata(c)
	return model.Directory{
		Key: "/library/collections/" + c.Key.String() + "/children", Type: "collection",
		Title: c.Title, Thumb: m.Thumb, Art: m.Art,
		LeafCount: model.Ptr(c.ChildCount),
		Extra:     []model.Attr{model.A("ratingKey", c.Key.String()), model.A("subtype", string(c.Type)), model.A("childCount", c.ChildCount), model.A("librarySectionID", m.LibrarySectionID)},
	}
}

func (s *Server) handleHubSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("query")) == "" {
		// PMS rejects an empty hub search with its HTML 400 page.
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	limit := 10
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
		limit = min(n, 100)
	}
	sectionID, _ := strconv.Atoi(q.Get("sectionId"))
	vibe, vibeItems, isVibe, err := s.vibeSearch(ctx, pr, q.Get("query"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var res []core.Title
	if isVibe {
		res = vibeItems
	} else {
		res, err = s.runSearch(ctx, pr, q.Get("query"), sectionID, limit*2)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	byType := map[core.MediaType][]core.Title{}
	for _, t := range res {
		byType[t.Type] = append(byType[t.Type], t)
	}
	allowed := map[core.MediaType]bool{}
	for _, t := range s.searchTypes(pr.User, sectionID) {
		allowed[t] = true
		if t == core.TypeShow {
			allowed[core.TypeEpisode] = true
		}
	}
	// PMS returns every result hub, empty ones included, in this order.
	mc := &model.MediaContainer{}
	for _, hd := range []struct {
		t     core.MediaType
		title string
	}{{core.TypeMovie, "Movies"}, {core.TypeEpisode, "Episodes"}, {"collection", "Collections"}, {core.TypeShow, "Shows"}} {
		items := byType[hd.t]
		if !allowed[hd.t] {
			items = nil
		}
		more := false
		if len(items) > limit {
			items, more = items[:limit], true
		}
		h := core.Hub{ID: string(hd.t), Title: hd.title, Type: hd.t, Items: items, More: more, Style: "shelf"}
		mh := s.renderHub(ctx, pr, hubSpec{Hub: h, noPromoted: true}, hubCtx{})
		if hd.t == "collection" && isVibe && vibe != nil {
			mh.Directory = []model.Directory{s.collectionDirectory(ctx, vibe)}
			mh.Size = len(mh.Directory)
		}
		mh.Context, mh.Key, mh.HubKey = "", "", ""
		for i := range mh.Metadata {
			mh.Metadata[i].Score = strconv.FormatFloat(searchScore(i, len(mh.Metadata)), 'f', 5, 64)
		}
		mc.Hub = append(mc.Hub, mh)
	}
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// searchScore is a descending relevance score for the i-th of n results.
func searchScore(i, n int) float64 {
	return 1 - float64(i)/float64(n+1)
}

// handleSearch is the legacy flat /search?query=.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	limit := 50
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
		limit = min(n, 200)
	}
	res, err := s.runSearch(ctx, pr, q.Get("query"), 0, limit)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := libraryContainer()
	mc.Metadata = s.renderTitles(ctx, pr, res, renderOpts{media: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleLibrarySearch is /library/search, used by the newer apps: results
// wrapped in SearchResult with a score.
func (s *Server) handleLibrarySearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	limit := 50
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
		limit = min(n, 200)
	}
	vibe, vibeItems, isVibe, err := s.vibeSearch(ctx, pr, q.Get("query"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var res []core.Title
	if isVibe {
		res = vibeItems
	} else {
		res, err = s.runSearch(ctx, pr, q.Get("query"), 0, limit)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	searchCollections := false
	if st := q.Get("searchTypes"); st != "" {
		allow := map[core.MediaType]bool{}
		for _, v := range strings.Split(st, ",") {
			switch strings.TrimSpace(v) {
			case "movies", "movie":
				allow[core.TypeMovie] = true
			case "collections", "collection":
				searchCollections = true
			case "tv", "shows", "show":
				allow[core.TypeShow] = true
				allow[core.TypeEpisode] = true
			}
		}
		if len(allow) == 0 {
			res = nil
		} else {
			filtered := res[:0:0]
			for _, t := range res {
				if allow[t.Type] {
					filtered = append(filtered, t)
				}
			}
			res = filtered
		}
	}
	md := s.renderTitles(ctx, pr, res, renderOpts{media: true})
	mc := &model.MediaContainer{}
	for i := range md {
		score := math.Round(searchScore(i, len(md))*10000) / 10000
		// PMS names the section here but sends no section id or key.
		md[i].LibrarySectionID, md[i].LibrarySectionKey = 0, ""
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: score, Metadata: &md[i]})
	}
	if isVibe && vibe != nil && (searchCollections || q.Get("searchTypes") == "") {
		d := s.collectionDirectory(ctx, vibe)
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 1, Directory: &d})
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}
