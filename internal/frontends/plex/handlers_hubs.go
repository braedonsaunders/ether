package plex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"golang.org/x/text/unicode/norm"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// hubCtx says where a hub list came from, so each hub's "key" can fetch
// the full row again.
type hubCtx struct {
	section int
	related frontend.RatingKey
	// visible caps the rendered items of a hub whose candidate window is
	// wider (section hubs assemble a stable window for later pages).
	visible   int
	pageItems bool
	// renderSec stamps librarySection* for a scoped hub even when section == 0
	// (e.g. Home per-directory rows that skip SQL shelfSnapshot pinning).
	renderSec *Section
	// Search hubs are rendered on demand, with a short GUID verification budget.
	searchIdentity bool
}

// hubSpec is a hub plus the PMS presentation details Ether generates for
// its own rows (key, promoted flag, watch state).
type hubSpec struct {
	frontend.Hub
	key        string // overrides the computed key
	noPromoted bool   // omit the promoted attribute (section rows)
	// included stamps includedAt on the items, as PMS's
	// /hubs/continueWatching does.
	included bool
	// plain keeps episodes in list form (the Home hero row); other rows
	// carry episodes in full, as PMS does.
	plain bool
	state map[frontend.RatingKey]frontend.WatchStatus
}

// Retain the bounded candidate row that was actually advertised. Clients can
// open a remembered identifier after the recommendation model changes, and
// some omit metadataID from related-row continuations.
type emittedHub struct {
	spec    hubSpec
	related frontend.RatingKey
	expires time.Time
	bytes   int
}

func (s *Server) emittedHubKey(ctx context.Context, pr *Request, id string, section int) string {
	scope, _, ok := s.shelfScope(ctx, pr)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s|%d|%s", scope, section, id)
}

func (s *Server) rememberHub(ctx context.Context, pr *Request, hs hubSpec, hc hubCtx) {
	if hc.section == 0 && hc.related == 0 {
		return
	}
	key := s.emittedHubKey(ctx, pr, hs.ID, hc.section)
	if key == "" {
		return
	}
	hs.Items = append([]frontend.Title(nil), hs.Items[:min(len(hs.Items), 200)]...)
	data, _ := json.Marshal(hs.Items)
	if len(data) > 4<<20 {
		return
	}
	s.emittedHubMu.Lock()
	defer s.emittedHubMu.Unlock()
	previous := s.emittedHubs[key]
	if s.emittedHubs == nil || (len(s.emittedHubs) >= 512 && previous.bytes == 0) || s.emittedHubBytes-previous.bytes+len(data) > 4<<20 {
		s.emittedHubs = map[string]emittedHub{}
		s.emittedHubBytes = 0
		previous.bytes = 0
	}
	s.emittedHubs[key] = emittedHub{hs, hc.related, s.now().Add(shelfRetention), len(data)}
	s.emittedHubBytes += len(data) - previous.bytes
}

func hubItemsKey(h frontend.Hub, hc hubCtx) string {
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
	hc, vis := s.hubVisibility(hs, hc)
	return s.renderHubWithVisibility(ctx, pr, hs, hc, vis)
}

func (s *Server) hubVisibility(hs hubSpec, hc hubCtx) (hubCtx, frontend.Visibility) {
	vis := s.visibility()
	if hc.section != 0 && hs.ID != "home.continue" && hs.ID != "home.ondeck" && hs.ID != "continueWatching" {
		if sec, ok := s.sectionByID(hc.section); ok {
			vis = s.sectionVisibility(sec)
			if hc.renderSec == nil {
				hc.renderSec = &sec
			}
		}
	}
	return hc, vis
}

func (s *Server) renderHubWithVisibility(ctx context.Context, pr *Request, hs hubSpec, hc hubCtx, vis frontend.Visibility) model.Hub {
	return s.prepareHub(ctx, pr, hs, hc, vis).paint(ctx)
}

// hubRender holds the selected page and its wire projection. Keeping these
// separate lets a screen load the facts for its pages before painting them.
type hubRender struct {
	titles []frontend.Title
	total  int
	paint  func(context.Context) model.Hub
}

func (s *Server) prepareHub(ctx context.Context, pr *Request, hs hubSpec, hc hubCtx, vis frontend.Visibility) hubRender {
	h := hs.Hub
	if len(h.Collections) > 0 {
		return hubRender{paint: func(ctx context.Context) model.Hub { return s.renderCollectionCardHub(ctx, pr, h) }}
	}
	s.rememberHub(ctx, pr, hs, hc)
	renderSec := hc.renderSec
	if h.ID == "home.continue" || h.ID == "home.ondeck" || h.ID == "continueWatching" {
		renderSec = nil
	} else if renderSec == nil && hc.section != 0 {
		if sec, ok := s.sectionByID(hc.section); ok {
			renderSec = &sec
		}
	}
	if strings.HasPrefix(h.ID, "movie.recentlyviewed.") || strings.HasPrefix(h.ID, "tv.recentlyviewed.") {
		if sec, ok := s.sectionFromHubID(h.ID); ok {
			count := max(len(h.Items), 1)
			if hc.visible > 0 {
				count = min(count, hc.visible)
			}
			if measured, err := s.recentlyWatchedHub(ctx, pr, sec, count); err == nil {
				hs = measured
				h = hs.Hub
			}
		}
	}
	kindSec := renderSec != nil && isPluginKindSection(*renderSec)
	build := func() []frontend.Title {
		if kindSec {
			return h.Items
		}
		if loader, ok := s.catalog().(frontend.RowFactsLoader); ok {
			h.Items = loader.LoadRowFacts(ctx, h.Items)
		}
		var unavailable func(*frontend.Title) bool
		if sel := s.exp().NewRowSelection(frontend.RowSelectionOptions{Visibility: vis, PrioritizeAvailable: s.prioritizeAvailable(),
			SameDayUnavailable: func(_ context.Context, t *frontend.Title) bool {
				if unavailable == nil {
					unavailable = s.sameDayUnavailableFunc(ctx, pr, h.Items)
				}
				return unavailable(t)
			},
		}); sel != nil {
			return sel.Items(ctx, pr.User, h)
		}
		return h.Items
	}
	// Filter the section and availability before rendering or paging. The
	// resulting wire list is the canonical generation (including its counts).
	if hc.section != 0 && hs.state == nil && hs.key == "" {
		if sec, ok := s.sectionByID(hc.section); ok {
			if view, ok := frontend.SectionView(h, sec.Type); ok {
				h = view
			} else {
				h.Items = nil
			}
		}
	}
	related := hc.related != 0
	pinned := (hc.section != 0 || h.Key != 0) && hs.state == nil && hs.key == "" && !related
	isWatchlist := frontend.IsWatchlistHub(h.ID)
	var page []frontend.Title
	opts := renderOpts{media: true, state: hs.state, inSection: related, guids: related, detailEpisodes: !hs.plain, vis: &vis, section: renderSec, searchIdentity: hc.searchIdentity}
	generation := ""
	var canonicalTotal *int
	if pinned {
		h.More = false
		var selected []frontend.Title
		if h.Key != 0 && !h.Key.IsEphemeral() && s.collections() != nil {
			if c, err := s.collectionRaw(ctx, pr.User, h.Key); err == nil && !c.Upcoming {
				selected, generation, _ = s.collectionSnapshot(ctx, pr, c, h.Key, h.Type)
				if generation != "" {
					canonicalTotal = model.Ptr(len(selected))
				}
			}
		}
		if generation == "" {
			selected, generation = s.shelfSnapshot(ctx, pr, h, hc, func() ([]frontend.Title, []model.Metadata) {
				// Snapshot membership before decorating the visible page.
				sourceItems := build()
				memberCtx := withStoredPolicy(ctx)
				if isWatchlist {
					memberCtx = withShelfMembers(memberCtx, sourceItems)
				}
				members := s.openableTitlesWith(memberCtx, vis, pr.User, sourceItems, shelfContainsAll(memberCtx, sourceItems))
				identities := make([]model.Metadata, 0, len(members))
				for _, title := range members {
					identities = append(identities, model.Metadata{RatingKey: title.Key.String()})
				}
				return sourceItems, identities
			})
		}
		selected = s.openableTitlesWith(withShelfMembers(withStoredPolicy(ctx), selected), vis, pr.User, selected, true)
		if canonicalTotal != nil {
			canonicalTotal = model.Ptr(len(selected))
		}
		// Membership was already evaluated in the generation; refresh presentation
		// (watch badges, artwork enrichment) without reapplying live availability.
		if hc.visible > 0 && len(selected) > hc.visible {
			selected = selected[:hc.visible]
			h.More = true
		}
		page = selected
		opts = renderOpts{media: true, availabilityInSQL: true, vis: &vis, section: renderSec}
	} else {
		page = build()
		if hc.visible > 0 && len(page) > hc.visible {
			page = page[:hc.visible]
			h.More = true
		}
	}
	total := 0
	if hc.pageItems {
		if pinned || isWatchlist {
			ctx = withShelfMembers(ctx, page)
		}
		page = s.hubPageTitles(ctx, pr, page, &opts)
		total = len(page)
		start, end := pr.window(total)
		page = page[start:end]
	}
	return hubRender{titles: page, total: total, paint: func(ctx context.Context) model.Hub {
		if pinned || isWatchlist {
			ctx = withShelfMembers(ctx, page)
		}
		md := s.renderTitles(ctx, pr, page, opts)
		if kindSec && !pinned && len(md) > 0 {
			byKey := make(map[string]*frontend.Title, len(page))
			for i := range page {
				byKey[page[i].Key.String()] = &page[i]
			}
			for i := range md {
				md[i].Type = kindTypeForBase(string(renderSec.Type), md[i].Type, byKey[md[i].RatingKey])
			}
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
			Key:            s.shelfURL(key, generation),
			Title:          h.Title,
			Type:           typ,
			HubIdentifier:  h.ID,
			Context:        ctxName,
			Size:           len(md),
			More:           h.More,
			Style:          style,
			PreviewOnFocus: h.PreviewOnFocus, RankedTop10: h.RankedTop10,
			Metadata:  md,
			TotalSize: canonicalTotal,
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
	}}
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

type hubTitlePageKey struct{}

type hubTitlePage struct {
	guids       map[frontend.RatingKey]string
	plexMeta    map[frontend.RatingKey]frontend.CachedMetadataOverlay
	hasPlexMeta bool
	state       map[frontend.RatingKey]frontend.WatchStatus
	slots       *slotMediaPage
	resumes     map[frontend.RatingKey]bool
	overlays    bool
}

// paintHubPages batches only the selected pages. Shelf membership and
// continuation receipts have already been cut using their own row policy.
func (s *Server) paintHubPages(ctx context.Context, pr *Request, pages []hubRender) []model.Hub {
	if !reqmemo.Active(ctx) {
		ctx = reqmemo.With(ctx)
	}
	var all []frontend.Title
	for _, page := range pages {
		all = append(all, page.titles...)
	}
	batch := hubTitlePage{slots: &slotMediaPage{}}
	if len(all) > 0 {
		ctx = s.withSeriesRows(ctx, pr.User, all)
		batch.state = s.stateFor(ctx, pr, all)
		if pr.User != nil {
			if pre, ok := s.playback().(resumePagePreloader); ok {
				var playable []frontend.RatingKey
				for _, title := range all {
					if isPlayable(title.Type) {
						playable = append(playable, title.Key)
					}
				}
				var probed []frontend.RatingKey
				ctx, probed = pre.PreloadResumes(ctx, pr.User, playable)
				batch.resumes = make(map[frontend.RatingKey]bool, len(probed))
				for _, k := range probed {
					batch.resumes[k] = true
				}
			}
		}
		if guid := s.libraryGUIDs(); guid != nil {
			batch.guids = guid.CachedMany(ctx, renderTitleKeys(all))
		}
		if s.plexMetaOn() {
			batch.plexMeta = s.libraryMetadata().CachedMany(ctx, renderTitleKeys(all))
			batch.hasPlexMeta = true
		}
		if d := s.discovery(); d != nil {
			if d.Ratings != nil {
				ctx = d.Ratings.WithPage(ctx, all)
			}
			if d.Overlays != nil {
				if next, err := d.Overlays.WithTitlePage(ctx, pr.User, all); err == nil {
					ctx = next
					batch.overlays = true
				}
			}
		}
	}
	ctx = context.WithValue(ctx, hubTitlePageKey{}, batch)
	out := make([]model.Hub, 0, len(pages))
	for _, page := range pages {
		out = append(out, page.paint(ctx))
	}
	s.finishSlotPage(ctx, pr, batch.slots)
	return out
}

func (s *Server) renderHubs(ctx context.Context, pr *Request, hubs []frontend.Hub, hc hubCtx) []model.Hub {
	if !reqmemo.Active(ctx) {
		ctx = reqmemo.With(ctx)
	}
	if pre, ok := s.catalog().(frontend.StoredTitlePreloader); ok && len(hubs) > 1 {
		var keys []frontend.RatingKey
		for _, h := range hubs {
			for _, t := range h.Items {
				if t.Key != 0 && !t.Key.IsPluginKey() {
					keys = append(keys, t.Key)
				}
			}
		}
		if len(keys) > 1 {
			pre.PreloadStored(ctx, keys)
		}
	}
	pages := make([]hubRender, 0, len(hubs))
	for _, h := range hubs {
		spec := hubSpec{Hub: h}
		rowCtx, vis := s.hubVisibility(spec, hc)
		pages = append(pages, s.prepareHub(ctx, pr, spec, rowCtx, vis))
	}
	return s.paintHubPages(ctx, pr, pages)
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
	// Continue Watching is PMS's OnDeck, bounded by its OnDeckLimit: a
	// row or page asking for more answers the whole bounded deck.
	bounded := count >= plexOnDeckLimit
	count = min(count, plexOnDeckLimit)
	t := frontend.MediaType("")
	if sec.ID != 0 {
		t = sec.Type
	}
	// Home renders resume and On Deck from the same selection. Share that
	// request-local work before either row filters its own membership.
	type selection struct {
		items []frontend.Title
		state map[frontend.RatingKey]frontend.WatchStatus
		more  bool
	}
	uid := int64(0)
	if pr.User != nil {
		uid = pr.User.ID
	}
	unfiltered := v == cwSection && sec.Type == frontend.TypeShow
	fetch := s.saferFetchCount(pr.User, count)
	var scope Section
	if v == cwSection && isCustomTVSection(sec) {
		scope = sec
		// A newer episode from another library must not fill this shelf's page.
		fetch = s.saferFetchCount(pr.User, plexOnDeckLimit)
	}
	picked, _ := reqmemo.Do(ctx, fmt.Sprintf("plex:continue:%p:%d:%s:%d:%t:%d", s, uid, t, fetch, unfiltered, scope.ID), func() (selection, error) {
		selector := s.continueExperienceForSection(pr, scope)
		if unfiltered {
			selector = selector.WithoutFilter()
		}
		items, state, more := selector.Shelf(ctx, pr.User, t, fetch)
		return selection{items, state, more}, nil
	})
	items, state, more := append([]frontend.Title(nil), picked.items...), maps.Clone(picked.state), picked.more
	if scope.ID != 0 {
		// If membership cannot be checked, suppress the shelf.
		items, _ = s.scopedSectionMembers(frontend.WithCatalogViewer(ctx, pr.User), scope, items)
	}
	if windowed := s.onDeckWindowed(items, state); len(windowed) < len(items) {
		// Newest first: the window cut the tail, and the overflow
		// candidate behind it is older still.
		items, more = windowed, false
	}
	if v == cwSection && sec.Type == frontend.TypeShow && pr.User != nil {
		if gate, ok := s.state().(frontend.CompletedShowsState); ok {
			shows := make([]frontend.RatingKey, 0, len(items))
			for _, item := range items {
				if state[item.Key].ViewOffset <= 0 {
					shows = append(shows, titleShow(&item))
				}
			}
			completed, err := gate.PlaybackCompletedShows(ctx, pr.User.ID, shows)
			if err != nil {
				items = nil
			} else {
				out := items[:0]
				for _, item := range items {
					if state[item.Key].ViewOffset > 0 || completed[titleShow(&item)] {
						out = append(out, item)
					}
				}
				items = out
			}
		}
	}
	vis := s.visibility()
	if sec.ID != 0 {
		vis = s.sectionVisibility(sec)
	}
	items = s.visibleTitlesWith(vis, pr.User, items)
	if scope.ID != 0 {
		more = len(items) > count
	}
	if count > 0 && len(items) > count {
		items = items[:count]
	}
	hs := hubSpec{Hub: frontend.Hub{Title: "Continue Watching", Style: "hero", Promoted: true}, state: state}
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
		if sec.Type == frontend.TypeShow {
			hs.ID, hs.Context, hs.Type = "tv.inprogress."+id, "hub.tv.inprogress", frontend.TypeEpisode
		} else {
			hs.ID, hs.Context, hs.Type = "movie.inprogress."+id, "hub.movie.inprogress", frontend.TypeMovie
		}
	}
	hs.More = more && !bounded
	hs.Items = items
	return hs
}

func (s *Server) recentlyAddedHub(ctx context.Context, pr *Request, sec Section, count int) (hubSpec, error) {
	var (
		items []frontend.Title
		more  bool
		err   error
	)
	if isKindSection(sec) && s.exp() != nil {
		page, lerr := s.exp().LibraryItems(ctx, pr.User, plexDevice(pr), sec.IDString(), frontend.ItemQuery{
			Filter:  frontend.Filter{KnownAddedDate: true},
			Sort:    frontend.SortAdded,
			Desc:    true,
			Limit:   count + 1,
			NoTotal: true,
		})
		if lerr != nil {
			return hubSpec{}, lerr
		}
		items = page.Items
		if len(items) > count {
			more = true
			items = items[:count]
		}
	} else {
		recent := s.exp().NewRecent(frontend.RecentOptions{Visibility: s.sectionVisibility(sec), Unrestricted: func(u *frontend.User) bool { return !s.viewerRestricted(u) }})
		items, more, err = recent.Added(ctx, pr.User, sec.Type, count, func(ctx context.Context, q frontend.BrowseQuery) (frontend.Page, error) {
			q.Filter, _ = s.sectionBrowseScope(ctx, sec, pr.User, q.Filter)
			return s.catalogBrowse(ctx, q)
		})
		if err != nil {
			return hubSpec{}, err
		}
	}
	id := strconv.Itoa(sec.ID)
	hs := hubSpec{Hub: frontend.Hub{Title: "Recently Added in " + sec.Title, Type: sec.Type, Style: "shelf", Promoted: true, Items: items}}
	if sec.Type == frontend.TypeShow {
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

// hubInSections reports whether a hub's items belong to one of the sections.
func (s *Server) hubInSections(h frontend.Hub, ids []int) bool {
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		sec, ok := s.sectionByID(id)
		if !ok {
			continue
		}
		if h.Type == "" {
			if len(h.Items) == 0 {
				return true
			}
			if _, ok := frontend.SectionView(h, sec.Type); ok {
				return true
			}
			continue
		}
		if titleInSectionType(h.Type, sec.Type) {
			return true
		}
	}
	return false
}

func (s *Server) homeHubs(w http.ResponseWriter, r *http.Request, promotedOnly bool) {
	r = r.WithContext(s.exp().NoProviderWait(r.Context()))
	route := "hubs"
	if promotedOnly {
		route = "hubs/promoted"
	}
	s.serveHome(w, r, route, func(_ context.Context, r *http.Request) homeBuild { return s.buildHomeHubs(r, promotedOnly) })
}

func (s *Server) buildHomeHubs(r *http.Request, promotedOnly bool) homeBuild {
	if strings.TrimSpace(r.URL.Query().Get("contentDirectoryID")) != "" {
		return s.buildScopedHomeHubs(r, promotedOnly)
	}
	ctx := withStoredPolicy(r.Context())
	pr := FromContext(ctx)
	q := r.URL.Query()
	count := countParam(r, 12)
	hc := s.hubContext(pr, 0)
	mc := hubContainer()
	var hubs []frontend.Hub
	if s.recommender() != nil {
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
	provisional := s.recommender() != nil && !hasRecommendedRows(hubs)
	if source, ok := s.recommender().(collectionCardHubSource); ok {
		hubs = append(hubs, source.CollectionCardHubs(ctx, pr.User, hc, count)...)
	}
	includeContinue := !metadataOptionOn(q.Get("excludeContinueWatching"))
	progress := s.homeProgressSpecs(ctx, pr, selected, includeContinue, count)
	var discovery []frontend.Hub
	for _, spec := range s.discoveryHubs(ctx, pr, count) {
		discovery = append(discovery, spec.Hub)
	}

	pages := make([]hubRender, 0, len(progress)+len(hubs)+len(discovery))
	ids := make([]string, 0, len(hubs)+len(discovery))
	vis := s.visibility()
	for _, spec := range progress {
		pages = append(pages, s.prepareHub(ctx, pr, spec, hubCtx{}, vis))
	}
	add := func(hub frontend.Hub) {
		pages = append(pages, s.prepareHub(ctx, pr, hubSpec{Hub: hub}, hubCtx{}, vis))
		ids = append(ids, hub.ID)
	}
	for _, hub := range hubs {
		if hub.ID != "home.ondeck" && frontend.HomePurpose(hub) != "continue" && (!promotedOnly || hub.Promoted) {
			add(hub)
		}
	}
	for _, hub := range discovery {
		add(hub)
	}
	paintedRows := s.paintHubPages(ctx, pr, pages)
	painted := make(map[string][]model.Hub, len(ids))
	for i, id := range ids {
		painted[id] = append(painted[id], paintedRows[len(progress)+i])
	}
	hubs, ondeckHub := s.placeHomeOnDeck(ctx, pr, hc, hubs, selected, progress, paintedRows[:len(progress)])

	assembly := frontend.HomeAssembly[model.Hub]{
		Render: func(hub frontend.Hub) model.Hub {
			if hub.ID == "home.ondeck" && ondeckHub != nil {
				return *ondeckHub
			}
			rows := painted[hub.ID]
			painted[hub.ID] = rows[1:]
			return rows[0]
		},
		Identity:    func(row model.Hub) string { return row.HubIdentifier },
		Report:      homeLayoutFacts,
		InLibraries: func(frontend.Hub) bool { return true },
		Continue: func() (frontend.Hub, model.Hub) {
			return progress[0].Hub, paintedRows[0]
		},
		Playlists: func() (model.Hub, bool) {
			hub, _, ok := s.playlistsHubProjection(ctx, pr, count)
			return hub, ok
		},
		Discovery: func() []frontend.Hub { return discovery },
		Live: func() (model.Hub, bool) {
			hub, _ := s.liveTVHubProjection(r)
			if hub == nil {
				return model.Hub{}, false
			}
			return *hub, true
		},
		Keep: func(row model.Hub) bool { return selected == nil || selected[row.HubIdentifier] },
		Layout: func(rows []model.Hub, hubs []frontend.Hub, neutral []frontend.NeutralHomeRow) []model.Hub {
			return s.arrangeHomeRows(ctx, pr, hc, "home", rows, hubs, neutral)
		},
	}
	rows, served := assembly.Rows(hubs, includeContinue, promotedOnly, true, frontend.HomePurpose)
	mc.Hub = heroReadyHomeHubs(stableHomeHubs(pr, rows))
	if pr.HasPage {
		total := len(mc.Hub)
		lo, hi := pr.window(total)
		mc.TotalSize = model.Ptr(total)
		mc.Offset = model.Ptr(lo)
		mc.Hub = mc.Hub[lo:hi]
	}
	mc.Size = len(mc.Hub)
	return homeBuild{mc: mc, served: served, provisional: provisional}
}

// hasRecommendedRows reports recommender rows beyond the Continue Watching
// placeholder: without them the viewer's model was not ready yet.
func hasRecommendedRows(hubs []frontend.Hub) bool {
	for _, h := range hubs {
		if h.ID != "home.continue" {
			return true
		}
	}
	return false
}

// hubContext is the personalization context of a hubs request: the
// device class from the client's X-Plex-* headers, and the daypart and
// weekend flag in server local time.
func (s *Server) hubContext(pr *Request, section int) frontend.HubContext {
	hc := frontend.NewHubContext(s.now().Local(), pr.Product, pr.Platform, pr.Device)
	hc.SectionID = section
	hc.ClientID = pr.ClientID
	hc.SessionID = pr.SessionID
	hc.Language = pr.Language
	return hc
}

// recommendHome asks for Home rows, with the request context when the
// recommender adapts to it.
func (s *Server) recommendHome(ctx context.Context, pr *Request, hc frontend.HubContext, count int) ([]frontend.Hub, error) {
	return (s.exp().NewHomeSelection(frontend.HomeSelectionOptions{Source: s.recommender(), Visibility: s.visibility(), Restricted: s.viewerRestricted(pr.User)})).Rows(ctx, pr.User, hc, "", count)
}

func (s *Server) recommendSection(ctx context.Context, pr *Request, t frontend.MediaType, hc frontend.HubContext, count int) ([]frontend.Hub, error) {
	return (s.exp().NewHomeSelection(frontend.HomeSelectionOptions{Source: s.recommender(), Visibility: s.visibility(), Restricted: s.viewerRestricted(pr.User)})).Rows(ctx, pr.User, hc, t, count)
}

func (s *Server) recommendSectionFor(ctx context.Context, pr *Request, sec Section, hc frontend.HubContext, count int) ([]frontend.Hub, error) {
	return (s.exp().NewHomeSelection(frontend.HomeSelectionOptions{Source: s.recommender(), Visibility: s.sectionVisibility(sec), Restricted: s.viewerRestricted(pr.User)})).Rows(ctx, pr.User, hc, sec.Type, count)
}

// warmBrowseLink opens at most one already-visible playable after the hub
// response is written: the viewer's own Continue Watching / On Deck item,
// never a recommendation nobody chose. It only resolves and opens the
// cached/debrid link (no opening prefetch), and Prewarm's existing per-user
// / rate budgets still apply.
func (s *Server) warmBrowseLink(ctx context.Context, pr *Request, rows []model.Hub) {
	if pr == nil || pr.User == nil || libraryToolClient(pr) {
		return
	}
	pw, ok := s.playback().(frontend.Prewarmer)
	if !ok {
		return
	}
	key, ok := firstBrowseWarmKey(rows)
	if !ok {
		return
	}
	pw.Prewarm(frontend.DetachQueryStats(context.WithoutCancel(ctx)), pr.User, key, s.clientCaps(pr), frontend.PrewarmOptions{Reason: frontend.PrewarmBrowse})
}

// firstBrowseWarmKey is the first movie or episode in a row of the viewer's
// own progress (Continue Watching, a section's in-progress row or On Deck).
// Rows of recommendations, trending or anything else are never warmed: each
// warm-up searches the indexers for a title the viewer may never play.
func firstBrowseWarmKey(rows []model.Hub) (frontend.RatingKey, bool) {
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
			k, err := frontend.ParseRatingKey(md.RatingKey)
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
func (s *Server) recordImpressions(pr *Request, hc frontend.HubContext, written []model.Hub, served map[string]frontend.Hub) {
	sink, ok := s.recommender().(frontend.ImpressionSink)
	if !ok || pr.User == nil || len(written) == 0 {
		return
	}
	out := make([]frontend.Hub, 0, len(written))
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
	if pr.User == nil || s.playlists() == nil {
		return model.Hub{}, frontend.Row{}, false
	}
	pls, err := s.playlists().List(ctx, pr.User.ID)
	if err != nil {
		return model.Hub{}, frontend.Row{}, false
	}
	owned := pls[:0:0]
	plKeys := make([]frontend.RatingKey, 0, len(pls))
	for _, pl := range pls {
		if pl.UserID != pr.User.ID {
			continue
		}
		owned = append(owned, pl)
		plKeys = append(plKeys, pl.Key)
	}
	var states map[frontend.RatingKey]frontend.WatchStatus
	if s.state() != nil && len(plKeys) > 0 {
		states, _ = s.state().GetMany(ctx, pr.User.ID, plKeys)
	}
	meta := make(map[frontend.RatingKey]model.Metadata, len(owned))
	for i := range owned {
		st, hasState := states[owned[i].Key]
		meta[owned[i].Key] = s.playlistListingMetadataWithState(ctx, &owned[i], s.playlistType(ctx, &owned[i]), st, hasState)
	}
	pls = owned
	sort.SliceStable(pls, func(i, j int) bool { return meta[pls[i].Key].LastViewedAt > meta[pls[j].Key].LastViewedAt })
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
	row := frontend.Row{ID: "home.playlists", Title: "Recent Playlists", Purpose: frontend.RowPurpose("playlist"), Style: "shelf", Promoted: true, More: more}
	keys := make([]string, 0, len(pls))
	for i := range pls {
		h.Metadata = append(h.Metadata, meta[pls[i].Key])
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
				mc.Hub = []model.Hub{s.renderHub(ctx, pr, hubSpec{Hub: frontend.Hub{ID: "continueWatching", Title: "Continue Watching"}}, hubCtx{})}
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
			items := make([]frontend.Title, 0, len(spec.Items))
			for _, title := range spec.Items {
				if !isKindSection(sec) && titleInSectionType(title.Type, sec.Type) {
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
		if layout, ok := s.recommender().(frontend.HomeRowLayoutGate); ok && !layout.HomeRowEnabled(ctx, pr.User, s.hubContext(pr, 0), "home.continue") {
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
		ids := parseSectionList(raw)
		for _, id := range ids {
			allowed[id] = true
		}
		// The filter and the section each row then reports agree: a title
		// the named sections list is attributed to them.
		ctx = withPreferredSections(ctx, ids...)
		secs := s.sectionsForTitles(ctx, hs.Items)
		filtered := hs.Items[:0]
		for _, item := range hs.Items {
			if section, ok := secs[item.Key]; ok && allowed[section.ID] {
				filtered = append(filtered, item)
			}
		}
		hs.Items = filtered
	}
	mc := hubContainer()
	opts := renderOpts{media: true, state: hs.state, detailEpisodes: !hs.plain}
	if v == cwSection && isCustomTVSection(sec) {
		vis := s.sectionVisibility(sec)
		opts.vis, opts.section = &vis, &sec
	}
	items := s.hubPageTitles(ctx, pr, hs.Items, &opts)
	total := len(items)
	start := min(max(pr.Start, 0), total)
	count := countParam(r, 50)
	if pr.HasPage && pr.Size >= 0 {
		count = min(pr.Size, 200)
	}
	end := start + min(count, total-start)
	mc.Metadata = s.renderTitles(ctx, pr, items[start:end], opts)
	if hs.included {
		stampIncluded(mc.Metadata)
	}
	if r.URL.Query().Get("includeMeta") == "1" {
		mc.Meta = &model.Meta{}
		for _, section := range s.userSections(pr.User) {
			mc.Meta.Type = append(mc.Meta.Type, s.metaType(section, section.Type, ""))
		}
	}
	mc.Size = len(mc.Metadata)
	if v != cwSection || pr.HasPage {
		mc.SetPage(start, total)
	}
	s.write(w, r, mc)
}

// Select page membership before decorating the titles.
func (s *Server) hubPageTitles(ctx context.Context, pr *Request, items []frontend.Title, opts *renderOpts) []frontend.Title {
	opts.listRender = len(items) > 1
	opts.membershipChecked = true
	ctx = withStoredPolicy(ctx)
	vis := s.visibilityCtx(ctx)
	if opts.vis != nil {
		vis = *opts.vis
	}
	ctx = s.withSeriesRows(ctx, pr.User, items)
	return s.openableTitlesWith(ctx, vis, pr.User, items, opts.availabilityInSQL || shelfContainsAll(ctx, items))
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
	if pr.HasPage {
		// Assembly assigns candidates across rows based on the requested count.
		// Every page must therefore use the same bounded assembly window.
		count = 200
	}
	mc := hubContainer()
	if s.writeCollectionCardPage(w, r, pr, id) {
		return
	}
	rawDir := strings.TrimSpace(q.Get("contentDirectoryID"))
	sections := parseSectionList(rawDir)
	if rawDir != "" {
		sections = s.allowedSectionIDs(pr.User, sections)
	}
	var ranked bool
	var total int
	if (rawDir == "" || len(sections) > 0) && id != "" {
		if hs, ok := s.findHub(ctx, pr, id, sections, q.Get("metadataID"), count); ok {
			hc := hubCtx{pageItems: true}
			if len(sections) == 1 {
				hc.section = sections[0]
			}
			if k, err := frontend.ParseRatingKey(q.Get("metadataID")); err == nil {
				hc.related = k
			}
			hc, vis := s.hubVisibility(hs, hc)
			prepared := s.prepareHub(ctx, pr, hs, hc, vis)
			mh := prepared.paint(ctx)
			total = prepared.total
			mc.Metadata = mh.Metadata
			ranked = mh.RankedTop10
		}
	}
	start, _ := pr.window(total)
	mc.Size = len(mc.Metadata)
	mc.SetPage(start, total)
	if ranked {
		paintRankedCollection(pr, mc, start)
	}
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
	// A library block's hub id is <rowID>@<lib>; Plex's own rows resolve by row id.
	native := rowHubID(id)
	switch {
	case native == "tv.upnext":
		k, err := frontend.ParseRatingKey(metadataID)
		if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
			return hubSpec{}, false
		}
		return s.upNextHub(ctx, pr, k)
	case native == "home.continue":
		return s.continueHub(ctx, pr, cwHome, Section{}, count), true
	case native == "continueWatching":
		return s.continueHub(ctx, pr, cwGlobal, Section{}, count), true
	case strings.HasPrefix(native, "movie.inprogress.") || strings.HasPrefix(native, "tv.inprogress."):
		if sec, ok := s.sectionFromHubID(native); ok && s.canSeeSection(pr.User, sec.ID) {
			return s.continueHub(ctx, pr, cwSection, sec, count), true
		}
		return hubSpec{}, false
	case strings.HasPrefix(native, "movie.recentlyviewed.") || strings.HasPrefix(native, "tv.recentlyviewed."):
		if sec, ok := s.sectionFromHubID(native); ok && s.canSeeSection(pr.User, sec.ID) {
			if hs, err := s.recentlyWatchedHub(ctx, pr, sec, count); err == nil {
				return hs, true
			}
		}
		return hubSpec{}, false
	case strings.HasPrefix(native, "movie.recentlyadded.") || strings.HasPrefix(native, "tv.recentlyadded."):
		if sec, ok := s.sectionFromHubID(native); ok && s.canSeeSection(pr.User, sec.ID) {
			if hs, err := s.recentlyAddedHub(ctx, pr, sec, count); err == nil {
				return hs, true
			}
		}
		return hubSpec{}, false
	}
	section := 0
	if len(sections) == 1 {
		section = sections[0]
	}
	cacheKey := s.emittedHubKey(ctx, pr, id, section)
	s.emittedHubMu.Lock()
	remembered, found := s.emittedHubs[cacheKey]
	s.emittedHubMu.Unlock()
	if found && (remembered.related == 0 || metadataID == "") && s.now().Before(remembered.expires) && s.hubInSections(remembered.spec.Hub, sections) &&
		(remembered.related == 0 || (metadataID == "" || metadataID == remembered.related.String()) && s.relatedSourceVisible(ctx, pr, remembered.related)) {
		return remembered.spec, true
	}
	candidates := candidateHubIDs(id)
	singleKindSection := false
	if len(sections) == 1 {
		if sec, ok := s.sectionByID(sections[0]); ok && isKindSection(sec) {
			singleKindSection = true
		}
	}
	if !singleKindSection {
		for _, h := range s.discoveryHubsFor(ctx, pr, count, section != 0) {
			if !matchHubID(h.ID, candidates) || !s.hubInSections(h.Hub, sections) {
				continue
			}
			if len(sections) == 1 {
				// A section's copy of a mixed row ("More" from a library).
				if sec, ok := s.sectionByID(sections[0]); ok {
					if v, ok := frontend.SectionView(h.Hub, sec.Type); ok {
						v.Items = s.filterSectionMembers(ctx, sec, v.Items)
						h.Hub = v
					}
				}
			}
			h.ID = id
			return h, true
		}
	}
	if s.recommender() == nil {
		return hubSpec{}, false
	}
	if k, err := frontend.ParseRatingKey(metadataID); err == nil && s.relatedSourceVisible(ctx, pr, k) {
		if hubs, err := s.recommender().Related(ctx, pr.User, k, count); err == nil {
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
		if isPluginKindSection(sec) {
			_, kindServed := s.pluginSectionHubs(ctx, pr, sec, count)
			for hid, h := range kindServed {
				if matchHubID(hid, candidates) {
					h.ID = id
					return hubSpec{Hub: h}, true
				}
			}
			continue
		}
		if !isVideoSection(sec) {
			continue
		}
		if hubs, err := s.recommendSectionFor(ctx, pr, sec, s.hubContext(pr, sec.ID), count); err == nil {
			for _, h := range hubs {
				if matchHubID(h.ID, candidates) {
					if view, ok := frontend.SectionView(h, sec.Type); ok {
						view.ID = id
						return hubSpec{Hub: view}, true
					}
				}
			}
		}
	}
	if src, ok := s.recommender().(frontend.HubItemSource); ok {
		secID := 0
		if len(sections) == 1 {
			secID = sections[0]
		}
		for _, candID := range candidates {
			if h, ok := src.HubItemsFor(ctx, pr.User, s.hubContext(pr, secID), candID, count); ok && s.hubInSections(h, sections) {
				if len(sections) == 1 {
					if sec, ok := s.sectionByID(sections[0]); ok {
						if view, ok := frontend.SectionView(h, sec.Type); ok {
							h = view
						} else {
							continue
						}
					}
				}
				h.ID = id
				return hubSpec{Hub: h}, true
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
func (s *Server) relatedSourceVisible(ctx context.Context, pr *Request, k frontend.RatingKey) bool {
	return s.detailRelated(ctx, pr).SourceVisible(ctx, pr.User, k)
}

func (s *Server) detailRelated(ctx context.Context, pr *Request) frontend.DetailRelated {
	return s.exp().NewDetailRelated(frontend.DetailRelatedOptions{Source: s.recommender(), Visibility: s.visibility(), Get: func(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
		return s.metadataTitle(ctx, pr, key)
	}})
}

// handleMetadataHubs is /hubs/metadata/{key}[/related] and
// /library/metadata/{key}/related.
func (s *Server) handleMetadataHubs(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(s.exp().NoProviderWait(r.Context()))
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := keyParam(r)
	if err == nil && k.Kind() == frontend.KindPlaylist {
		if s.playlists() == nil {
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
		if _, lookupErr := s.metadataTitle(ctx, pr, k); errors.Is(lookupErr, frontend.ErrNotFound) {
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
		if sec, ok := s.sectionForTitle(ctx, title); ok {
			mc.LibrarySectionID = model.Ptr(sec.ID)
			mc.LibrarySectionTitle = sec.Title
			mc.LibrarySectionUUID = sec.UUID
		}
	}
	if s.recommender() != nil {
		hubs, err := s.detailRelated(ctx, pr).Rows(ctx, pr.User, k, countParam(r, 12))
		if err != nil && !errors.Is(err, frontend.ErrNotFound) {
			s.log.Debug("related hubs", "key", k, "err", err)
		}
		mc.Hub = s.renderHubs(ctx, pr, hubs, hubCtx{related: k})
	}
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// upNextHub is shared by post-play and the advertised shelf-items URL so
// clients can reload the row or use it as a play-queue source.
func (s *Server) upNextHub(ctx context.Context, pr *Request, k frontend.RatingKey) (hubSpec, bool) {
	if k.Kind() != frontend.KindEpisode {
		return hubSpec{}, false
	}
	title, err := s.metadataTitle(ctx, pr, k)
	if err != nil || title == nil || titleShow(title) == 0 {
		return hubSpec{}, false
	}
	ep, state := s.showOnDeck(ctx, pr, titleShow(title), nil)
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
	return hubSpec{Hub: frontend.Hub{ID: "tv.upnext", Title: upNextTitle(pr.Language), Type: frontend.TypeEpisode,
		Context: "hub.tv.upnext", Items: []frontend.Title{*ep}}, state: state}, true
}

// handlePostplay is /hubs/metadata/{key}/postplay. An episode with a
// next episode keeps Up Next first; everything else leads with the
// viewer's personalized hook queue (#30, the same #1 pick prefetch
// pre-buffered), then the related rows. Without a PostPlayer this is the
// old behaviour: Up Next, then related.
func (s *Server) handlePostplay(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(s.exp().NoProviderWait(r.Context()))
	ctx := r.Context()
	pr := FromContext(ctx)
	k, err := frontend.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil || !s.relatedSourceVisible(ctx, pr, k) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	count := countParam(r, 12)
	mc := hubContainer()
	mc.SetSyncProjection(false)
	mc.Hub, err = frontend.AssemblePostPlay(ctx, s.postPlaySelection(ctx, pr, k, !s.viewerRestricted(pr.User)), pr.User, k, count,
		func() (frontend.RatingKey, model.Hub, bool) {
			hub, ok := s.upNextHub(ctx, pr, k)
			if !ok {
				return 0, model.Hub{}, false
			}
			return hub.Items[0].Key, s.renderHub(ctx, pr, hub, hubCtx{related: k}), true
		}, func(row frontend.PostPlayRow) (model.Hub, bool) { return s.postPlayHub(ctx, pr, k, row) },
		func(hub frontend.Hub) model.Hub { return s.renderHub(ctx, pr, hubSpec{Hub: hub}, hubCtx{related: k}) })
	if err != nil {
		s.log.Debug("postplay related hubs", "key", k, "err", err)
	}
	mc.Size = len(mc.Hub)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Search

type searchHubDef struct {
	id    string
	typ   string
	title string
}

// emptyHub is the hub as PMS sends it with no results.
func (hd searchHubDef) emptyHub() model.Hub {
	return model.Hub{Title: hd.title, Type: hd.typ, HubIdentifier: hd.id, Style: "shelf"}
}

// emptyHubSearch is PMS's /hubs/search answer with no results: all 17
// hubs, each empty. PMS never answers a hub search without its hubs.
func emptyHubSearch() *model.MediaContainer {
	mc := &model.MediaContainer{Hub: make([]model.Hub, 0, len(searchHubDefs))}
	for _, hd := range searchHubDefs {
		mc.Hub = append(mc.Hub, hd.emptyHub())
	}
	mc.Size = len(mc.Hub)
	return mc
}

// writeSearchErr is writeErr for a search endpoint. A search the client's
// next keystroke superseded answers with empty: the endpoint's own
// no-results shape, which every client already parses. PMS never sends a
// /hubs/search without its hubs; strict clients such as Plex for Roku need
// not cope with one (W373).
func (s *Server) writeSearchErr(w http.ResponseWriter, r *http.Request, err error, empty *model.MediaContainer) {
	if frontend.SearchWasSuperseded(r.Context()) {
		s.write(w, r, empty)
		return
	}
	s.writeErr(w, r, err)
}

// searchHubDefs is PMS's 17-hub set and order for /hubs/search and
// /hubs/search/voice.
var searchHubDefs = []searchHubDef{
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
	SearchPeople(ctx context.Context, terms []string, limit int) ([]frontend.Person, error)
	PersonCreditKeys(ctx context.Context, person int64, types []frontend.MediaType, limit int) ([]frontend.RatingKey, error)
}

// peopleBatchCatalog reads every searched person's credits and their
// titles' visibility rows in a few statements (catalog/people_batch.go).
type peopleBatchCatalog interface {
	PersonCreditTitles(ctx context.Context, persons []int64, types []frontend.MediaType, limit int, facts frontend.TitleFacts) (map[int64][]frontend.PersonCredit, map[frontend.RatingKey]frontend.Title, error)
	CachedVisibilityRows(ctx context.Context, keys []frontend.RatingKey, facts frontend.TitleFacts) (map[frontend.RatingKey]frontend.Title, error)
}

type discoveryVisibilityCatalog interface {
	DiscoveryVisibleForViewer(*frontend.User, *frontend.Title) bool
}

type reasonedTitle struct {
	title       frontend.Title
	reason      string
	reasonID    string
	reasonTitle string
}

type peopleSearchResult struct {
	actors        []model.Directory
	actorsMore    bool
	directors     []model.Directory
	directorsMore bool
	titles        map[frontend.MediaType][]reasonedTitle
	// rows: titles are visibility rows, to be read in full when shown.
	rows bool
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
func titleSearchScore(t *frontend.Title, i, n int) float64 {
	if t.SearchScore > 0 {
		return t.SearchScore
	}
	return searchScore(i, n)
}

// reportedScore is a relevance as PMS reports it, between 0 and 1.
func reportedScore(score float64) float64 { return min(1, score) }

func peopleQueryTerms(query string) []string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.TrimSpace(query)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		switch r {
		case 'ß':
			b.WriteString("ss")
			continue
		case 'æ':
			b.WriteString("ae")
			continue
		case 'œ':
			b.WriteString("oe")
			continue
		case 'ø':
			r = 'o'
		case 'ł':
			r = 'l'
		case 'đ', 'ð':
			r = 'd'
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
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

func personInList(people []frontend.Person, tmdb int64) (frontend.Person, bool) {
	for _, p := range people {
		if p.TMDB == tmdb {
			return p, true
		}
	}
	return frontend.Person{}, false
}

func personSearchDirectory(sec Section, person frontend.Person, kind string, count int, thumbKey frontend.RatingKey, inHub bool, tagKey string) model.Directory {
	idStr := strconv.FormatInt(person.TMDB, 10)
	secIDStr := strconv.Itoa(sec.ID)
	tagType := 6
	if kind == "director" {
		tagType = 4
	}
	secType := 1
	if sec.Type == frontend.TypeShow {
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
	)
	if frontend.IsLibraryPersonTagKey(tagKey) {
		attrs = append(attrs, model.A("tagKey", tagKey))
	}
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
	out := peopleSearchResult{titles: map[frontend.MediaType][]reasonedTitle{}}
	ps, ok := s.catalog().(peopleSearchCatalog)
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
	queryTypes := make([]frontend.MediaType, 0, len(allowedTypes)+1)
	allowedSet := map[frontend.MediaType]bool{}
	for _, t := range allowedTypes {
		allowedSet[t] = true
		queryTypes = append(queryTypes, t)
		// Only /hubs/search emits reasoned episode rows; /library/search uses
		// section directory pills whose sections are never episode sections.
		if inHub && t == frontend.TypeShow {
			allowedSet[frontend.TypeEpisode] = true
			queryTypes = append(queryTypes, frontend.TypeEpisode)
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
	// An explicit search: rules the owner limited to browsing do not apply to
	// reasoned title hits. Section directory pills link to /library/sections/{id}/all
	// (Browse), so they only count titles Browse itself will return (#653).
	catVis, _ := s.catalog().(frontend.SearchVisibilityCatalog)
	browseVis, _ := s.catalog().(discoveryVisibilityCatalog)
	// The batched path judges rows; it resolves their shows from rows too.
	vis := s.visibility()
	now := s.now()
	creditLimit := min(max(limit*3, 30), 120)
	// A person whose whole name is the query leads, then the most credited.
	// Each person costs a credit walk, so only as many are walked as a hub
	// can show, with headroom for people with nothing visible.
	sort.SliceStable(candidates, func(i, j int) bool {
		return personSearchScore(query, candidates[i].Name) > personSearchScore(query, candidates[j].Name)
	})
	walkBudget, partialBudget := limit+limit/2, min(max(limit/2, 3), 15)
	if !inHub {
		creditLimit = min(max(limit, 30), 60)
		walkBudget = min(walkBudget, 60)
		partialBudget = min(partialBudget, 12)
	}
	var walked []frontend.Person
	budgetSpent := false
	for _, cand := range candidates {
		if frontend.IsPluginPersonID(cand.TMDB) || !personMatchesTerms(cand.Name, terms) {
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
			budgetSpent = true
			break
		}
		walkBudget--
		walked = append(walked, cand)
	}
	// Each walked person's credited titles and roles. With the batched
	// catalogue reads it is a few statements for all of them, and the
	// titles carry only what their visibility needs; the ones a hub can
	// show are read in full below.
	type personTitles struct {
		titles    []frontend.Title
		browsable func(frontend.RatingKey) bool
		role      func(t *frontend.Title, kind string) (frontend.Person, bool)
	}
	byPerson := make([]personTitles, len(walked))
	batch, batched := s.catalog().(peopleBatchCatalog)
	if batched {
		ids := make([]int64, len(walked))
		for i, p := range walked {
			ids[i] = p.TMDB
		}
		credits, rows, err := batch.PersonCreditTitles(withStoredPolicy(ctx), ids, queryTypes, creditLimit, vis.TitleFacts(pr.User))
		if err != nil {
			return out
		}
		if err := s.peopleVisibilityRows(ctx, batch, &vis, pr.User, rows); err != nil {
			return out
		}
		for i, p := range walked {
			roles := map[frontend.RatingKey]map[string]frontend.Person{}
			browsableKeys := map[frontend.RatingKey]bool{}
			var titles []frontend.Title
			for _, cr := range credits[p.TMDB] {
				if cr.Browsable {
					browsableKeys[cr.Key] = true
				}
				if roles[cr.Key] == nil {
					roles[cr.Key] = map[string]frontend.Person{}
					if t, ok := rows[cr.Key]; ok {
						titles = append(titles, t)
					}
				}
				if _, ok := roles[cr.Key][cr.Kind]; !ok {
					roles[cr.Key][cr.Kind] = cr.Person
				}
			}
			byPerson[i] = personTitles{
				titles:    titles,
				browsable: func(k frontend.RatingKey) bool { return browsableKeys[k] },
				role: func(t *frontend.Title, kind string) (frontend.Person, bool) {
					person, ok := roles[t.Key][kind]
					return person, ok
				},
			}
		}
	} else {
		for i, p := range walked {
			keys, err := ps.PersonCreditKeys(ctx, p.TMDB, queryTypes, creditLimit)
			if err != nil || len(keys) == 0 {
				continue
			}
			tmdbID := p.TMDB
			byPerson[i] = personTitles{
				titles:    s.getTitles(withStoredPolicy(ctx), keys),
				browsable: func(frontend.RatingKey) bool { return true },
				role: func(t *frontend.Title, kind string) (frontend.Person, bool) {
					if kind == "cast" {
						return personInList(t.Cast, tmdbID)
					}
					return personInList(t.Directors, tmdbID)
				},
			}
		}
	}
	walkedIDs := make([]int64, 0, len(walked))
	for _, cand := range walked {
		if cand.TMDB > 0 {
			walkedIDs = append(walkedIDs, cand.TMDB)
		}
	}
	personTagKeys := s.plexPeopleByTMDB(ctx, walkedIDs)
	// The section a credited title counts under is the one that lists it,
	// resolved for every walked person's titles in one pass.
	var credited []frontend.Title
	for _, p := range byPerson {
		credited = append(credited, p.titles...)
	}
	titleSecs := s.sectionsForTitles(ctx, credited)
	var actorTitles, directorTitles []reasonedTitle
	for ci, cand := range walked {
		titles, browsable, role := byPerson[ci].titles, byPerson[ci].browsable, byPerson[ci].role
		if len(titles) == 0 {
			continue
		}
		type secHit struct {
			count     int
			thumbPath string
			thumbKey  frontend.RatingKey
		}
		actorSecs := map[int]*secHit{}
		directorSecs := map[int]*secHit{}
		idStr := strconv.FormatInt(cand.TMDB, 10)
		for i := range titles {
			t := titles[i]
			if t.Key.IsPluginKey() || !allowedSet[t.Type] {
				continue
			}
			if catVis != nil && !catVis.SearchVisible(&t) {
				continue
			}
			if t.Adult || !t.IsReleased(now) {
				continue
			}
			if !s.visibleTitleWith(ctx, vis, pr.User, &t) {
				continue
			}
			sec, hasSec := titleSecs[t.Key]
			secBrowsable := hasSec && t.Type == sec.Type && browsable(t.Key) && (browseVis == nil || browseVis.DiscoveryVisibleForViewer(pr.User, &t))
			if p, ok := role(&t, "cast"); ok {
				if secBrowsable {
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
				if inHub {
					actorTitles = append(actorTitles, reasonedTitle{
						title:       t,
						reason:      "actor",
						reasonID:    idStr,
						reasonTitle: cand.Name,
					})
				}
			}
			if p, ok := role(&t, "director"); ok {
				if secBrowsable {
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
				if inHub {
					directorTitles = append(directorTitles, reasonedTitle{
						title:       t,
						reason:      "director",
						reasonID:    idStr,
						reasonTitle: cand.Name,
					})
				}
			}
		}
		for _, sec := range secs {
			plexTagKey := personTagKeys[cand.TMDB].TagKey
			if h := actorSecs[sec.ID]; h != nil && h.count > 0 {
				if len(out.actors) < limit {
					person := frontend.Person{TMDB: cand.TMDB, Name: cand.Name, ThumbPath: h.thumbPath}
					out.actors = append(out.actors, personSearchDirectory(sec, person, "actor", h.count, h.thumbKey, inHub, plexTagKey))
					out.actorScores = append(out.actorScores, personSearchScore(query, cand.Name))
				} else {
					out.actorsMore = true
				}
			}
			if h := directorSecs[sec.ID]; h != nil && h.count > 0 {
				if len(out.directors) < limit {
					person := frontend.Person{TMDB: cand.TMDB, Name: cand.Name, ThumbPath: h.thumbPath}
					out.directors = append(out.directors, personSearchDirectory(sec, person, "director", h.count, h.thumbKey, inHub, plexTagKey))
					out.directorScores = append(out.directorScores, personSearchScore(query, cand.Name))
				} else {
					out.directorsMore = true
				}
			}
		}
	}
	if budgetSpent {
		// More people matched than were walked.
		out.actorsMore = out.actorsMore || len(out.actors) >= limit
		out.directorsMore = out.directorsMore || len(out.directors) >= limit
	}
	seenByType := map[frontend.MediaType]map[frontend.RatingKey]bool{}
	addReasoned := func(rt reasonedTitle) {
		seen := seenByType[rt.title.Type]
		if seen == nil {
			seen = map[frontend.RatingKey]bool{}
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
	if batched {
		// A hub shows at most limit titles of a type. They are visibility
		// rows: the hub reads in full the ones it shows (hubReasoned).
		for typ, rts := range out.titles {
			out.titles[typ] = rts[:min(len(rts), limit)]
		}
		out.rows = true
	}
	return out
}

// hubSearchCandidates asks the catalogue for the search's candidate window
// as rows with what the viewer's policy reads, and returns how a hub reads
// the rows it shows as list items. Nil when the catalogue cannot.
func (s *Server) hubSearchCandidates(ctx context.Context, u *frontend.User) (context.Context, func([]frontend.Title) ([]frontend.Title, error)) {
	list, ok := s.catalog().(frontend.SearchListDetailsCatalog)
	if !ok {
		return ctx, nil
	}
	vis := s.visibility()
	judge, known := vis.FactJudge(frontend.WithCatalogViewer(ctx, u))
	if !known {
		return ctx, nil
	}
	ctx = frontend.WithSearchCandidates(ctx, &frontend.SearchCandidates{Facts: vis.TitleFacts(u), Detail: judge})
	return ctx, func(items []frontend.Title) ([]frontend.Title, error) { return list.SearchListDetails(ctx, items) }
}

// hubReasoned is the titles people found for a hub of typ: those not in seen,
// in order, as many as fill room and one more (the hub's More), read in
// full when they are visibility rows. A title no longer readable gives its
// place to the next, as it did when every reasoned title was read first.
func (s *Server) hubReasoned(ctx context.Context, people peopleSearchResult, typ frontend.MediaType, seen map[frontend.RatingKey]bool, room int) []reasonedTitle {
	var cands []reasonedTitle
	for _, rt := range people.titles[typ] {
		if !seen[rt.title.Key] {
			cands = append(cands, rt)
		}
	}
	if !people.rows {
		return cands
	}
	want := room + 1
	if room < 0 {
		want = 0
	}
	var out []reasonedTitle
	for next := 0; len(out) < want && next < len(cands); {
		part := cands[next:min(next+want-len(out), len(cands))]
		next += len(part)
		keys := make([]frontend.RatingKey, 0, len(part))
		full := map[frontend.RatingKey]frontend.Title{}
		for _, rt := range part {
			if rt.title.Key.IsPluginKey() {
				full[rt.title.Key] = rt.title
			} else {
				keys = append(keys, rt.title.Key)
			}
		}
		for _, t := range s.getTitles(withStoredPolicy(ctx), keys) {
			full[t.Key] = t
		}
		for _, rt := range part {
			if t, ok := full[rt.title.Key]; ok {
				rt.title = t
				out = append(out, rt)
			}
		}
	}
	return out
}

// peopleVisibilityRows completes a people search's visibility rows
// (PersonCreditTitles) for vis's decisions for u: titles availability
// judges on their facts are read in full, and vis resolves the shows of
// seasons and episodes from their rows.
func (s *Server) peopleVisibilityRows(ctx context.Context, batch peopleBatchCatalog, vis *frontend.Visibility, u *frontend.User, rows map[frontend.RatingKey]frontend.Title) error {
	ctx = withStoredPolicy(ctx)
	judge, known := vis.FactJudge(frontend.WithCatalogViewer(ctx, u))
	var detail, shows []frontend.RatingKey
	seenShow := map[frontend.RatingKey]bool{}
	for key, t := range rows {
		if !known || (judge != nil && judge(t)) {
			detail = append(detail, key)
		}
		if t.ShowKey != 0 && !seenShow[t.ShowKey] {
			seenShow[t.ShowKey] = true
			shows = append(shows, t.ShowKey)
		}
	}
	if len(detail) > 0 {
		for _, t := range s.getTitles(ctx, detail) {
			rows[t.Key] = t
		}
	}
	if len(shows) == 0 {
		return nil
	}
	parents, err := batch.CachedVisibilityRows(ctx, shows, vis.TitleFacts(u))
	if err != nil {
		return err
	}
	get := vis.GetTitle
	vis.GetTitle = func(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
		if t, ok := parents[key]; ok {
			return &t, nil
		}
		if get == nil {
			return nil, frontend.ErrNotFound
		}
		return get(ctx, key)
	}
	return nil
}

func (s *Server) searchRequestContext(ctx context.Context) context.Context {
	return frontend.WithExplicitSearch(frontend.WithSearchMemo(s.exp().NoProviderWait(ctx)))
}

// enterSearch is searchRequestContext for r, made the latest search of the
// viewer's client: the next keystroke's search cancels it and drops the
// remote lookups only it wanted. leave must run when the request ends.
func (s *Server) enterSearch(r *http.Request, query string) (*http.Request, func()) {
	ctx := s.searchRequestContext(r.Context())
	leave := func() {}
	if pr := FromContext(ctx); pr != nil && pr.User != nil {
		ctx, leave = frontend.EnterSearch(ctx, pr.User.ID, s.searchClientID(pr), query)
	}
	return r.WithContext(ctx), leave
}

func (s *Server) searchTypes(u *frontend.User, sectionID int) []frontend.MediaType {
	var out []frontend.MediaType
	seen := map[frontend.MediaType]bool{}
	for _, sec := range s.userSections(u) {
		if sectionID > 0 && sec.ID != sectionID {
			continue
		}
		if !seen[sec.Type] {
			seen[sec.Type] = true
			out = append(out, sec.Type)
		}
	}
	return out
}

func (s *Server) searchExperience() frontend.Search {
	unrestricted := func(u *frontend.User) bool { return !s.viewerRestricted(u) }
	src := s.searchSources()
	if src.Catalog == nil {
		// A server built outside the app (tests, spike mode) searches its
		// own dependencies the same way.
		searcher := s.vibeSearch()
		if searcher == nil {
			searcher, _ = s.collections().(frontend.VibeSearcher)
		}
		src = frontend.SearchSources{Catalog: s.catalog().Search, Familiar: s.exp().FamiliarFrom(s.state())}
		if pt, ok := s.catalog().(frontend.PeopleTitlesCatalog); ok {
			src.PeopleTitles = pt.PeopleTitles
		}
		if searcher != nil {
			src.Vibe = searcher
		}
	}
	return s.exp().SearchFor(src, s.visibility(), unrestricted)
}

func (s *Server) searchClientID(pr *Request) string {
	if pr == nil {
		return ""
	}
	if pr.ClientID != "" {
		return pr.ClientID
	}
	if pr.SessionID != "" {
		return pr.SessionID
	}
	return pr.Token
}

func (s *Server) filterSectionReasoned(ctx context.Context, sec Section, items []reasonedTitle) []reasonedTitle {
	if len(items) == 0 || sec.PluginID != "" || s.exp() == nil {
		return items
	}
	sr, ok := s.exp().(frontend.LibraryScopeReader)
	if !ok {
		return items
	}
	libID := sectionLibraryID(sec)
	if libID == 0 {
		return items
	}
	keys := make([]frontend.RatingKey, len(items))
	for i := range items {
		k := items[i].title.Key
		if (items[i].title.Type == frontend.TypeSeason || items[i].title.Type == frontend.TypeEpisode) && items[i].title.ShowKey != 0 {
			k = items[i].title.ShowKey
		}
		keys[i] = k
	}
	members, scoped, err := sr.LibraryMembers(ctx, libID, keys)
	if err != nil || !scoped {
		return items
	}
	out := make([]reasonedTitle, 0, len(items))
	for i := range items {
		k := items[i].title.Key
		if (items[i].title.Type == frontend.TypeSeason || items[i].title.Type == frontend.TypeEpisode) && items[i].title.ShowKey != 0 {
			k = items[i].title.ShowKey
		}
		if members[k] {
			out = append(out, items[i])
		}
	}
	return out
}

func (s *Server) runSearch(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]frontend.Title, error) {
	pluginItems := s.searchPluginTitles(ctx, pr, query, sectionID, limit)
	ctx = frontend.WithSearchClient(ctx, s.searchClientID(pr))
	var scopedSec Section
	scoped := false
	fetch := s.saferFetchCount(pr.User, limit)
	if sectionID > 0 {
		if sec, ok := s.sectionByID(sectionID); ok && s.sectionHasLibraryScope(ctx, sec, pr.User) {
			scopedSec, scoped = sec, true
			fetch = min(max(fetch*4, 50), 200)
		}
	}
	result, err := s.searchExperience().Resolve(ctx, pr.User, query, s.searchTypes(pr.User, sectionID), fetch, false)
	if scoped {
		result.Items = s.filterSectionMembers(ctx, scopedSec, result.Items)
	}
	result.Items = s.takeVisible(pr.User, result.Items, limit)
	result.Items = mergePluginSearchResults(result.Items, pluginItems, 0, limit)
	return result.Items, err
}

func (s *Server) collectionDirectory(ctx context.Context, c *frontend.Collection) model.Directory {
	m := s.collectionMetadata(c, ctx)
	return model.Directory{
		Key: "/library/collections/" + c.Key.String() + "/children", Type: "collection",
		Title: c.Title, Thumb: m.Thumb, Art: m.Art,
		LeafCount: model.Ptr(c.ChildCount),
		Extra:     []model.Attr{model.A("ratingKey", c.Key.String()), model.A("subtype", string(c.Type)), model.A("childCount", c.ChildCount), model.A("librarySectionID", m.LibrarySectionID)},
	}
}

func (s *Server) searchCollections(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]model.Directory, float64, bool) {
	if s.collections() == nil || strings.TrimSpace(query) == "" {
		return nil, 0, false
	}
	type scored struct {
		col   frontend.Collection
		score float64
	}
	var matches []scored
	foldedName := searchFoldName(query)
	for _, sec := range s.userSections(pr.User) {
		if sectionID > 0 && sec.ID != sectionID {
			continue
		}
		var cs []frontend.Collection
		var err error
		if matching, ok := s.collections().(frontend.OwnedCollectionMatcher); ok {
			cs, err = matching.ListOwnedMatching(ctx, pr.User, sec.Type, foldedName, func(title string) bool {
				return playlistSearchScore(title, query) > 0
			})
		} else if owned, ok := s.collections().(frontend.OwnedCollectionLister); ok {
			cs, err = owned.ListOwned(ctx, pr.User, sec.Type)
		} else {
			cs, err = s.collections().List(ctx, pr.User, sec.Type)
		}
		if err != nil {
			continue
		}
		named := cs[:0]
		scores := map[frontend.RatingKey]float64{}
		for _, c := range cs {
			if sc := playlistSearchScore(c.Title, query); sc > 0 {
				named = append(named, c)
				scores[c.Key] = sc
			}
		}
		for _, c := range s.scopedCollectionCounts(ctx, pr.User, named) {
			matches = append(matches, scored{col: c, score: scores[c.Key]})
		}
	}
	if len(matches) == 0 {
		return nil, 0, false
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return strings.ToLower(matches[i].col.Title) < strings.ToLower(matches[j].col.Title)
	})
	more := false
	if limit > 0 && len(matches) > limit {
		matches, more = matches[:limit], true
	}
	dirs := make([]model.Directory, 0, len(matches))
	for i := range matches {
		dirs = append(dirs, s.collectionDirectory(ctx, &matches[i].col))
	}
	return dirs, matches[0].score, more
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
	r, leave := s.enterSearch(r, r.URL.Query().Get("query"))
	defer leave()
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
	if sectionID > 0 {
		// A title the searched section lists is reported in it.
		ctx = withPreferredSections(ctx, sectionID)
	}
	var scopedSec Section
	scopedSection := false
	fetch := max(limit*2, s.saferFetchCount(pr.User, limit))
	if sectionID > 0 {
		if sec, ok := s.sectionByID(sectionID); ok && s.sectionHasLibraryScope(ctx, sec, pr.User) {
			scopedSec, scopedSection = sec, true
			fetch = min(max(fetch*2, 50), 200)
		}
	}
	ctx = frontend.WithSearchPeopleLimit(ctx, max(64, limit*4))
	// People are searched beside titles; neither waits for the other.
	peopleDone := make(chan peopleSearchResult, 1)
	peopleCtx := ctx // ctx is reassigned below; the goroutine keeps its own copy
	go func() {
		catPeople := s.searchPeople(peopleCtx, pr, query, sectionID, limit, true)
		plugPeople := s.searchPluginPeople(peopleCtx, pr, query, sectionID, limit, true)
		peopleDone <- mergePeopleSearchResults(catPeople, plugPeople, limit)
	}()
	pluginItems := s.searchPluginTitles(ctx, pr, query, sectionID, fetch)
	// Hub items render as list items (three cast members, directors,
	// genres): the catalogue reads those facts for the titles a hub shows,
	// not every credit, keyword and alternative title, and returns the rest
	// of its window as rows (frontend.SearchCandidates).
	ctx = frontend.WithSearchClient(ctx, s.searchClientID(pr))
	searchCtx, listDetails := s.hubSearchCandidates(frontend.WithSearchListFacts(ctx), pr.User)
	result, err := s.searchExperience().Resolve(searchCtx, pr.User, query, s.searchTypes(pr.User, sectionID), fetch, true)
	people := <-peopleDone
	if err != nil {
		s.writeSearchErr(w, r, err, emptyHubSearch())
		return
	}
	if scopedSection {
		result.Items = s.filterSectionMembers(ctx, scopedSec, result.Items)
		for mt, rts := range people.titles {
			people.titles[mt] = s.filterSectionReasoned(ctx, scopedSec, rts)
		}
	}
	vibe, res, isVibe := result.Collection, mergePluginSearchResults(result.Items, pluginItems, result.AIItems, fetch), result.CollectionSearch
	aiRank := map[frontend.RatingKey]int{}
	for i := 0; i < min(result.AIItems, len(res)); i++ {
		aiRank[res[i].Key] = i
	}
	byType := map[frontend.MediaType][]frontend.Title{}
	for _, t := range res {
		typ := frontend.MediaType(s.plexTypeFor(&t))
		if typ == "" {
			typ = t.Type
		}
		byType[typ] = append(byType[typ], t)
	}
	allowed := map[frontend.MediaType]bool{}
	for _, t := range s.searchTypes(pr.User, sectionID) {
		allowed[t] = true
		if t == frontend.TypeShow {
			allowed[frontend.TypeEpisode] = true
		}
		if string(t) == plexBaseArtist {
			allowed[plexBaseAlbum] = true
			allowed[plexBaseTrack] = true
		}
	}
	// PMS returns every result hub, empty ones included: those with results
	// first, best match first, then the rest in searchHubDefs order.
	mc := &model.MediaContainer{}
	var tops []float64 // each hub's best score
	for _, hd := range searchHubDefs {
		top := 0.0
		hubMediaType := frontend.MediaType(hd.typ)
		isKindMediaHub := (hd.id == plexBaseArtist || hd.id == plexBaseAlbum || hd.id == plexBaseTrack || hd.id == plexBasePhoto) &&
			(len(byType[hubMediaType]) > 0 || len(people.titles[hubMediaType]) > 0)
		switch {
		case hd.id == string(frontend.TypeMovie) || hd.id == string(frontend.TypeShow) || hd.id == string(frontend.TypeEpisode) || isKindMediaHub:
			mediaType := frontend.MediaType(hd.typ)
			var items []frontend.Title
			reasons := map[string]reasonedTitle{}
			if allowed[mediaType] {
				peopleByKey := make(map[frontend.RatingKey]reasonedTitle, len(people.titles[mediaType]))
				for _, rt := range people.titles[mediaType] {
					if _, exists := peopleByKey[rt.title.Key]; !exists {
						peopleByKey[rt.title.Key] = rt
					}
				}
				seen := map[frontend.RatingKey]bool{}
				for _, t := range byType[mediaType] {
					seen[t.Key] = true
					items = append(items, t)
					if t.SearchScore < 0.96 {
						if rt, ok := peopleByKey[t.Key]; ok {
							reasons[t.Key.String()] = rt
						}
					}
				}
				for _, rt := range s.hubReasoned(ctx, people, mediaType, seen, limit-len(items)) {
					seen[rt.title.Key] = true
					items = append(items, rt.title)
					reasons[rt.title.Key.String()] = rt
				}
			}
			more := false
			if len(items) > limit {
				items, more = items[:limit], true
			}
			if listDetails != nil {
				if items, err = listDetails(items); err != nil {
					s.writeSearchErr(w, r, err, emptyHubSearch())
					return
				}
			}
			h := frontend.Hub{ID: hd.id, Title: hd.title, Type: mediaType, Items: items, More: more, Style: "shelf"}
			scores := make(map[string]float64, len(items))
			relevant := make(map[string]bool, len(items))
			hubHasAI := false
			bestAIRank := result.AIItems
			for i := range items {
				rankPos := searchScore(i, len(items))
				if rnk, isAI := aiRank[items[i].Key]; isAI {
					hubHasAI = true
					bestAIRank = min(bestAIRank, rnk)
					scores[items[i].Key.String()] = rankPos
					relevant[items[i].Key.String()] = true
				} else {
					sc := titleSearchScore(&items[i], i, len(items))
					if hubHasAI {
						sc = min(sc, rankPos)
					}
					scores[items[i].Key.String()] = sc
					relevant[items[i].Key.String()] = items[i].SearchScore > 0
				}
			}
			mh := s.renderHub(ctx, pr, hubSpec{Hub: h, noPromoted: true}, hubCtx{searchIdentity: true})
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
			if hubHasAI && len(mh.Metadata) > 0 {
				top = max(top, 1.9-float64(bestAIRank)*0.001)
			}
			mc.Hub = append(mc.Hub, mh)
		case hd.id == "collection":
			mh := model.Hub{
				Title:         hd.title,
				Type:          hd.typ,
				HubIdentifier: hd.id,
				Style:         "shelf",
			}
			if isVibe && vibe != nil {
				mh.Directory = []model.Directory{s.collectionDirectory(ctx, vibe)}
				mh.Size = len(mh.Directory)
				if result.AIItems > 0 {
					top = 2.0
				} else {
					top = searchPersonExactScore
				}
			} else if q.Get("includeCollections") == "1" {
				dirs, best, more := s.searchCollections(ctx, pr, query, sectionID, limit)
				mh.Directory = dirs
				mh.Size = len(dirs)
				mh.More = more
				top = best
			}
			mc.Hub = append(mc.Hub, mh)
		case hd.id == "playlist":
			mh := model.Hub{Title: hd.title, Type: hd.typ, HubIdentifier: hd.id, Style: "shelf"}
			mh.Metadata = s.searchPlaylists(ctx, pr, query, q.Get("contentDirectoryID"), limit)
			mh.Size = len(mh.Metadata)
			if mh.Size > 0 {
				top, _ = strconv.ParseFloat(mh.Metadata[0].Score, 64)
			}
			mc.Hub = append(mc.Hub, mh)
		case hd.id == "actor":
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
		case hd.id == "director":
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
			mc.Hub = append(mc.Hub, hd.emptyHub())
		}
		tops = append(tops, top)
	}
	orderSearchHubs(mc.Hub, tops)
	s.appendLiveSearchHubs(r, mc, query, limit)
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
	r, leave := s.enterSearch(r, r.URL.Query().Get("query"))
	defer leave()
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	limit := 50
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 {
		limit = min(n, 200)
	}
	res, err := s.runSearch(ctx, pr, q.Get("query"), 0, limit)
	if err != nil {
		s.writeSearchErr(w, r, err, libraryContainer())
		return
	}
	mc := libraryContainer()
	mc.Metadata = s.renderTitles(ctx, pr, res, renderOpts{media: true, searchIdentity: true})
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

type librarySearchFilter struct {
	filtered             bool
	allow                map[frontend.MediaType]bool
	wantPeople           bool
	wantCollections      bool
	wantOwnedCollections bool
	wantPlaylists        bool
	wantSeasons          bool
	wantEpisodes         bool
	wantLiveTV           bool
}

// parseLibrarySearchTypes parses all searchTypes query values (both comma-
// separated lists and repeated parameters) case-insensitively.
func parseLibrarySearchTypes(q url.Values) librarySearchFilter {
	var f librarySearchFilter
	if q.Get("includeCollections") == "1" {
		f.wantCollections = true
		f.wantOwnedCollections = true
	}
	for _, raw := range q["searchTypes"] {
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.ToLower(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			f.filtered = true
			if f.allow == nil {
				f.allow = map[frontend.MediaType]bool{}
			}
			switch tok {
			case "movies", "movie":
				f.allow[frontend.TypeMovie] = true
				f.wantPlaylists = true
			case "collections", "collection":
				f.wantCollections = true
				f.wantOwnedCollections = true
			case "tv", "shows", "show":
				f.allow[frontend.TypeShow] = true
				f.allow[frontend.TypeEpisode] = true
				f.wantPlaylists = true
			case "seasons", "season":
				f.allow[frontend.TypeSeason] = true
				f.wantSeasons = true
			case "episodes", "episode":
				f.allow[frontend.TypeEpisode] = true
				f.wantEpisodes = true
			case "music":
				f.allow[plexBaseArtist] = true
				f.allow[plexBaseAlbum] = true
				f.allow[plexBaseTrack] = true
			case "artists", "artist":
				f.allow[plexBaseArtist] = true
			case "albums", "album":
				f.allow[plexBaseAlbum] = true
			case "tracks", "track":
				f.allow[plexBaseTrack] = true
			case "photos", "photo":
				f.allow[plexBasePhoto] = true
			case "people", "person", "actors", "actor", "directors", "director":
				f.wantPeople = true
			case "playlists", "playlist":
				f.wantPlaylists = true
			case "livetv", "channels", "channel":
				f.wantLiveTV = true
			}
		}
	}
	if !f.filtered {
		f.wantPeople = true
		f.wantCollections = true
		f.wantPlaylists = true
		f.wantLiveTV = true
	}
	return f
}

// handleLibrarySearch is /library/search, used by the newer apps: results
// wrapped in SearchResult with a score.
func (s *Server) handleLibrarySearch(w http.ResponseWriter, r *http.Request) {
	r, leave := s.enterSearch(r, r.URL.Query().Get("query"))
	defer leave()
	ctx := r.Context()
	pr := FromContext(ctx)
	q := r.URL.Query()
	query := q.Get("query")
	limit, valid := searchLimit(w, r, 50, 200)
	if !valid {
		return
	}
	sf := parseLibrarySearchTypes(q)
	// People and Live TV are searched beside titles when the request wants them.
	var peopleDone chan peopleSearchResult
	if sf.wantPeople {
		peopleDone = make(chan peopleSearchResult, 1)
		peopleCtx := ctx // ctx is reassigned below
		go func() {
			catPeople := s.searchPeople(peopleCtx, pr, query, 0, limit, false)
			plugPeople := s.searchPluginPeople(peopleCtx, pr, query, 0, limit, false)
			peopleDone <- mergePeopleSearchResults(catPeople, plugPeople, limit)
		}()
	}
	var liveDone chan liveLibrarySearchResult
	if sf.wantLiveTV {
		liveDone = make(chan liveLibrarySearchResult, 1)
		liveReq := r.WithContext(ctx)
		go func() {
			liveDone <- s.searchLiveTVLibrary(liveReq, pr, query, limit)
		}()
	}
	baseTypes := s.searchTypes(pr.User, 0)
	queryTypes := baseTypes
	if sf.filtered {
		queryTypes = nil
		hasShow := false
		for _, mt := range baseTypes {
			if mt == frontend.TypeShow {
				hasShow = true
			}
			if sf.allow[mt] {
				queryTypes = append(queryTypes, mt)
			}
		}
		if hasShow {
			if sf.wantSeasons && !slices.Contains(queryTypes, frontend.TypeSeason) {
				queryTypes = append(queryTypes, frontend.TypeSeason)
			}
			if sf.wantEpisodes && !slices.Contains(queryTypes, frontend.TypeEpisode) {
				queryTypes = append(queryTypes, frontend.TypeEpisode)
			}
		}
	}
	fetch := s.saferFetchCount(pr.User, limit)
	var pluginItems []frontend.Title
	if !sf.filtered || len(sf.allow) > 0 {
		pluginItems = s.searchPluginTitles(ctx, pr, query, 0, limit)
	}
	ctx = frontend.WithSearchClient(ctx, s.searchClientID(pr))
	// /library/search renders titles as list items (no trailers, keywords,
	// or full cast). Request visibility-only candidate rows here and hydrate
	// list facts only for the <= limit titles that survive final ranking.
	searchCtx, listDetails := s.hubSearchCandidates(frontend.WithSearchListFacts(ctx), pr.User)
	var result frontend.SearchResults
	var err error
	if len(queryTypes) > 0 {
		result, err = s.searchExperience().Resolve(searchCtx, pr.User, query, queryTypes, fetch, true)
	} else if sf.wantCollections && len(baseTypes) > 0 {
		result, err = s.searchExperience().Resolve(searchCtx, pr.User, query, baseTypes, fetch, true)
	}
	var people peopleSearchResult
	if peopleDone != nil {
		people = <-peopleDone
	}
	var live liveLibrarySearchResult
	if liveDone != nil {
		live = <-liveDone
	}
	if err != nil {
		s.writeSearchErr(w, r, err, &model.MediaContainer{})
		return
	}
	vibe, res, isVibe := result.Collection, mergePluginSearchResults(result.Items, pluginItems, result.AIItems, fetch), result.CollectionSearch
	aiKeys := map[frontend.RatingKey]bool{}
	for i := 0; i < min(result.AIItems, len(res)); i++ {
		aiKeys[res[i].Key] = true
	}
	if sf.filtered {
		if len(sf.allow) == 0 {
			res = nil
		} else {
			filtered := res[:0:0]
			for _, t := range res {
				if sf.allow[t.Type] || sf.allow[frontend.MediaType(s.plexTypeFor(&t))] {
					filtered = append(filtered, t)
				}
			}
			res = filtered
		}
	}
	if !reqmemo.Active(ctx) {
		ctx = reqmemo.With(ctx)
	}
	storedCtx := withStoredPolicy(ctx)
	vis := s.visibilityCtx(storedCtx)
	filterOpenable := func(ts []frontend.Title) []frontend.Title {
		if len(ts) == 0 {
			return nil
		}
		rowCtx := s.withSeriesRows(storedCtx, pr.User, ts)
		return s.openableTitlesWith(rowCtx, vis, pr.User, ts, false)
	}
	type librarySearchEntry struct {
		sr       model.SearchResult
		raw      float64
		titleKey frontend.RatingKey
	}
	var leading []librarySearchEntry
	var leadingTitles []frontend.Title
	if result.AIItems > 0 {
		var aiTitles, kwTitles []frontend.Title
		for _, t := range res {
			if aiKeys[t.Key] {
				aiTitles = append(aiTitles, t)
			} else {
				kwTitles = append(kwTitles, t)
			}
		}
		res = kwTitles
		aiTitles = filterOpenable(aiTitles)
		if len(aiTitles) > limit {
			aiTitles = aiTitles[:limit]
		}
		leadingTitles = aiTitles
		for i := range aiTitles {
			sc := max(0.96, math.Round((1-float64(i)*0.0001)*10000)/10000)
			leading = append(leading, librarySearchEntry{
				sr:       model.SearchResult{Score: sc},
				raw:      sc,
				titleKey: aiTitles[i].Key,
			})
		}
		if isVibe && vibe != nil && sf.wantCollections {
			d := s.collectionDirectory(ctx, vibe)
			sc := 0.96
			if len(leading) == 0 {
				sc = 1
			}
			leading = append(leading, librarySearchEntry{
				sr:  model.SearchResult{Score: sc, Directory: &d},
				raw: sc,
			})
		}
	}
	var peopleDirs []model.Directory
	var peopleScores []float64
	if sf.wantPeople {
		peopleDirs = append(peopleDirs, people.actors...)
		peopleDirs = append(peopleDirs, people.directors...)
		peopleScores = append(append(peopleScores, people.actorScores...), people.directorScores...)
	}
	scores := make(map[string]float64, len(res))
	for i := range res {
		scores[res[i].Key.String()] = titleSearchScore(&res[i], i, len(res)+len(peopleDirs))
	}
	res = filterOpenable(res)
	kwByKey := make(map[frontend.RatingKey]frontend.Title, len(res))
	totalRanked := len(res) + len(peopleDirs)
	entries := make([]librarySearchEntry, 0, totalRanked)
	for i := range res {
		kwByKey[res[i].Key] = res[i]
		score, ok := scores[res[i].Key.String()]
		if !ok {
			score = searchScore(i, totalRanked)
		}
		entries = append(entries, librarySearchEntry{
			sr:       model.SearchResult{Score: math.Round(reportedScore(score)*10000) / 10000},
			raw:      score,
			titleKey: res[i].Key,
		})
	}
	for i := range peopleDirs {
		d := peopleDirs[i]
		entries = append(entries, librarySearchEntry{
			sr:  model.SearchResult{Score: math.Round(peopleScores[i]*10000) / 10000, Directory: &d},
			raw: peopleScores[i],
		})
	}
	if sf.wantOwnedCollections {
		dirs, _, _ := s.searchCollections(ctx, pr, query, 0, limit)
		seenCol := map[string]bool{}
		if isVibe && vibe != nil {
			seenCol["/library/collections/"+vibe.Key.String()+"/children"] = true
		}
		for _, d := range dirs {
			if seenCol[d.Key] {
				continue
			}
			seenCol[d.Key] = true
			sc := playlistSearchScore(d.Title, query)
			if sc <= 0 {
				sc = 0.5
			}
			dir := d
			entries = append(entries, librarySearchEntry{
				sr:  model.SearchResult{Score: math.Round(sc*10000) / 10000, Directory: &dir},
				raw: sc,
			})
		}
	}
	if sf.wantPlaylists {
		directories := q.Get("contentDirectoryID")
		if directories == "" && sf.filtered && (sf.allow[frontend.TypeMovie] || sf.allow[frontend.TypeShow]) {
			var ids []string
			for _, sec := range s.userSections(pr.User) {
				if sec.Type == frontend.TypeMovie && sf.allow[frontend.TypeMovie] || sec.Type == frontend.TypeShow && sf.allow[frontend.TypeShow] {
					ids = append(ids, strconv.Itoa(sec.ID))
				}
			}
			directories = strings.Join(ids, ",")
		}
		for _, m := range s.searchPlaylists(ctx, pr, query, directories, limit) {
			md := m
			score, _ := strconv.ParseFloat(md.Score, 64)
			md.Score = ""
			entries = append(entries, librarySearchEntry{
				sr:  model.SearchResult{Score: score, Metadata: &md},
				raw: score,
			})
		}
	}
	for i := range live.results {
		entries = append(entries, librarySearchEntry{
			sr:  live.results[i],
			raw: live.scores[i],
		})
	}
	// PMS lists every result best match first.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].raw > entries[j].raw })
	for i := range entries {
		if len(leading) > 0 && entries[i].sr.Score > 0.95 {
			entries[i].sr.Score = 0.95
		} else if live.hasChannelMatches && entries[i].raw < liveChannelMatchRawFloor && entries[i].sr.Score > 0.98 {
			entries[i].sr.Score = 0.98
		}
	}
	var combined []librarySearchEntry
	if len(leading) > 0 {
		combined = append(leading, entries...)
	} else {
		combined = entries
		if isVibe && vibe != nil && sf.wantCollections {
			d := s.collectionDirectory(ctx, vibe)
			combined = append(combined, librarySearchEntry{
				sr:  model.SearchResult{Score: 1, Directory: &d},
				raw: 1,
			})
		}
	}
	// A flat search has one window across every result family.
	if len(combined) > limit {
		combined = combined[:limit]
	}
	aiByKey := make(map[frontend.RatingKey]frontend.Title, len(leadingTitles))
	for _, t := range leadingTitles {
		aiByKey[t.Key] = t
	}
	surviving := make([]frontend.Title, 0, len(combined))
	savedKeywords := make(map[frontend.RatingKey][]string)
	for _, e := range combined {
		if e.titleKey == 0 {
			continue
		}
		t, ok := aiByKey[e.titleKey]
		if !ok {
			t, ok = kwByKey[e.titleKey]
		}
		if !ok {
			continue
		}
		if len(t.Keywords) > 0 {
			savedKeywords[t.Key] = t.Keywords
		}
		surviving = append(surviving, t)
	}
	mdByKey := make(map[string]model.Metadata, len(surviving))
	if len(surviving) > 0 {
		if listDetails != nil {
			if surviving, err = listDetails(surviving); err != nil {
				s.writeSearchErr(w, r, err, &model.MediaContainer{})
				return
			}
			for i := range surviving {
				if len(surviving[i].Keywords) == 0 {
					if kw := savedKeywords[surviving[i].Key]; len(kw) > 0 {
						surviving[i].Keywords = kw
					}
				}
			}
		}
		rendered := s.renderTitles(ctx, pr, surviving, renderOpts{media: true, searchIdentity: true})
		for i := range rendered {
			// PMS names the section here but sends no section id or key.
			rendered[i].LibrarySectionID, rendered[i].LibrarySectionKey = 0, ""
			mdByKey[rendered[i].RatingKey] = rendered[i]
		}
	}
	mc := &model.MediaContainer{SearchResult: make([]model.SearchResult, 0, len(combined))}
	for _, e := range combined {
		if e.titleKey != 0 {
			md, ok := mdByKey[e.titleKey.String()]
			if !ok {
				continue
			}
			e.sr.Metadata = &md
		}
		mc.SearchResult = append(mc.SearchResult, e.sr)
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}
