package plex

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"golang.org/x/text/unicode/norm"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/homefilter"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

// hubCtx says where a hub list came from, so each hub's "key" can fetch
// the full row again.
type hubCtx struct {
	section int
	related core.RatingKey
	// visible caps the rendered items of a hub whose candidate window is
	// wider (section hubs assemble a stable window for later pages).
	visible int
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

func (s *Server) renderHub(ctx context.Context, pr *Request, hs hubSpec, hc hubCtx) model.Hub {
	h := hs.Hub
	if strings.HasPrefix(h.ID, "movie.recentlyviewed.") || strings.HasPrefix(h.ID, "tv.recentlyviewed.") {
		if sec, ok := s.sectionFromHubID(h.ID); ok {
			if measured, err := s.recentlyWatchedHub(ctx, pr, sec, max(len(h.Items), 1)); err == nil {
				hs = measured
				h = hs.Hub
			}
		}
	}
	build := func() []core.Title {
		if loader, ok := s.d.Catalog.(interface {
			LoadRowFacts(context.Context, []core.Title) []core.Title
		}); ok {
			h.Items = loader.LoadRowFacts(ctx, h.Items)
		}

		return (experience.RowSelection{Visibility: s.visibility(), PrioritizeAvailable: s.d.PrioritizeAvailable,
			SameDayUnavailable: func(ctx context.Context, t *core.Title) bool { return s.sameDayUnavailable(ctx, pr, t) },
		}).Items(ctx, pr.User, h)
	}
	// Filter the section and availability before rendering or paging. The
	// resulting wire list is the canonical generation (including its counts).
	if hc.section != 0 && hs.state == nil && hs.key == "" {
		if sec, ok := s.sectionByID(hc.section); ok {
			if view, ok := core.SectionView(h, sec.Type); ok {
				h = view
			} else {
				h.Items = nil
			}
		}
	}
	related := hc.related != 0
	pinned := hc.section != 0 && hs.state == nil && hs.key == ""
	var sourceItems []core.Title
	render := func() []model.Metadata {
		sourceItems = build()
		if !pinned && hc.visible > 0 && len(sourceItems) > hc.visible {
			sourceItems = sourceItems[:hc.visible]
			h.More = true
		}
		return s.renderTitles(ctx, pr, sourceItems, renderOpts{media: true, state: hs.state, inSection: related, guids: related, detailEpisodes: !hs.plain})
	}
	var md []model.Metadata
	generation := ""
	if pinned {
		h.More = false
		var selected []core.Title
		selected, generation = s.shelfSnapshot(ctx, pr, h, hc, func() ([]core.Title, []model.Metadata) {
			metadata := render()
			return sourceItems, metadata
		})
		// Membership was already evaluated in the generation; refresh presentation
		// (watch badges, artwork enrichment) without reapplying live availability.
		if hc.visible > 0 && len(selected) > hc.visible {
			selected = selected[:hc.visible]
			h.More = true
		}
		md = s.renderTitles(ctx, pr, selected, renderOpts{media: true, availabilityInSQL: true})
		// More is calculated from the canonical generation, not source hints.
	} else {
		md = render()
	}
	if hc.visible > 0 && len(md) > hc.visible {
		md = md[:hc.visible]
		h.More = true
	}
	if hs.included {
		stampIncluded(md)
	}
	keys := make([]string, 0, len(md))
	// Recorded PMS Home and related hubs name exactly their rendered cards.
	for _, item := range md {
		keys = append(keys, item.RatingKey)
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
		Key:           s.shelfURL(key, generation),
		Title:         h.Title,
		Type:          typ,
		HubIdentifier: h.ID,
		Context:       ctxName,
		Size:          len(md),
		More:          h.More,
		Style:         style,
		Metadata:      md,
	}
	if h.ID == "tv.upnext" {
		mh.UpNext = model.Ptr(true)
	}
	if !hs.noPromoted && !related {
		mh.Promoted = model.Ptr(h.Promoted)
	}
	if len(keys) > 0 {
		mh.HubKey = s.shelfURL("/library/metadata/"+strings.Join(keys, ","), generation)
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
	if sec.ID != 0 {
		t = sec.Type
	}
	selector := s.continueExperience(pr)
	if v == cwSection && sec.Type == core.TypeShow {
		selector.Filter = nil
	}
	items, state, more := selector.Shelf(ctx, pr.User, t, s.saferFetchCount(pr.User, count))
	if v == cwSection && sec.Type == core.TypeShow && pr.User != nil {
		if gate, ok := s.d.State.(interface {
			PlaybackCompletedShows(context.Context, int64, []core.RatingKey) (map[core.RatingKey]bool, error)
		}); ok {
			shows := make([]core.RatingKey, 0, len(items))
			for _, item := range items {
				if state[item.Key].ViewOffset <= 0 {
					shows = append(shows, core.ShowKey(item.Key.TMDB()))
				}
			}
			completed, err := gate.PlaybackCompletedShows(ctx, pr.User.ID, shows)
			if err != nil {
				items = nil
			} else {
				out := items[:0]
				for _, item := range items {
					if state[item.Key].ViewOffset > 0 || completed[core.ShowKey(item.Key.TMDB())] {
						out = append(out, item)
					}
				}
				items = out
			}
		}
	}
	if v == cwHome {
		resuming := items[:0]
		for _, item := range items {
			if state[item.Key].ViewOffset > 0 {
				resuming = append(resuming, item)
			}
		}
		items = resuming
	}
	items = s.takeVisible(pr.User, items, count)
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
	hs.More = more
	hs.Items = items
	return hs
}

func (s *Server) recentlyAddedHub(ctx context.Context, pr *Request, sec Section, count int) (hubSpec, error) {
	return s.recentlyAddedHubPage(ctx, pr, sec, count, false)
}

func (s *Server) recentlyAddedHubPage(ctx context.Context, pr *Request, sec Section, count int, noTotal bool) (hubSpec, error) {
	recent := experience.Recent{Visibility: s.visibility(), Unrestricted: func(u *core.User) bool { return !s.viewerRestricted(u) }}
	items, more, err := recent.Added(ctx, pr.User, sec.Type, count, func(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
		q.NoTotal = noTotal
		return s.catalogBrowse(ctx, q)
	})
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
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
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
	route := "hubs"
	if promotedOnly {
		route = "hubs/promoted"
	}
	s.serveHome(w, r, route, func(_ context.Context, r *http.Request) homeBuild { return s.buildHomeHubs(r, promotedOnly) })
}

func (s *Server) buildHomeHubs(r *http.Request, promotedOnly bool) homeBuild {
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	q := r.URL.Query()
	count := countParam(r, 12)
	sections := parseSectionList(q.Get("contentDirectoryID"))
	directory := strings.TrimSpace(q.Get("contentDirectoryID"))
	perDirectory := directory != "" && !strings.Contains(directory, ",")
	first := strings.TrimSpace(strings.Split(q.Get("pinnedContentDirectoryID"), ",")[0])
	if first == "" {
		first = directory
	}
	var directorySection Section
	if perDirectory && directory != "playlists" {
		id, err := strconv.Atoi(directory)
		var ok bool
		if err == nil {
			directorySection, ok = s.sectionByID(id)
		}
		if !ok || !s.canSeeSection(pr.User, id) {
			return homeBuild{mc: hubContainer()}
		}
	}
	hc := s.hubContext(pr, 0)
	mc := hubContainer()
	var hubs []core.Hub
	var sharedDiscovery []core.Hub
	if perDirectory {
		var err error
		var filter *homefilter.Snapshot
		hubs, sharedDiscovery, filter, err = s.sharedHome(ctx, pr, hc, directory, q.Get("pinnedContentDirectoryID"), s.saferFetchCount(pr.User, count), count)
		if filter != nil {
			ctx = homefilter.With(ctx, filter)
		}
		if err != nil {
			s.log.Warn("home hubs", "err", err)
		}
	} else if s.d.Recommender != nil {
		var err error
		if hubs, err = s.recommendHome(ctx, pr, hc, s.saferFetchCount(pr.User, count)); err != nil {
			s.log.Warn("home hubs", "err", err)
		}
	}
	var selected map[string]bool
	if ids := q["identifier"]; len(ids) > 0 {
		selected = map[string]bool{}
		for _, v := range ids {
			for _, id := range strings.Split(v, ",") {
				selected[id] = true
			}
		}
	}
	assembly := experience.HomeAssembly[model.Hub]{
		Render: func(hub core.Hub) model.Hub {
			owner := first
			if hub.ID == "home.playlists" {
				owner = "playlists"
			} else if sec := s.homeDirectoryForHub(hub.ID); sec != "" {
				owner = sec
			}
			if perDirectory && owner != directory {
				// The layout needs the same painted item count, but the client
				// will never receive this row's expensive metadata projection.
				items := (experience.RowSelection{Visibility: s.visibility(), PrioritizeAvailable: s.d.PrioritizeAvailable,
					SameDayUnavailable: func(ctx context.Context, t *core.Title) bool { return s.sameDayUnavailable(ctx, pr, t) },
				}).Items(ctx, pr.User, hub)
				items = s.availableForViewer(ctx, pr.User, s.visibleTitles(pr.User, items))
				typ := string(hub.Type)
				if typ == "" {
					typ = "mixed"
				}
				contextName := hub.Context
				if contextName == "" {
					contextName = "hub." + hub.ID
				}
				return model.Hub{HubIdentifier: hub.ID, Title: hub.Title, Type: typ, Context: contextName, Size: len(items)}
			}
			return s.renderHub(ctx, pr, hubSpec{Hub: hub}, hubCtx{})
		},
		Identity:    func(row model.Hub) string { return row.HubIdentifier },
		Report:      homeLayoutFacts,
		InLibraries: func(hub core.Hub) bool { return perDirectory || s.hubInSections(hub, sections) },
		Continue: func() (core.Hub, model.Hub) {
			spec := s.continueHub(ctx, pr, cwHome, Section{}, count)
			return spec.Hub, s.renderHub(ctx, pr, spec, hubCtx{})
		},
		Playlists: func() (model.Hub, bool) {
			if q.Get("contentDirectoryID") != "" && q.Get("contentDirectoryID") != "playlists" {
				return model.Hub{}, false
			}
			hub, _, ok := s.playlistsHubProjection(ctx, pr, count)
			return hub, ok
		},
		Discovery: func() []core.Hub {
			hubs := append([]core.Hub(nil), sharedDiscovery...)
			if !perDirectory {
				for _, spec := range s.discoveryHubs(ctx, pr, count) {
					hubs = append(hubs, spec.Hub)
				}
			}
			if perDirectory && directorySection.ID != 0 {
				if spec, err := s.recentlyAddedHubPage(ctx, pr, directorySection, count, true); err == nil {
					hubs = append(hubs, spec.Hub)
				}
				if s.d.Recommender != nil {
					sectionHubs, err := s.recommendSection(ctx, pr, directorySection.Type, s.hubContext(pr, directorySection.ID), s.saferFetchCount(pr.User, count))
					if err == nil {
						for _, hub := range sectionHubs {
							if s.homeDirectoryForHub(hub.ID) == directory {
								hubs = append(hubs, hub)
							}
						}
					}
				}
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
		Keep: func(row model.Hub) bool { return selected == nil || selected[row.HubIdentifier] },
		Layout: func(rows []model.Hub, hubs []core.Hub, neutral []core.NeutralHomeRow) []model.Hub {
			return s.arrangeHomeRows(ctx, pr, hc, "home", rows, hubs, neutral)
		},
	}
	rows, served := assembly.Rows(hubs, !metadataOptionOn(q.Get("excludeContinueWatching")), promotedOnly, len(sections) == 0 || perDirectory, experience.HomePurpose)
	// PMS retains separate Home resume and next-up shelves, suppressing
	// the latter only when both nonempty memberships are identical.
	if !metadataOptionOn(q.Get("excludeContinueWatching")) && (selected == nil || selected["home.ondeck"]) {
		spec := s.continueHub(ctx, pr, cwGlobal, Section{}, count)
		episodes := spec.Items[:0]
		for _, item := range spec.Items {
			if item.Type == core.TypeEpisode {
				episodes = append(episodes, item)
			}
		}
		spec.Items = episodes
		spec.ID, spec.Context, spec.Title, spec.key = "home.ondeck", "hub.home.ondeck", "On Deck", "/hubs/home/onDeck"
		spec.included, spec.noPromoted, spec.plain = false, false, false
		ondeck := s.renderHub(ctx, pr, spec, hubCtx{})
		var resume model.Hub
		for _, row := range rows {
			if row.HubIdentifier == "home.continue" {
				resume = row
			}
		}
		same := len(ondeck.Metadata) > 0 && len(ondeck.Metadata) == len(resume.Metadata)
		keys := map[string]bool{}
		for _, item := range resume.Metadata {
			keys[item.RatingKey] = true
		}
		for _, item := range ondeck.Metadata {
			same = same && keys[item.RatingKey]
		}
		filtered := rows[:0]
		for _, row := range rows {
			if row.HubIdentifier != "home.ondeck" {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
		if !same {
			rows = append(rows, ondeck)
		}
	} else {
		filtered := rows[:0]
		for _, row := range rows {
			if row.HubIdentifier != "home.ondeck" {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	if perDirectory {
		var filtered []model.Hub
		for _, row := range rows {
			owner := first
			if row.HubIdentifier == "home.playlists" {
				owner = "playlists"
			} else if section := s.homeDirectoryForHub(row.HubIdentifier); section != "" {
				owner = section
			}
			if owner == directory {
				if row.HubIdentifier != "home.continue" && strings.EqualFold(row.Style, "hero") {
					row.Style = "shelf"
				}
				filtered = append(filtered, row)
			}
		}
		mc.Hub = stableHomeHubs(filtered)
	} else {
		mc.Hub = heroReadyHomeHubs(stableHomeHubs(rows))
	}
	mc.Size = len(mc.Hub)
	return homeBuild{mc: mc, served: served, provisional: s.d.Recommender != nil && !hasRecommendedRows(hubs)}
}

// hasRecommendedRows reports recommender rows beyond the Continue Watching
// placeholder: without them the viewer's model was not ready yet.
func hasRecommendedRows(hubs []core.Hub) bool {
	for _, h := range hubs {
		if h.ID != "home.continue" {
			return true
		}
	}
	return false
}

// Library hub namespaces identify their source independently of the request.
// Everything else belongs to the first pinned directory.
func (s *Server) homeDirectoryForHub(id string) string {
	var typ core.MediaType
	switch {
	case strings.HasPrefix(id, "movie."):
		typ = core.TypeMovie
	case strings.HasPrefix(id, "tv."):
		typ = core.TypeShow
	default:
		return ""
	}
	if sec, ok := s.sectionFor(typ); ok {
		return strconv.Itoa(sec.ID)
	}
	return ""
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

// warmBrowseLink opens at most one already-visible playable after the hub
// response is written: the viewer's own Continue Watching / On Deck item,
// never a recommendation nobody chose. It only resolves and opens the
// cached/debrid link (no opening prefetch), and Prewarm's existing per-user
// / rate budgets still apply.
func (s *Server) warmBrowseLink(ctx context.Context, pr *Request, rows []model.Hub) {
	if pr == nil || pr.User == nil {
		return
	}
	pw, ok := s.d.Playback.(core.Prewarmer)
	if !ok {
		return
	}
	key, ok := firstBrowseWarmKey(rows)
	if !ok {
		return
	}
	pw.Prewarm(store.DetachQueryStats(context.WithoutCancel(ctx)), pr.User, key, s.clientCaps(pr), core.PrewarmOptions{Reason: core.PrewarmBrowse})
}

// firstBrowseWarmKey is the first movie or episode in a row of the viewer's
// own progress (Continue Watching, a section's in-progress row or On Deck).
// Rows of recommendations, trending or anything else are never warmed: each
// warm-up searches the indexers for a title the viewer may never play.
func firstBrowseWarmKey(rows []model.Hub) (core.RatingKey, bool) {
	ownProgress := func(id string) bool {
		id = strings.ToLower(id)
		return strings.Contains(id, "continue") || strings.Contains(id, "inprogress") || strings.Contains(id, "ondeck")
	}
	for _, row := range rows {
		if !ownProgress(row.HubIdentifier) && !ownProgress(row.Context) {
			continue
		}
		for _, md := range row.Metadata {
			if md.Type != "" && md.Type != "movie" && md.Type != "episode" {
				continue
			}
			k, err := core.ParseRatingKey(md.RatingKey)
			if err != nil || !isPlayableKey(k) {
				continue
			}
			return k, true
		}
	}
	return 0, false
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

func (s *Server) playlistsHubProjection(ctx context.Context, pr *Request, count int) (model.Hub, frontend.Row, bool) {
	if pr.User == nil || s.d.Playlists == nil {
		return model.Hub{}, frontend.Row{}, false
	}
	pls, err := s.d.Playlists.List(ctx, pr.User.ID)
	if err != nil {
		return model.Hub{}, frontend.Row{}, false
	}
	owned := pls[:0:0]
	viewed := map[core.RatingKey]int64{}
	for _, pl := range pls {
		if pl.UserID != pr.User.ID {
			continue
		}
		m := s.playlistListingMetadata(ctx, &pl)
		viewed[pl.Key] = m.LastViewedAt
		owned = append(owned, pl)
	}
	pls = owned
	sort.SliceStable(pls, func(i, j int) bool { return viewed[pls[i].Key] > viewed[pls[j].Key] })
	more := len(pls) > count
	if more {
		pls = pls[:count]
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
		h.Metadata = append(h.Metadata, s.playlistListingMetadata(ctx, &pls[i]))
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

func (s *Server) handleHubs(w http.ResponseWriter, r *http.Request)         { s.homeHubs(w, r, false) }
func (s *Server) handlePromotedHubs(w http.ResponseWriter, r *http.Request) { s.homeHubs(w, r, true) }

func (s *Server) handleContinueWatching(w http.ResponseWriter, r *http.Request) {
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	mc := hubContainer()
	sec := Section{}
	if raw := r.URL.Query().Get("contentDirectoryID"); raw != "" {
		var allowed []Section
		seen := map[int]bool{}
		for _, id := range parseSectionList(raw) {
			if candidate, ok := s.sectionByID(id); ok && !seen[id] && s.canSeeSection(pr.User, id) {
				allowed = append(allowed, candidate)
				seen[id] = true
			}
		}
		if len(allowed) == 0 {
			if len(parseSectionList(raw)) > 0 {
				mc.Hub = []model.Hub{s.renderHub(ctx, pr, hubSpec{Hub: core.Hub{ID: "continueWatching", Title: "Continue Watching"}}, hubCtx{})}
				mc.Size = 1
				s.write(w, r, mc)
			} else {
				writeHTTPError(w, http.StatusNotFound)
			}
			return
		}
		if len(allowed) == 1 {
			sec = allowed[0]
		}
	}
	count := countParam(r, 20)
	var spec hubSpec
	if r.URL.Query().Get("contentDirectoryID") != "" {
		// Select a common bounded candidate window before narrowing it to a
		// library. Applying the preference filter to a window already narrowed
		// to count could otherwise leave the combined shelf shorter than a
		// single-library shelf.
		fetch := min(200, count*2+24)
		spec = s.continueHub(ctx, pr, cwGlobal, Section{}, fetch)
		// renderHub also applies availability. Do it before the per-library
		// page limit so unavailable entries cannot displace visible items.
		spec.Items = s.availableForViewer(ctx, pr.User, spec.Items)
		if sec.ID != 0 {
			items := make([]core.Title, 0, len(spec.Items))
			for _, title := range spec.Items {
				if section, ok := s.untitledSectionFor(title.Type); ok && section.ID == sec.ID {
					items = append(items, title)
				}
			}
			spec.Items = items
		}
		spec.More = len(spec.Items) > count || (sec.ID == 0 && spec.More)
		if len(spec.Items) > count {
			spec.Items = spec.Items[:count]
		}
	} else {
		spec = s.continueHub(ctx, pr, cwGlobal, sec, count)
	}
	hub := s.renderHub(ctx, pr, spec, hubCtx{})
	if r.URL.Query().Get("contentDirectoryID") != "" {
		if layout, ok := s.d.Recommender.(interface {
			HomeRowEnabled(context.Context, *core.User, core.HubContext, string) bool
		}); ok && !layout.HomeRowEnabled(ctx, pr.User, s.hubContext(pr, 0), "home.continue") {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
	}
	mc.Hub = []model.Hub{hub}
	mc.Size = 1
	s.write(w, r, mc)
}

// handleContinueWatchingItems is the flat item list of Continue Watching
// (/hubs/continueWatching/items, /hubs/home/continueWatching and
// /hubs/sections/{id}/continueWatching/items).
func (s *Server) handleContinueWatchingItems(w http.ResponseWriter, r *http.Request) {
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	if raw := r.URL.Query().Get("contentDirectoryID"); raw != "" && len(parseSectionList(raw)) == 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	v, sec := cwGlobal, Section{}
	if r.URL.Path == "/hubs/home/continueWatching" {
		v = cwHome
	}
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
	if raw := r.URL.Query().Get("contentDirectoryID"); raw != "" && v != cwSection {
		allowed := map[int]bool{}
		for _, id := range parseSectionList(raw) {
			allowed[id] = true
		}
		filtered := hs.Items[:0]
		for _, item := range hs.Items {
			if section, ok := s.sectionFor(item.Type); ok && allowed[section.ID] {
				filtered = append(filtered, item)
			}
		}
		hs.Items = filtered
	}
	mc := hubContainer()
	mc.Metadata = s.renderTitles(ctx, pr, hs.Items, renderOpts{media: true, state: hs.state, detailEpisodes: !hs.plain})
	if hs.included {
		stampIncluded(mc.Metadata)
	}
	if r.URL.Query().Get("includeMeta") == "1" {
		mc.Meta = &model.Meta{}
		for _, section := range s.userSections(pr.User) {
			mc.Meta.Type = append(mc.Meta.Type, s.metaType(section, section.Type, ""))
		}
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
	if v != cwSection || pr.HasPage {
		mc.SetPage(start, total)
	}
	s.write(w, r, mc)
}

func (s *Server) handleSectionHubs(w http.ResponseWriter, r *http.Request) {
	sec, ok := s.sectionParam(w, r)
	if !ok {
		return
	}
	s.serveHome(w, r, "hubs/sections/"+strconv.Itoa(sec.ID), func(_ context.Context, r *http.Request) homeBuild { return s.buildSectionHubs(r, sec) })
}

func (s *Server) buildSectionHubs(r *http.Request, sec Section) homeBuild {
	batch := &shelfBatch{}
	ctx := withStoredPolicy(context.WithValue(r.Context(), shelfBatchKey{}, batch))
	defer func() { s.enqueueShelves(batch.records) }()
	pr := FromContext(ctx)
	visibleCount := countParam(r, 12)
	count := 200
	mc := hubContainer()
	mc.LibrarySectionID = model.Ptr(sec.ID)
	mc.LibrarySectionTitle = sec.Title
	mc.LibrarySectionUUID = sec.UUID
	hc := hubCtx{section: sec.ID, visible: visibleCount}
	pc := s.hubContext(pr, sec.ID)
	served := map[string]core.Hub{}
	var layoutHubs []core.Hub
	var layoutNeutral []core.NeutralHomeRow
	add := func(hs hubSpec) {
		mh := s.renderHub(ctx, pr, hs, hc)
		mc.Hub = append(mc.Hub, mh)
		hub, neutral := homeLayoutFacts(mh)
		layoutHubs = append(layoutHubs, hub)
		layoutNeutral = append(layoutNeutral, neutral)
		served[mh.HubIdentifier] = hs.Hub
	}
	add(s.continueHub(ctx, pr, cwSection, sec, visibleCount))
	if ra, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
		add(ra)
	} else {
		s.log.Warn("recently added hub", "section", sec.ID, "err", err)
	}
	var sectionPlaylist *model.Hub
	if r.URL.Query().Get("includeLibraryPlaylists") == "1" {
		if h, ok := s.sectionPlaylistsHub(ctx, pr, sec.ID, visibleCount); ok {
			sectionPlaylist = &h
		}
	}
	provisional := false
	if s.d.Recommender != nil {
		hubs, err := s.recommendSection(ctx, pr, sec.Type, pc, s.saferFetchCount(pr.User, count))
		if err != nil {
			s.log.Warn("section hubs", "section", sec.ID, "err", err)
		}
		provisional = !hasRecommendedRows(hubs)
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
	mc.Hub = stableHomeHubs(s.arrangeHomeRows(ctx, pr, pc, sectionName, mc.Hub, layoutHubs, layoutNeutral))
	if sectionPlaylist != nil {
		at := 0
		for i, h := range mc.Hub {
			if strings.Contains(h.HubIdentifier, ".recentlyadded.") {
				at = i + 1
				break
			}
		}
		mc.Hub = append(mc.Hub, model.Hub{})
		copy(mc.Hub[at+1:], mc.Hub[at:])
		mc.Hub[at] = *sectionPlaylist
	}

	s.log.Debug("section hubs rendered", "section", sec.ID, "user", pr.User.ID, "count", count, "rows", len(mc.Hub))
	mc.Size = len(mc.Hub)
	return homeBuild{mc: mc, served: served, section: sec.ID, provisional: provisional}
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
		// Assembly assigns candidates across rows based on the requested count.
		// Every page must therefore use the same bounded assembly window.
		count = 200
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
	case strings.HasPrefix(id, "movie.recentlyviewed.") || strings.HasPrefix(id, "tv.recentlyviewed."):
		if sec, ok := s.sectionFromHubID(id); ok && s.canSeeSection(pr.User, sec.ID) {
			if hs, err := s.recentlyWatchedHub(ctx, pr, sec, count); err == nil {
				return hs, true
			}
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
	if hubs, err := s.recommendHome(ctx, pr, s.hubContext(pr, 0), count); len(sections) == 0 && err == nil {
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
	if src, ok := s.d.Recommender.(core.HubItemSource); ok {
		secID := 0
		if len(sections) == 1 {
			secID = sections[0]
		}
		if h, ok := src.HubItemsFor(ctx, pr.User, s.hubContext(pr, secID), id, count); ok && s.hubInSections(h, sections) {
			return hubSpec{Hub: h}, true
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
	if err == nil && k.Kind() == core.KindPlaylist {
		if s.d.Playlists == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if _, err := s.playlist(ctx, pr.User, k); err != nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		s.write(w, r, &model.MediaContainer{})
		return
	}
	if err == nil && !s.relatedSourceVisible(ctx, pr, k) && strings.HasPrefix(r.URL.Path, "/hubs/") {
		// Only genuinely absent catalogue identities receive the legacy empty
		// response; authorization failures retain a refusal.
		if _, lookupErr := s.metadataTitle(ctx, pr, k); errors.Is(lookupErr, core.ErrNotFound) {
			s.write(w, r, hubContainer())
			return
		}
	}
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	mc := hubContainer()
	if title, lookupErr := s.metadataTitle(ctx, pr, k); lookupErr == nil {
		if sec, ok := s.sectionFor(title.Type); ok {
			mc.LibrarySectionID = model.Ptr(sec.ID)
			mc.LibrarySectionTitle = sec.Title
			mc.LibrarySectionUUID = sec.UUID
		}
	}
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
	if k.Kind() != core.KindEpisode {
		return hubSpec{}, false
	}
	ep, state := s.showOnDeck(ctx, pr, core.ShowKey(k.TMDB()), nil)
	if ep == nil || ep.Key == k || !s.visibleTitle(ctx, pr.User, ep) {
		return hubSpec{}, false
	}
	active := false
	for _, st := range state {
		active = active || st.ViewCount > 0 || st.ViewOffset > 0
	}
	if !active {
		return hubSpec{}, false
	}
	return hubSpec{Hub: core.Hub{ID: "tv.upnext", Title: upNextTitle(pr.Language), Type: core.TypeEpisode,
		Context: "hub.tv.upnext", Items: []core.Title{*ep}}, state: state}, true
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
	mc.SetSyncProjection(false)
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

// searchHubDefs is PMS's 17-hub set and order for /hubs/search and
// /hubs/search/voice.
var searchHubDefs = []struct {
	id    string
	typ   string
	title string
}{
	{"show", "show", "Shows"},
	{"artist", "artist", "Artists"},
	{"album", "album", "Albums"},
	{"movie", "movie", "Movies"},
	{"episode", "episode", "Episodes"},
	{"track", "track", "Tracks"},
	{"collection", "collection", "Collections"},
	{"photoalbum", "photoalbum", "Photo Albums"},
	{"autotag", "autotag", "Automatic Tags"},
	{"photo", "photo", "Photos"},
	{"tag", "tag", "Tags"},
	{"actor", "actor", "Actors"},
	{"director", "director", "Directors"},
	{"genre", "genre", "Genres"},
	{"playlist", "playlist", "Playlists"},
	{"shared", "shared", "Shared"},
	{"place", "place", "Places"},
}

type peopleSearchCatalog interface {
	SearchPeople(ctx context.Context, terms []string, limit int) ([]core.Person, error)
	PersonCreditKeys(ctx context.Context, person int64, types []core.MediaType, limit int) ([]core.RatingKey, error)
}

type reasonedTitle struct {
	title       core.Title
	reason      string
	reasonID    string
	reasonTitle string
}

type peopleSearchResult struct {
	actors        []model.Directory
	actorsMore    bool
	directors     []model.Directory
	directorsMore bool
	titles        map[core.MediaType][]reasonedTitle
	// actorScores and directorScores are each directory's relevance.
	actorScores    []float64
	directorScores []float64
}

// Search relevance as PMS reports it: a person whose whole name is the
// query, any other matching person, and a title found through a person.
const (
	searchPersonExactScore = 0.92
	searchPersonScore      = 0.32
	searchReasonedScore    = 0.33
)

// personSearchScore is a matching person's relevance to query.
func personSearchScore(query, name string) float64 {
	if searchFoldName(query) == searchFoldName(name) {
		return searchPersonExactScore
	}
	return searchPersonScore
}

// searchFoldName is s in lower case without accents, keeping only letters
// and digits, so names compare whatever their punctuation and spacing.
func searchFoldName(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// titleSearchScore is a result's relevance: the catalogue's score when it
// has one, otherwise one by its place among n. It orders results and hubs;
// clients are sent it capped at 1 (reportedScore).
func titleSearchScore(t *core.Title, i, n int) float64 {
	if t.SearchScore > 0 {
		return t.SearchScore
	}
	return searchScore(i, n)
}

// reportedScore is a relevance as PMS reports it, between 0 and 1.
func reportedScore(score float64) float64 { return min(1, score) }

func peopleQueryTerms(query string) []string {
	return strings.FieldsFunc(strings.ToLower(strings.TrimSpace(query)), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9' || r >= 0x80)
	})
}

func personMatchesTerms(name string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	words := peopleQueryTerms(name)
	for _, term := range terms {
		matched := false
		for _, w := range words {
			if strings.HasPrefix(w, term) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func personInList(people []core.Person, tmdb int64) (core.Person, bool) {
	for _, p := range people {
		if p.TMDB == tmdb {
			return p, true
		}
	}
	return core.Person{}, false
}

func personSearchDirectory(sec Section, person core.Person, kind string, count int, thumbKey core.RatingKey, inHub bool) model.Directory {
	idStr := strconv.FormatInt(person.TMDB, 10)
	secIDStr := strconv.Itoa(sec.ID)
	tagType := 6
	if kind == "director" {
		tagType = 4
	}
	secType := 1
	if sec.Type == core.TypeShow {
		secType = 2
	}
	attrs := []model.Attr{
		model.A("key", "/library/sections/"+secIDStr+"/all?"+kind+"="+idStr),
		model.A("librarySectionID", sec.ID),
		model.A("librarySectionKey", "/library/sections/"+secIDStr),
		model.A("librarySectionTitle", sec.Title),
		model.A("librarySectionType", secType),
	}
	if inHub {
		attrs = append(attrs,
			model.A("reason", "section"),
			model.A("reasonID", sec.ID),
			model.A("reasonTitle", sec.Title),
		)
	}
	attrs = append(attrs,
		model.A("type", "tag"),
		model.A("id", person.TMDB),
		model.A("filter", kind+"="+idStr),
		model.A("tag", person.Name),
		model.A("tagType", tagType),
		model.A("tagKey", "tmdb-person-"+idStr),
	)
	if person.ThumbPath != "" && thumbKey != 0 {
		attrs = append(attrs, model.A("thumb", "/library/metadata/"+thumbKey.String()+"/person/"+kind+"/"+base64.RawURLEncoding.EncodeToString([]byte(person.ThumbPath))))
	}
	attrs = append(attrs, model.A("count", count))
	return model.Directory{Extra: attrs}
}

// rankDirectories orders person directories best match first (stable) and
// returns their scores in the new order.
func rankDirectories(dirs []model.Directory, scores []float64) []float64 {
	idx := make([]int, len(dirs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	sortedDirs := make([]model.Directory, len(dirs))
	sortedScores := make([]float64, len(dirs))
	for i, j := range idx {
		sortedDirs[i], sortedScores[i] = dirs[j], scores[j]
	}
	copy(dirs, sortedDirs)
	return sortedScores
}

func stampDirectoryScores(dirs []model.Directory, scores []float64) {
	for i := range dirs {
		score := strconv.FormatFloat(scores[i], 'f', 5, 64)
		inserted := false
		extra := make([]model.Attr, 0, len(dirs[i].Extra)+1)
		for _, a := range dirs[i].Extra {
			extra = append(extra, a)
			if a.Name == "reasonTitle" {
				extra = append(extra, model.A("score", score))
				inserted = true
			}
		}
		if !inserted {
			extra = append(extra, model.A("score", score))
		}
		dirs[i].Extra = extra
	}
}

// searchPeople queries the indexed people table and resolves visible
// actor/director hubs and reasoned titles for the viewer.
func (s *Server) searchPeople(ctx context.Context, pr *Request, query string, sectionID, limit int, inHub bool) peopleSearchResult {
	out := peopleSearchResult{titles: map[core.MediaType][]reasonedTitle{}}
	ps, ok := s.d.Catalog.(peopleSearchCatalog)
	if !ok {
		return out
	}
	terms := peopleQueryTerms(query)
	if len(terms) == 0 {
		return out
	}
	allowedTypes := s.searchTypes(pr.User, sectionID)
	if len(allowedTypes) == 0 {
		return out
	}
	queryTypes := make([]core.MediaType, 0, len(allowedTypes)+1)
	allowedSet := map[core.MediaType]bool{}
	for _, t := range allowedTypes {
		allowedSet[t] = true
		queryTypes = append(queryTypes, t)
		if t == core.TypeShow {
			allowedSet[core.TypeEpisode] = true
			queryTypes = append(queryTypes, core.TypeEpisode)
		}
	}
	var secs []Section
	for _, sec := range s.userSections(pr.User) {
		if sectionID > 0 && sec.ID != sectionID {
			continue
		}
		secs = append(secs, sec)
	}
	if len(secs) == 0 {
		return out
	}
	candidates, err := ps.SearchPeople(ctx, terms, max(limit*4, 24))
	if err != nil || len(candidates) == 0 {
		return out
	}
	// An explicit search: rules the owner limited to browsing do not apply.
	catVis, _ := s.d.Catalog.(interface{ SearchVisible(*core.Title) bool })
	now := s.now()
	creditLimit := max(limit*3, 30)
	// A person whose whole name is the query leads, then the most credited.
	// Each person costs a credit walk, so only as many are walked as a hub
	// can show, with headroom for people with nothing visible.
	sort.SliceStable(candidates, func(i, j int) bool {
		return personSearchScore(query, candidates[i].Name) > personSearchScore(query, candidates[j].Name)
	})
	walkBudget, partialBudget := limit+limit/2, max(limit/2, 3)
	var actorTitles, directorTitles []reasonedTitle
	for _, cand := range candidates {
		if !personMatchesTerms(cand.Name, terms) {
			continue
		}
		exactName := personSearchScore(query, cand.Name) == searchPersonExactScore
		if !exactName {
			// Partial name matches ("Her" in Barbara Hershey) rank last and
			// are only a few of the people a hub shows.
			if partialBudget == 0 {
				walkBudget = 0
			} else {
				partialBudget--
			}
		}
		if walkBudget == 0 {
			out.actorsMore = out.actorsMore || len(out.actors) >= limit
			out.directorsMore = out.directorsMore || len(out.directors) >= limit
			break
		}
		walkBudget--
		keys, err := ps.PersonCreditKeys(ctx, cand.TMDB, queryTypes, creditLimit)
		if err != nil || len(keys) == 0 {
			continue
		}
		titles := s.getTitles(withStoredPolicy(ctx), keys)
		type secHit struct {
			count     int
			thumbPath string
			thumbKey  core.RatingKey
		}
		actorSecs := map[int]*secHit{}
		directorSecs := map[int]*secHit{}
		idStr := strconv.FormatInt(cand.TMDB, 10)
		for i := range titles {
			t := titles[i]
			if !allowedSet[t.Type] {
				continue
			}
			if catVis != nil && !catVis.SearchVisible(&t) {
				continue
			}
			if t.Adult || !t.IsReleased(now) {
				continue
			}
			if !s.visibleTitle(ctx, pr.User, &t) {
				continue
			}
			sec, hasSec := s.sectionFor(t.Type)
			if p, ok := personInList(t.Cast, cand.TMDB); ok {
				if hasSec {
					h := actorSecs[sec.ID]
					if h == nil {
						h = &secHit{}
						actorSecs[sec.ID] = h
					}
					h.count++
					if h.thumbPath == "" && p.ThumbPath != "" {
						h.thumbPath = p.ThumbPath
						h.thumbKey = t.Key
					}
				}
				actorTitles = append(actorTitles, reasonedTitle{
					title:       t,
					reason:      "actor",
					reasonID:    idStr,
					reasonTitle: cand.Name,
				})
			}
			if p, ok := personInList(t.Directors, cand.TMDB); ok {
				if hasSec {
					h := directorSecs[sec.ID]
					if h == nil {
						h = &secHit{}
						directorSecs[sec.ID] = h
					}
					h.count++
					if h.thumbPath == "" && p.ThumbPath != "" {
						h.thumbPath = p.ThumbPath
						h.thumbKey = t.Key
					}
				}
				directorTitles = append(directorTitles, reasonedTitle{
					title:       t,
					reason:      "director",
					reasonID:    idStr,
					reasonTitle: cand.Name,
				})
			}
		}
		for _, sec := range secs {
			if h := actorSecs[sec.ID]; h != nil && h.count > 0 {
				if len(out.actors) < limit {
					person := core.Person{TMDB: cand.TMDB, Name: cand.Name, ThumbPath: h.thumbPath}
					out.actors = append(out.actors, personSearchDirectory(sec, person, "actor", h.count, h.thumbKey, inHub))
					out.actorScores = append(out.actorScores, personSearchScore(query, cand.Name))
				} else {
					out.actorsMore = true
				}
			}
			if h := directorSecs[sec.ID]; h != nil && h.count > 0 {
				if len(out.directors) < limit {
					person := core.Person{TMDB: cand.TMDB, Name: cand.Name, ThumbPath: h.thumbPath}
					out.directors = append(out.directors, personSearchDirectory(sec, person, "director", h.count, h.thumbKey, inHub))
					out.directorScores = append(out.directorScores, personSearchScore(query, cand.Name))
				} else {
					out.directorsMore = true
				}
			}
		}
	}
	seenByType := map[core.MediaType]map[core.RatingKey]bool{}
	addReasoned := func(rt reasonedTitle) {
		seen := seenByType[rt.title.Type]
		if seen == nil {
			seen = map[core.RatingKey]bool{}
			seenByType[rt.title.Type] = seen
		}
		if seen[rt.title.Key] {
			return
		}
		seen[rt.title.Key] = true
		out.titles[rt.title.Type] = append(out.titles[rt.title.Type], rt)
	}
	for _, rt := range actorTitles {
		addReasoned(rt)
	}
	for _, rt := range directorTitles {
		addReasoned(rt)
	}
	return out
}

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

func (s *Server) searchExperience() experience.Search {
	unrestricted := func(u *core.User) bool { return !s.viewerRestricted(u) }
	src := s.d.Search
	if src.Catalog == nil {
		// A server built outside the app (tests, spike mode) searches its
		// own dependencies the same way.
		searcher := s.d.VibeSearch
		if searcher == nil {
			searcher, _ = s.d.Collections.(core.VibeSearcher)
		}
		src = experience.SearchSources{Catalog: s.d.Catalog.Search, Natural: experience.DiscoverySearch(s.d.Discovery), Familiar: experience.FamiliarFrom(s.d.State)}
		if searcher != nil {
			src.Vibe = searcher
		}
	}
	return src.For(s.visibility(), unrestricted)
}

func (s *Server) runSearch(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]core.Title, error) {
	result, err := s.searchExperience().Resolve(ctx, pr.User, query, s.searchTypes(pr.User, sectionID), s.saferFetchCount(pr.User, limit), false)
	result.Items = s.takeVisible(pr.User, result.Items, limit)
	return result.Items, err
}

func (s *Server) collectionDirectory(ctx context.Context, c *core.Collection) model.Directory {
	m := s.collectionMetadata(c, ctx)
	return model.Directory{
		Key: "/library/collections/" + c.Key.String() + "/children", Type: "collection",
		Title: c.Title, Thumb: m.Thumb, Art: m.Art,
		LeafCount: model.Ptr(c.ChildCount),
		Extra:     []model.Attr{model.A("ratingKey", c.Key.String()), model.A("subtype", string(c.Type)), model.A("childCount", c.ChildCount), model.A("librarySectionID", m.LibrarySectionID)},
	}
}

// searchLimit follows the search endpoints' two distinct refusal classes.
func searchLimit(w http.ResponseWriter, r *http.Request, fallback, ceiling int) (int, bool) {
	raw, present := firstQuery(r.URL.Query(), "limit")
	if !present {
		return fallback, true
	}
	n, valid := parseWhole32(raw)
	if !valid {
		writeHTTPError(w, http.StatusBadRequest)
		return 0, false
	}
	if n <= 0 {
		writeHTTPError(w, http.StatusInternalServerError)
		return 0, false
	}
	return min(int(n), ceiling), true
}

func (s *Server) handleHubSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	query := q.Get("query")
	if strings.TrimSpace(query) == "" {
		// PMS rejects an empty hub search with its HTML 400 page.
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	limit, valid := searchLimit(w, r, 10, 100)
	if !valid {
		return
	}
	sectionID, _ := strconv.Atoi(q.Get("sectionId"))
	fetch := max(limit*2, s.saferFetchCount(pr.User, limit))
	// People are searched beside titles; neither waits for the other.
	peopleDone := make(chan peopleSearchResult, 1)
	go func() { peopleDone <- s.searchPeople(ctx, pr, query, sectionID, limit, true) }()
	result, err := s.searchExperience().Resolve(ctx, pr.User, query, s.searchTypes(pr.User, sectionID), fetch, true)
	people := <-peopleDone
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	vibe, res, isVibe := result.Collection, result.Items, result.CollectionSearch
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
	// PMS returns every result hub, empty ones included: those with results
	// first, best match first, then the rest in searchHubDefs order.
	mc := &model.MediaContainer{}
	var tops []float64 // each hub's best score
	for _, hd := range searchHubDefs {
		top := 0.0
		switch hd.id {
		case string(core.TypeMovie), string(core.TypeShow), string(core.TypeEpisode):
			mediaType := core.MediaType(hd.typ)
			var items []core.Title
			reasons := map[string]reasonedTitle{}
			if allowed[mediaType] {
				seen := map[core.RatingKey]bool{}
				for _, t := range byType[mediaType] {
					seen[t.Key] = true
					items = append(items, t)
				}
				for _, rt := range people.titles[mediaType] {
					if seen[rt.title.Key] {
						continue
					}
					seen[rt.title.Key] = true
					items = append(items, rt.title)
					reasons[rt.title.Key.String()] = rt
				}
			}
			more := false
			if len(items) > limit {
				items, more = items[:limit], true
			}
			h := core.Hub{ID: hd.id, Title: hd.title, Type: mediaType, Items: items, More: more, Style: "shelf"}
			scores := make(map[string]float64, len(items))
			relevant := make(map[string]bool, len(items))
			for i := range items {
				scores[items[i].Key.String()] = titleSearchScore(&items[i], i, len(items))
				relevant[items[i].Key.String()] = items[i].SearchScore > 0
			}
			mh := s.renderHub(ctx, pr, hubSpec{Hub: h, noPromoted: true}, hubCtx{})
			mh.Context, mh.Key, mh.HubKey = "", "", ""
			for i := range mh.Metadata {
				score, ok := scores[mh.Metadata[i].RatingKey]
				if !ok {
					score = searchScore(i, len(mh.Metadata))
				}
				if rt, ok := reasons[mh.Metadata[i].RatingKey]; ok {
					mh.Metadata[i].Reason = rt.reason
					mh.Metadata[i].ReasonID = rt.reasonID
					mh.Metadata[i].ReasonTitle = rt.reasonTitle
					score = searchReasonedScore
				}
				mh.Metadata[i].Score = strconv.FormatFloat(reportedScore(score), 'f', 5, 64)
				if relevant[mh.Metadata[i].RatingKey] {
					top = max(top, score)
				}
			}
			mc.Hub = append(mc.Hub, mh)
		case "collection":
			mh := model.Hub{
				Title:         hd.title,
				Type:          hd.typ,
				HubIdentifier: hd.id,
				Style:         "shelf",
			}
			if isVibe && vibe != nil {
				mh.Directory = []model.Directory{s.collectionDirectory(ctx, vibe)}
				mh.Size = len(mh.Directory)
				top = searchPersonExactScore
			}
			mc.Hub = append(mc.Hub, mh)
		case "playlist":
			mh := model.Hub{Title: hd.title, Type: hd.typ, HubIdentifier: hd.id, Style: "shelf"}
			mh.Metadata = s.searchPlaylists(ctx, pr, query, q.Get("contentDirectoryID"), limit)
			mh.Size = len(mh.Metadata)
			if mh.Size > 0 {
				top, _ = strconv.ParseFloat(mh.Metadata[0].Score, 64)
			}
			mc.Hub = append(mc.Hub, mh)
		case "actor":
			dirs := people.actors
			scores := rankDirectories(dirs, people.actorScores)
			stampDirectoryScores(dirs, scores)
			if len(scores) > 0 {
				top = scores[0]
			}
			mc.Hub = append(mc.Hub, model.Hub{
				Title:         hd.title,
				Type:          hd.typ,
				HubIdentifier: hd.id,
				Size:          len(dirs),
				More:          people.actorsMore,
				Style:         "shelf",
				Directory:     dirs,
			})
		case "director":
			dirs := people.directors
			scores := rankDirectories(dirs, people.directorScores)
			stampDirectoryScores(dirs, scores)
			if len(scores) > 0 {
				top = scores[0]
			}
			mc.Hub = append(mc.Hub, model.Hub{
				Title:         hd.title,
				Type:          hd.typ,
				HubIdentifier: hd.id,
				Size:          len(dirs),
				More:          people.directorsMore,
				Style:         "shelf",
				Directory:     dirs,
			})
		default:
			mc.Hub = append(mc.Hub, model.Hub{
				Title:         hd.title,
				Type:          hd.typ,
				HubIdentifier: hd.id,
				Style:         "shelf",
			})
		}
		tops = append(tops, top)
	}
	orderSearchHubs(mc.Hub, tops)
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// searchStrongMatch is the relevance from which a hub's best result moves
// the hub ahead of PMS's fixed hub order, as PMS does: the title the viewer
// typed, a title it begins, a person by full name. Hubs holding only weak
// matches (PMS's 0.33) keep their place.
const searchStrongMatch = 0.5

// orderSearchHubs moves hubs whose best result is a strong match (tops)
// to the front, best first, and leaves the rest in their given order.
func orderSearchHubs(hubs []model.Hub, tops []float64) {
	idx := make([]int, len(hubs))
	for i := range idx {
		idx[i] = i
	}
	key := func(i int) float64 {
		if tops[i] < searchStrongMatch {
			return 0
		}
		return tops[i]
	}
	sort.SliceStable(idx, func(a, b int) bool { return key(idx[a]) > key(idx[b]) })
	ordered := make([]model.Hub, len(hubs))
	for i, j := range idx {
		ordered[i] = hubs[j]
	}
	copy(hubs, ordered)
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
	query := q.Get("query")
	limit, valid := searchLimit(w, r, 50, 200)
	if !valid {
		return
	}
	// People are searched beside titles when the request wants them.
	wantPeople := q.Get("searchTypes") == ""
	for _, v := range strings.Split(q.Get("searchTypes"), ",") {
		switch strings.TrimSpace(v) {
		case "people", "person", "actors", "actor", "directors", "director":
			wantPeople = true
		}
	}
	var peopleDone chan peopleSearchResult
	if wantPeople {
		peopleDone = make(chan peopleSearchResult, 1)
		go func() { peopleDone <- s.searchPeople(ctx, pr, query, 0, limit, false) }()
	}
	result, err := s.searchExperience().Resolve(ctx, pr.User, query, s.searchTypes(pr.User, 0), limit, true)
	var people peopleSearchResult
	if peopleDone != nil {
		people = <-peopleDone
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	vibe, res, isVibe := result.Collection, result.Items, result.CollectionSearch
	searchCollections := false
	searchPeopleFlag := q.Get("searchTypes") == ""
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
			case "people", "person", "actors", "actor", "directors", "director":
				searchPeopleFlag = true
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
	var peopleDirs []model.Directory
	var peopleScores []float64
	if searchPeopleFlag {
		peopleDirs = append(peopleDirs, people.actors...)
		peopleDirs = append(peopleDirs, people.directors...)
		peopleScores = append(append(peopleScores, people.actorScores...), people.directorScores...)
	}
	scores := make(map[string]float64, len(res))
	for i := range res {
		scores[res[i].Key.String()] = titleSearchScore(&res[i], i, len(res)+len(peopleDirs))
	}
	md := s.renderTitles(ctx, pr, res, renderOpts{media: true})
	mc := &model.MediaContainer{}
	totalRanked := len(md) + len(peopleDirs)
	raw := make([]float64, 0, totalRanked)
	for i := range md {
		score, ok := scores[md[i].RatingKey]
		if !ok {
			score = searchScore(i, totalRanked)
		}
		// PMS names the section here but sends no section id or key.
		md[i].LibrarySectionID, md[i].LibrarySectionKey = 0, ""
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: math.Round(reportedScore(score)*10000) / 10000, Metadata: &md[i]})
		raw = append(raw, score)
	}
	for i := range peopleDirs {
		d := peopleDirs[i]
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: math.Round(peopleScores[i]*10000) / 10000, Directory: &d})
		raw = append(raw, peopleScores[i])
	}
	if q.Get("searchTypes") == "" || containsExactSearchToken(q.Get("searchTypes"), "movies") || containsExactSearchToken(q.Get("searchTypes"), "tv") {
		directories := q.Get("contentDirectoryID")
		if directories == "" && q.Get("searchTypes") != "" {
			var ids []string
			for _, sec := range s.userSections(pr.User) {
				if sec.Type == core.TypeMovie && containsExactSearchToken(q.Get("searchTypes"), "movies") || sec.Type == core.TypeShow && containsExactSearchToken(q.Get("searchTypes"), "tv") {
					ids = append(ids, strconv.Itoa(sec.ID))
				}
			}
			directories = strings.Join(ids, ",")
		}
		for _, m := range s.searchPlaylists(ctx, pr, query, directories, limit) {
			md := m
			score, _ := strconv.ParseFloat(md.Score, 64)
			md.Score = ""
			mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: score, Metadata: &md})
			raw = append(raw, score)
		}
	}
	// PMS lists every result best match first.
	order := make([]int, len(raw))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return raw[order[a]] > raw[order[b]] })
	ranked := make([]model.SearchResult, len(order))
	for i, j := range order {
		ranked[i] = mc.SearchResult[j]
	}
	mc.SearchResult = ranked
	if isVibe && vibe != nil && (searchCollections || q.Get("searchTypes") == "") {
		d := s.collectionDirectory(ctx, vibe)
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 1, Directory: &d})
	}
	// A flat search has one window across every result family.
	if len(mc.SearchResult) > limit {
		mc.SearchResult = mc.SearchResult[:limit]
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}
