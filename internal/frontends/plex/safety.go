package plex

import (
	"context"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// AccessPolicy adds owner-defined restrictions to the mandatory title guard.
type AccessPolicy = frontend.AccessPolicy

func (s *Server) saferFetchCount(u *frontend.User, want int) int {
	if want <= 0 || !s.needPlexRatingFilter(u) {
		return want
	}
	n := want * 2
	if extra := want + 24; n > extra {
		n = extra
	}
	if n > 200 {
		n = 200
	}
	return n
}

func (s *Server) takeVisible(u *frontend.User, ts []frontend.Title, want int) []frontend.Title {
	out := s.visibleTitles(u, ts)
	if want > 0 && len(out) > want {
		return out[:want]
	}
	return out
}

func (s *Server) catalogBrowse(ctx context.Context, q frontend.BrowseQuery) (frontend.Page, error) {
	if s.plexMetaOn() {
		q.Filter.SaferPlexRating = true
	}
	return s.catalog().Browse(ctx, q)
}

func (s *Server) visibleTitles(u *frontend.User, ts []frontend.Title) []frontend.Title {
	return s.visibleTitlesWith(s.visibility(), u, ts)
}

func (s *Server) visibleTitlesWith(vis frontend.Visibility, u *frontend.User, ts []frontend.Title) []frontend.Title {
	hasPlugin := false
	for i := range ts {
		if ts[i].Key.IsPluginKey() {
			hasPlugin = true
			break
		}
	}
	if hasPlugin {
		catalog := make([]frontend.Title, 0, len(ts))
		for i := range ts {
			if !ts[i].Key.IsPluginKey() {
				catalog = append(catalog, ts[i])
			}
		}
		allowedCat := s.visibleTitlesWith(vis, u, catalog)
		catOK := make(map[frontend.RatingKey]bool, len(allowedCat))
		for i := range allowedCat {
			catOK[allowedCat[i].Key] = true
		}
		out := make([]frontend.Title, 0, len(ts))
		for i := range ts {
			if ts[i].Key.IsPluginKey() {
				if s.canSeePluginTitle(u, &ts[i]) {
					out = append(out, ts[i])
				}
			} else if catOK[ts[i].Key] {
				out = append(out, ts[i])
			}
		}
		return out
	}
	out := vis.TitlesAllowed(u, ts)
	metaSrc := s.libraryMetadata()
	if !s.needPlexRatingFilter(u) || metaSrc == nil {
		return out
	}
	keys := make([]frontend.RatingKey, 0, len(out))
	for i := range out {
		keys = append(keys, out[i].Key)
	}
	rows := metaSrc.CachedMany(context.Background(), keys)
	var guids map[frontend.RatingKey]string
	if guidSrc := s.libraryGUIDs(); guidSrc != nil {
		guids = guidSrc.CachedMany(context.Background(), keys)
	}
	kept := out[:0]
	for i := range out {
		if s.plexMetaRatingAllowedCached(u, &out[i], rows, guids) {
			kept = append(kept, out[i])
		}
	}
	return kept
}

func (s *Server) titleAllowed(u *frontend.User, t *frontend.Title) bool {
	return frontend.TitleAllowed(s.policy(), u, t) && s.plexMetaRatingAllowed(u, t)
}

func titleInSectionType(titleType, sectionType frontend.MediaType) bool {
	switch sectionType {
	case frontend.TypeMovie:
		return titleType == frontend.TypeMovie
	case frontend.TypeShow:
		switch titleType {
		case frontend.TypeShow, frontend.TypeSeason, frontend.TypeEpisode:
			return true
		}
		return false
	default:
		return sectionType != "" && titleType == sectionType
	}
}

func (s *Server) visibilityWithSectionFor(sectionFor func(frontend.MediaType) (int, bool)) frontend.Visibility {
	v := frontend.Visibility{Policy: s.policy(), FilterAvailable: s.filterAvailable(), JudgedOnFacts: s.judgedOnFacts(),
		SectionFor: sectionFor}
	if s.catalog() != nil {
		v.GetTitle = s.seriesTitle
	}
	if s.playback() != nil {
		v.Sessions = s.playback().Sessions
	}
	return v
}

func (s *Server) visibility() frontend.Visibility {
	return s.visibilityCtx(context.Background())
}

func (s *Server) visibilityCtx(ctx context.Context) frontend.Visibility {
	// Resolve the published sections once per Visibility (and per request
	// via reqmemo when ctx carries one): title checks run for every row of a
	// hub or batch, and each section listing reads published libraries again.
	var once sync.Once
	var movie, show Section
	var movieOK, showOK bool
	sectionFor := func(t frontend.MediaType) (int, bool) {
		once.Do(func() {
			secs := s.getSectionsCtx(ctx)
			for _, sec := range secs {
				if isKindSection(sec) {
					continue
				}
				if !movieOK && sec.Type == frontend.TypeMovie {
					movie, movieOK = sec, true
				}
				if !showOK && sec.Type == frontend.TypeShow {
					show, showOK = sec, true
				}
			}
			for _, sec := range secs {
				if !movieOK && sec.Type == frontend.TypeMovie {
					movie, movieOK = sec, true
				}
				if !showOK && sec.Type == frontend.TypeShow {
					show, showOK = sec, true
				}
			}
		})
		switch t {
		case frontend.TypeShow, frontend.TypeSeason, frontend.TypeEpisode:
			return show.ID, showOK
		}
		return movie.ID, movieOK
	}
	return s.visibilityWithSectionFor(sectionFor)
}

// sectionVisibility scopes visibility to one section's media type and library
// authorization: titles of any other media type are rejected, and library
// access is checked against sec.ID rather than the canonical section for that
// media type.
func (s *Server) sectionVisibility(sec Section) frontend.Visibility {
	if sec.ID == 0 {
		return s.visibility()
	}
	return s.visibilityWithSectionFor(func(t frontend.MediaType) (int, bool) {
		if titleInSectionType(t, sec.Type) {
			return sec.ID, true
		}
		return 0, false
	})
}

// openableTitles is the common row/detail admission decision. A pinned shelf
// retains its availability cut, but never bypasses current authorization.
func (s *Server) openableTitles(ctx context.Context, u *frontend.User, ts []frontend.Title, pinned bool) []frontend.Title {
	return s.openableTitlesWith(ctx, s.visibility(), u, ts, pinned)
}

func (s *Server) openableTitlesWith(ctx context.Context, vis frontend.Visibility, u *frontend.User, ts []frontend.Title, pinned bool) []frontend.Title {
	// Plugin items are authorized by the Experience when projected; the
	// catalogue openability and built-in visibility checks do not know them,
	// so keep them without those filters (spec §6.4).
	orig := ts
	var kind, catalog []frontend.Title
	for i := range ts {
		if ts[i].Key.IsPluginKey() {
			kind = append(kind, ts[i])
		} else {
			catalog = append(catalog, ts[i])
		}
	}
	if len(kind) > 0 && len(catalog) == 0 {
		out := kind[:0]
		for i := range kind {
			if s.canSeePluginTitle(u, &kind[i]) {
				out = append(out, kind[i])
			}
		}
		return out
	}
	ts = catalog
	// visibleTitlesWith also applies the Plex metadata rating, in one batch.
	ts = s.visibleTitlesWith(vis, u, ts)
	ctx = s.withSeriesRows(ctx, u, ts)
	if !pinned {
		ts = vis.Available(ctx, u, ts)
	}
	if len(kind) == 0 {
		out := make([]frontend.Title, 0, len(ts))
		for _, t := range ts {
			_, admitted := shelfMember(ctx, t.Key)
			if (admitted || s.admittedTitle(&t)) && vis.Playable(ctx, u, &t) {
				out = append(out, t)
			}
		}
		return out
	}
	catOK := make(map[frontend.RatingKey]bool, len(ts))
	for _, t := range ts {
		_, admitted := shelfMember(ctx, t.Key)
		if (admitted || s.admittedTitle(&t)) && vis.Playable(ctx, u, &t) {
			catOK[t.Key] = true
		}
	}
	out := make([]frontend.Title, 0, len(orig))
	for i := range orig {
		if orig[i].Key.IsPluginKey() {
			if s.canSeePluginTitle(u, &orig[i]) {
				out = append(out, orig[i])
			}
		} else if catOK[orig[i].Key] {
			out = append(out, orig[i])
		}
	}
	return out
}

func (s *Server) visibleTitle(ctx context.Context, u *frontend.User, t *frontend.Title) bool {
	return s.visibleTitleWith(ctx, s.visibilityCtx(ctx), u, t)
}

// visibleTitleWith is visibleTitle through vis (s.visibility() with a
// caller's own show lookup).
func (s *Server) visibleTitleWith(ctx context.Context, vis frontend.Visibility, u *frontend.User, t *frontend.Title) bool {
	if t == nil {
		return false
	}
	if !s.admittedTitle(t) {
		return false
	}
	if _, pinned := shelfMember(ctx, t.Key); pinned {
		return s.titleAllowed(u, t)
	}
	return vis.Visible(ctx, u, t) && s.plexMetaRatingAllowed(u, t)
}

// admittedTitle shares a cheap predicate over facts the source already loaded.
func (s *Server) admittedTitle(t *frontend.Title) bool {
	if t == nil || t.IsFutureRelease(s.now()) {
		return false
	}
	if catalog, ok := s.catalog().(frontend.TitleOpenabilityCatalog); ok {
		return catalog.TitleOpenable(t)
	}
	return !t.Adult
}

func (s *Server) playableTitle(ctx context.Context, u *frontend.User, t *frontend.Title) bool {
	if t != nil && (t.MediaClass == frontend.MediaClassBook || t.MBItemType == "Book") {
		return false
	}
	return s.visibilityCtx(ctx).Playable(ctx, u, t) && s.plexMetaRatingAllowed(u, t)
}

// pluginAwarePlayable is playableTitle for plugin items. A plugin item's
// canonical library is its own; the visibility policy's SectionFor maps a
// projected MediaType onto a built-in section, which would wrongly test a
// music/photo item against Movies or TV Shows. Plugin items are authorized by
// the Experience when the metadata lookup succeeds (it checks the viewer
// against the owning library), so no built-in section check applies.
func (s *Server) pluginAwarePlayable(ctx context.Context, u *frontend.User, t *frontend.Title) bool {
	if t != nil && (t.MediaClass == frontend.MediaClassBook || t.MBItemType == "Book") {
		return false
	}
	if t != nil && t.Key.IsPluginKey() {
		return s.plexMetaRatingAllowed(u, t)
	}
	return s.playableTitle(ctx, u, t)
}
func (s *Server) availableForViewer(ctx context.Context, u *frontend.User, ts []frontend.Title) []frontend.Title {
	hasPlugin := false
	for i := range ts {
		if ts[i].Key.IsPluginKey() {
			hasPlugin = true
			break
		}
	}
	if !hasPlugin {
		return s.visibility().Available(ctx, u, ts)
	}
	catalog := make([]frontend.Title, 0, len(ts))
	for i := range ts {
		if !ts[i].Key.IsPluginKey() {
			catalog = append(catalog, ts[i])
		}
	}
	avail := s.visibility().Available(ctx, u, catalog)
	catOK := make(map[frontend.RatingKey]bool, len(avail))
	for i := range avail {
		catOK[avail[i].Key] = true
	}
	out := make([]frontend.Title, 0, len(ts))
	for i := range ts {
		if ts[i].Key.IsPluginKey() || catOK[ts[i].Key] {
			out = append(out, ts[i])
		}
	}
	return out
}

func (s *Server) viewerRestricted(u *frontend.User) bool {
	return frontend.ViewerRestricted(s.policy(), u)
}

type seriesRowsKey struct{}

// withSeriesRows reads the series of a list's episodes and seasons in one
// batch, for the Playable checks of the list and of its rendering (which
// otherwise load them one by one; On Deck: hundreds of shows). They are read
// from the mirror as visibility rows, with the facts u's policy reads, not
// as full titles, and seriesTitle answers from them: a list never waits on
// a provider refresh of a show. Plex's own detail cache reads the shows it
// renders in full separately (titleCache). A catalogue without visibility
// rows preloads the series into its title cache instead.
func (s *Server) withSeriesRows(ctx context.Context, u *frontend.User, ts []frontend.Title) context.Context {
	have, _ := ctx.Value(seriesRowsKey{}).(map[frontend.RatingKey]frontend.Title)
	var shows []frontend.RatingKey
	seen := map[frontend.RatingKey]bool{}
	for i := range ts {
		if k := ts[i].ShowKey; k != 0 && !seen[k] {
			seen[k] = true
			if _, ok := have[k]; !ok {
				shows = append(shows, k)
			}
		}
	}
	if len(shows) == 0 {
		return ctx
	}
	if rows, ok := s.catalog().(frontend.VisibilityRowsCatalog); ok {
		parents, err := rows.CachedVisibilityRows(ctx, shows, s.visibility().TitleFacts(u))
		if err == nil {
			merged := make(map[frontend.RatingKey]frontend.Title, len(have)+len(parents))
			for k, t := range have {
				merged[k] = t
			}
			for k, t := range parents {
				merged[k] = t
			}
			return context.WithValue(ctx, seriesRowsKey{}, merged)
		}
	}
	if pre, ok := s.catalog().(frontend.StoredTitlePreloader); ok && len(shows) > 1 {
		pre.PreloadStored(ctx, shows)
	}
	return ctx
}

// seriesTitle is the series lookup of s.visibility() (Playable): a series
// withSeriesRows read for this request, else artworkTitle.
func (s *Server) seriesTitle(ctx context.Context, key frontend.RatingKey) (*frontend.Title, error) {
	if rows, _ := ctx.Value(seriesRowsKey{}).(map[frontend.RatingKey]frontend.Title); rows != nil {
		if t, ok := rows[key]; ok {
			return &t, nil
		}
	}
	return s.artworkTitle(ctx, key)
}
