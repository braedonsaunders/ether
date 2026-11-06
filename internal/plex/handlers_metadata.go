package plex

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
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
	return s.visibility().TitleAllowed(u, t) && s.plexMetaRatingAllowed(u, t)
}

// metadataKeys parses a detail key. Numeric rating keys stay as they are.
// A plex:// guid (More Ways to Watch keeps the discover identity and asks
// this server for it) resolves through the local mapping. A variant part id
// resolves to the title that owns that part, so a re-read of the part the
// client just selected still shows the subtitle.
func (s *Server) metadataKeys(ctx context.Context, raw string) ([]core.RatingKey, error) {
	raw = strings.TrimSpace(raw)
	if decoded, err := url.PathUnescape(raw); err == nil && decoded != "" {
		raw = decoded
	}
	if strings.Contains(raw, "://") {
		if s.d.PlexGUID == nil {
			return nil, errors.New("no keys")
		}
		k, err := s.d.PlexGUID.ExactKey(ctx, raw)
		if err != nil {
			return nil, err
		}
		return []core.RatingKey{k}, nil
	}
	keys, err := parseKeys(raw)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		if mapped, rel, ok := s.partTitle(int64(k)); ok && rel != "" {
			keys[i] = mapped
		}
	}
	return keys, nil
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	if plexAndroidClient(pr) {
		pr.detailWaitUntil = time.Now().Add(detailWarmWaitBudget)
	}
	q := r.URL.Query()
	// Validate before any catalogue lookup or playback warm-up: these flags
	// have a boolean integer grammar even when their work is asynchronous.
	for _, name := range []string{"checkFiles", "skipRefresh", "asyncCheckFiles", "checkFileAvailability", "asyncAugmentMetadata", "asyncRefreshLocalMediaAgent", "asyncRefreshAnalysis"} {
		if values, present := q[name]; present {
			n, err := strconv.Atoi(values[0])
			if err != nil || (n != 0 && n != 1) {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
		}
	}
	keys, err := s.metadataKeys(ctx, chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
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
			md = s.collectionMetadata(c, ctx)
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
			md = s.playlistListingMetadata(ctx, pl)
			if pl.SmartURI != "" {
				md.Content = "library://x/directory/" + url.QueryEscape(pl.SmartURI)
			}
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
			if err == nil && !s.visibleTitle(ctx, pr.User, t) {
				err = core.ErrNotFound
			}
			// Policy/storage reads can fail closed on cancellation. Do not turn
			// that interrupted lookup into a claim that the title disappeared.
			if ctx.Err() != nil {
				return
			}
			if err != nil && len(keys) == 1 && q.Get("collection") == "" {
				if future, e := s.scheduledEpisode(ctx, pr, k); e == nil {
					md = s.scheduledMetadata(ctx, pr, future)
					mc = s.itemContainer(future.Type)
					mc.Metadata = append(mc.Metadata, md)
					continue
				}
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
				// Version discovery belongs to the open detail page, not to
				// admission of the independent playback warm-up. Start it first
				// so a warm-up that needs a search can join the same work.
				if pr.User != nil && s.detailSearchAllowed(pr) {
					if refiller, ok := s.d.Variants.(core.SearchCacheRefiller); ok {
						// Repeated metadata fetches while the same page is being
						// watched must not retry a failed search on every fetch.
						watchKey := fmt.Sprintf("%d|%s|%d", pr.User.ID, pr.ClientID, t.Key)
						s.variantWatch.mu.Lock()
						watching := s.variantWatch.pending[watchKey]
						s.variantWatch.mu.Unlock()
						if !watching {
							pr.detailRefillRequested = true
							pr.detailRefill = refiller.RefillSearchCache(t)
						}
					}
				}
				s.warmDetail(ctx, pr, t)
			}
			if len(keys) == 1 {
				// Live session intent (#32): a single-item detail open.
				s.recordDetailIntent(ctx, pr, t)
			}
			// renderTitles uses verified cached GUIDs and queues missing identities.
			// Remote identity enrichment must not hold detail or Play requests.

			if len(keys) == 1 {
				pr.versionItemKey = t.Key
			}
			if len(keys) == 1 && (metadataOptionOn(q.Get("checkFiles")) || metadataOptionOn(q.Get("asyncCheckFiles")) || metadataOptionOn(q.Get("checkFileAvailability"))) {
				pr.versionFlowKey = t.Key
			}
			md = s.detail(ctx, pr, t, metadataOptionOn(q.Get("includeChildren")), metadataOptionOn(q.Get("includeOnDeck")), metadataOptionOn(q.Get("includeRelated")), metadataOptionOn(q.Get("includeExtras")), q.Get("extraTypes"))
			if q.Get("includePreferences") == "1" {
				md.Preferences = s.itemTrackPreferences(ctx, pr, t)
			}
			if raw, present := q["includeRelatedCount"]; present && md.Related != nil {
				if count, err := strconv.Atoi(raw[0]); err == nil && count >= 0 {
					for i := range md.Related.Hub {
						h := &md.Related.Hub[i]
						if len(h.Metadata) > count {
							h.Metadata, h.More = h.Metadata[:count], true
						}
						if len(h.Directory) > count {
							h.Directory, h.More = h.Directory[:count], true
						}
						h.Size = len(h.Metadata) + len(h.Directory)
					}
				}
			}
			if mc == nil {
				mc = s.itemContainer(t.Type)
			}
		}
		mc.Metadata = append(mc.Metadata, md)
	}
	if mc == nil {
		mc = libraryContainer()
	}
	s.projectFileAvailability(r, mc)
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

func metadataOptionOn(raw string) bool {
	n, err := strconv.Atoi(raw)
	return err == nil && n == 1
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
	if children && t.Type == core.TypeShow {
		if kids, err := s.metadataChildren(ctx, pr, t.Key); err == nil {
			rendered := s.renderTitles(ctx, pr, kids, renderOpts{media: true, inSection: true, skipLeafCounts: t.Type == core.TypeShow})
			if t.Type == core.TypeShow {
				s.applyLeafCounts(ctx, pr, rendered, kids, memo)
			}
			md.Children = &model.Children{Size: len(rendered), Metadata: rendered}
		}
	}
	if onDeck && (t.Type == core.TypeShow || t.Type == core.TypeSeason) {
		show := t.Key
		if t.Type == core.TypeSeason {
			show = core.ShowKey(t.Key.TMDB())
		}
		if ep, st := s.showOnDeck(ctx, pr, show, memo); ep != nil && (t.Type != core.TypeSeason || ep.Season == t.Season) {
			// The show page's Play button plays this episode: resolve,
			// open and probe it so OnDeck and the following Play have its tracks.
			if noWarm, _ := ctx.Value(structuralProjectionKey{}).(bool); !noWarm {
				s.warmDetail(ctx, pr, ep)
			}
			if items := s.renderTitles(ctx, pr, []core.Title{*ep}, renderOpts{media: true, state: st}); len(items) > 0 {
				if extras {
					items[0].Extras = &model.Extras{}
					for _, e := range s.extrasFor(ctx, pr, ep, extraTypes) {
						items[0].Extras.Metadata = append(items[0].Extras.Metadata, s.extraMetadata(e, ep, true))
					}
					items[0].Extras.Size = len(items[0].Extras.Metadata)
				}
				md.OnDeck = &model.OnDeck{Metadata: items[0]}
			}
		}
	}
	if related && s.d.Recommender != nil {
		if hubs, err := s.detailRelated(ctx, pr).Rows(ctx, pr.User, t.Key, 12); err == nil {
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
	var source core.AvailabilitySource
	source, _ = s.d.Variants.(core.AvailabilitySource)
	return experience.SameDayUnavailable(ctx, t, s.now(), s.d.FilterAvailable, source)
}

// showOnDeck picks the episode to resume or start next.
func (s *Server) showOnDeck(ctx context.Context, pr *Request, show core.RatingKey, memo showEpisodes) (*core.Title, map[core.RatingKey]core.WatchStatus) {
	eps, err := s.episodesOfShow(ctx, pr, memo, show)
	if err != nil || len(eps) == 0 {
		return nil, nil
	}
	st := s.stateFor(ctx, pr, eps)
	return experience.ShowOnDeck(eps, st, func(t *core.Title) bool { return s.sameDayUnavailable(ctx, pr, t) }), st
}

const detailWarmWaitBudget = 4 * time.Second

// warmDetail schedules a detail-page warm-up for t and waits up to a bounded
// budget for its probe when no session, resumed probe or warm probe is ready
// yet, so the first detail/season/show open renders the file's real audio and
// subtitle tracks instead of an estimate.
func (s *Server) warmDetail(ctx context.Context, pr *Request, t *core.Title) {
	if t == nil || !isPlayable(t.Type) || s.d.Playback == nil {
		return
	}
	if plexAndroidClient(pr) {
		if _, ok := s.d.Playback.(core.MetadataProber); ok {
			// A single wait: do not add speculative warmDetail's four seconds
			// after the metadata probe's own bounded wait. Errors/timeouts
			// preserve the normal detail response and background preparation.
			_ = s.probeDetailTracks(ctx, pr, t)
			return
		}
	}
	caps := s.clientCaps(pr)
	if warm, ok := s.d.Playback.(core.Prewarmer); ok {
		warm.Prewarm(ctx, pr.User, t.Key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail})
	}
	if pr == nil || pr.User == nil {
		return
	}
	if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, t.Key); ok && ps != nil {
		return
	}
	if _, _, ok := s.resumedTracks(ctx, pr, t); ok {
		return
	}
	if wp, ok := s.d.Playback.(core.WarmProber); ok {
		if _, warm := wp.WarmProbeInfo(pr.User, caps, t.Key); warm {
			return
		}
	}
	if waiter, ok := s.d.Playback.(core.WarmProbeWaiter); ok {
		waiter.WaitWarmProbe(ctx, pr.User, caps, t.Key, detailWarmWaitBudget)
	}
}

// seasonNextEpisode picks the episode a viewer opening a season page is most
// likely to play first: an in-progress episode, else the episode after the
// last watched one, else the first aired episode.
func (s *Server) seasonNextEpisode(ctx context.Context, pr *Request, eps []core.Title) *core.Title {
	if len(eps) == 0 {
		return nil
	}
	now := s.now()
	st := s.stateFor(ctx, pr, eps)
	lastWatched := -1
	for i := range eps {
		if eps[i].Type != core.TypeEpisode || (!eps[i].Released.IsZero() && eps[i].Released.After(now)) {
			continue
		}
		ws := st[eps[i].Key]
		if ws.ViewOffset > 0 && ws.ViewCount == 0 {
			return &eps[i]
		}
		if ws.ViewCount > 0 {
			lastWatched = i
		}
	}
	for i := lastWatched + 1; i < len(eps); i++ {
		if eps[i].Type != core.TypeEpisode || (!eps[i].Released.IsZero() && eps[i].Released.After(now)) {
			continue
		}
		if st[eps[i].Key].ViewCount == 0 && !s.sameDayUnavailable(ctx, pr, &eps[i]) {
			return &eps[i]
		}
	}
	for i := range eps {
		if eps[i].Type == core.TypeEpisode && (eps[i].Released.IsZero() || !eps[i].Released.After(now)) && !s.sameDayUnavailable(ctx, pr, &eps[i]) {
			return &eps[i]
		}
	}
	return nil
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
	if parent.Type == core.TypeMovie || parent.Type == core.TypeEpisode {
		writeHTTPError(w, http.StatusBadRequest)
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
	if parent.Type == core.TypeShow && r.URL.Query().Has("unwatched") {
		if r.URL.Query().Get("unwatched") != "1" {
			kids = nil
		} else {
			eps, _ := s.episodesOfShow(ctx, pr, nil, k)
			states := s.stateFor(ctx, pr, eps)
			hasUnplayed := map[int]bool{}
			for _, ep := range eps {
				if states[ep.Key].ViewCount == 0 {
					hasUnplayed[ep.Season] = true
				}
			}
			filtered := kids[:0]
			for _, season := range kids {
				if hasUnplayed[season.Season] {
					filtered = append(filtered, season)
				}
			}
			kids = filtered
		}
	}

	childQ := r.URL.Query()
	if parent.Type == core.TypeShow {
		for name := range childQ {
			if strings.TrimRight(name, "!<>=") == "unwatched" {
				childQ.Del(name)
			}
		}
	}
	kids, err = s.childQuery(ctx, pr, kids, childQ)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	future := s.scheduledEpisodes(ctx, pr, k)
	kids = appendScheduled(kids, future)
	lo, hi := pr.window(len(kids))
	page := kids[lo:hi]
	if parent.Type == core.TypeSeason {
		if ep := s.seasonNextEpisode(ctx, pr, page); ep != nil {
			s.warmDetail(ctx, pr, ep)
		}
	}
	if len(future) == 0 {
		mc.Metadata = s.renderTitles(ctx, pr, page, renderOpts{media: true, inSection: true, skipLeafCounts: parent.Type == core.TypeShow})
	} else {
		// Batch ordinary rendering without ever passing placeholders to media
		// generation, then restore the canonical episode order.
		var aired []core.Title
		for _, child := range page {
			if !child.Released.After(s.now()) {
				aired = append(aired, child)
			}
		}
		rendered := s.renderTitles(ctx, pr, aired, renderOpts{media: true, inSection: true, skipLeafCounts: parent.Type == core.TypeShow})
		byKey := make(map[string]model.Metadata, len(rendered))
		for _, md := range rendered {
			byKey[md.RatingKey] = md
		}
		for _, child := range page {
			if child.Released.After(s.now()) {
				mc.Metadata = append(mc.Metadata, s.scheduledMetadata(ctx, pr, &child))
			} else if md, ok := byKey[child.Key.String()]; ok {
				mc.Metadata = append(mc.Metadata, md)
			}
		}
	}
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
	if parent.Type == core.TypeMovie {
		mc.ViewGroup = "movie"
	}
	mc.ViewMode = viewModeList
	mc.Mixed = model.Ptr(true)
	// Filter before paging, as handleChildren does, so the window and
	// totalSize count only episodes the viewer may see.
	eps = s.visibleTitles(pr.User, eps)
	if r.URL.Query().Has("unwatched") {
		n, err := strconv.ParseInt(r.URL.Query().Get("unwatched"), 10, 32)
		if err != nil || n < 0 || n > 1 || strings.HasPrefix(r.URL.Query().Get("unwatched"), "-") {
			writeHTTPError(w, 400)
			return
		}
		if n == 1 {
			states := s.stateFor(ctx, pr, eps)
			filtered := eps[:0]
			for _, ep := range eps {
				if states[ep.Key].ViewCount == 0 {
					filtered = append(filtered, ep)
				}
			}
			eps = filtered
		}
	}
	leafQ := r.URL.Query()
	leafQ.Del("unwatched")
	eps, err = s.childQuery(ctx, pr, eps, leafQ)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
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
	items, err := s.recommendationCandidates(ctx, pr.User, limit, func(ctx context.Context, fetch int) ([]core.Title, error) {
		return s.d.Catalog.Similar(ctx, k, fetch)
	})
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
			s.warmDetail(ctx, pr, ep)
			mc.Metadata = s.renderTitles(ctx, pr, []core.Title{*ep}, renderOpts{media: true, state: st})
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// ---------------------------------------------------------------------------
// Collections

func (s *Server) collection(ctx context.Context, u *core.User, k core.RatingKey) (*core.Collection, error) {
	return s.collectionSelection().Get(ctx, u, k, collectionDevice(ctx), true)
}

func (s *Server) collectionRaw(ctx context.Context, u *core.User, k core.RatingKey) (*core.Collection, error) {
	return s.collectionSelection().Get(ctx, u, k, "", false)
}

func collectionDevice(ctx context.Context) string {
	pr := FromContext(ctx)
	return core.ClassifyDevice(pr.Product, pr.Platform, pr.Device)
}

func (s *Server) collectionSelection() experience.ScopedCollection {
	return experience.ScopedCollection{Collections: s.d.Collections, Visibility: s.visibility(), Now: s.now, Unrestricted: func(viewer *core.User) bool { return !s.viewerRestricted(viewer) }}
}

func (s *Server) lookupPlaceholder(ctx context.Context, pr *Request, key core.RatingKey, raw string) (*core.Title, core.RatingKey, error) {
	var requested *core.RatingKey
	if raw != "" {
		collection, err := core.ParseRatingKey(raw)
		if err != nil {
			return nil, 0, core.ErrNotFound
		}
		requested = &collection
	}
	return s.collectionSelection().Placeholder(ctx, pr.User, key, requested, core.ClassifyDevice(pr.Product, pr.Platform, pr.Device))
}

func (s *Server) scopedCollectionCounts(ctx context.Context, u *core.User, cs []core.Collection) []core.Collection {
	return s.collectionSelection().Counts(ctx, u, cs, collectionDevice(ctx))
}

func (s *Server) collectionMetadata(c *core.Collection, contexts ...context.Context) model.Metadata {
	rk := c.Key.String()
	m := model.Metadata{
		RatingKey: rk,
		Key:       "/library/collections/" + rk + "/children",
		GUID:      "collection://" + rk,
		Type:      "collection",
		Title:     c.Title,
		Subtype:   string(c.Type),
		Summary:   c.Summary,
		Index:     model.Ptr(0),
		Thumb:     "/library/metadata/" + rk + "/thumb/" + collectionStamp(c),
		AddedAt:   s.now().Unix(),
		UpdatedAt: s.now().Unix(),
	}
	// PMS omits childCount for an empty collection on detail and listing
	// responses, while retaining the collection itself.
	if c.ChildCount > 0 {
		m.ChildCount = model.Ptr(c.ChildCount)
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
	if len(contexts) > 0 {
		s.applyListWatch(contexts[0], &m, c.Key)
	}
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
	all = filterCollectionTitles(all, r.URL.Query())
	lo, hi := pr.window(len(all))
	for i := lo; i < hi; i++ {
		mc.Metadata = append(mc.Metadata, s.collectionMetadata(&all[i], ctx))
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
				mc.Metadata = append(mc.Metadata, s.collectionMetadata(&cs[i], ctx))
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
	mc.Metadata = []model.Metadata{s.collectionMetadata(c, ctx)}
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
	recovering := shelfToken(r.URL.RawQuery) != ""
	buildOffset, buildSize := offset, size
	if recovering {
		buildOffset, buildSize = 0, int(^uint(0)>>1)
	}
	page, err := s.collectionSelection().Page(ctx, pr.User, c, k, collectionDevice(ctx), buildOffset, buildSize)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	mc := s.collectionItemsContainer(c, k)
	if strings.HasPrefix(r.URL.Path, "/library/metadata/") {
		mc.SetSyncProjection(true)
	}
	if !c.Upcoming {
		mc.Metadata = s.renderTitles(ctx, pr, s.filterAllowed(pr.User, page.Items), renderOpts{media: true})
	} else {
		for i := range page.Items {
			t := &page.Items[i]
			if t.IsFutureRelease(s.now()) {
				mc.Metadata = append(mc.Metadata, s.placeholderMetadata(ctx, pr, t, k))
			} else {
				mc.Metadata = append(mc.Metadata, s.renderTitles(ctx, pr, []core.Title{*t}, renderOpts{media: true})...)
			}
		}
	}
	if recovering {
		seen := map[string]bool{}
		unique := mc.Metadata[:0]
		for _, item := range mc.Metadata {
			if !seen[item.RatingKey] {
				seen[item.RatingKey] = true
				unique = append(unique, item)
			}
		}
		page.Total = len(unique)
		offset = min(offset, page.Total)
		end := offset + min(size, page.Total-offset)
		mc.Metadata = unique[offset:end]
	}
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, page.Total)
	s.write(w, r, mc)
}

func (s *Server) collectionItemsContainer(c *core.Collection, k core.RatingKey) *model.MediaContainer {
	mc := s.itemContainer(c.Type)
	mc.Key = k.String()
	mc.Title1 = c.Title
	mc.Title2 = c.Title
	mc.Summary = c.Summary
	mc.Thumb = "/library/metadata/" + k.String() + "/thumb/" + collectionStamp(c)
	mc.ViewGroup = string(c.Type)
	mc.ViewMode = viewModeList
	return mc
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

// Prefer exact facts for the TV picker, without making a slow source fail
// the detail page. Completed background probes are visible on later reads.
func (s *Server) probeDetailTracks(ctx context.Context, pr *Request, t *core.Title) error {
	if pr == nil || pr.User == nil || s.d.Playback == nil {
		return nil
	}
	if _, _, exact := s.mediaInfoWithSource(ctx, pr, t); exact {
		return nil
	}
	if prober, ok := s.d.Playback.(core.MetadataProber); ok {
		if !pr.detailWaitUntil.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, pr.detailWaitUntil)
			defer cancel()
		}
		return prober.ProbeMetadata(ctx, pr.User, t.Key, s.clientCaps(pr))
	}
	return nil
}

// Disabled warming on Plex Android permits cached metadata reads only: neither
// the required probe nor version discovery may start a fresh indexer search.
// Other clients retain their existing discovery policy.
func (s *Server) detailSearchAllowed(pr *Request) bool {
	if !plexAndroidClient(pr) {
		return true
	}
	c := s.cfg().Cache
	return !c.DisableSpeculation && c.Mode != "off" && c.DetailWarmup != "off"
}
