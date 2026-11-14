package plex

import (
	"context"

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
	out := s.visibility().TitlesAllowed(u, ts)
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

func (s *Server) visibility() frontend.Visibility {
	v := frontend.Visibility{Policy: s.policy(), FilterAvailable: s.filterAvailable(), JudgedOnFacts: s.judgedOnFacts(),
		SectionFor: func(t frontend.MediaType) (int, bool) { sec, ok := s.untitledSectionFor(t); return sec.ID, ok }}
	if s.catalog() != nil {
		v.GetTitle = s.seriesTitle
	}
	if s.playback() != nil {
		v.Sessions = s.playback().Sessions
	}
	return v
}

// openableTitles is the common row/detail admission decision. A pinned shelf
// retains its availability cut, but never bypasses current authorization.
func (s *Server) openableTitles(ctx context.Context, u *frontend.User, ts []frontend.Title, pinned bool) []frontend.Title {
	// Plugin items are authorized by the Experience when projected; the
	// catalogue openability and built-in visibility checks do not know them,
	// so keep them without those filters (spec §6.4).
	var kind, catalog []frontend.Title
	for i := range ts {
		if ts[i].Key.IsPluginKey() {
			kind = append(kind, ts[i])
		} else {
			catalog = append(catalog, ts[i])
		}
	}
	out := kind[:0]
	for i := range kind {
		if s.plexMetaRatingAllowed(u, &kind[i]) {
			out = append(out, kind[i])
		}
	}
	ts = catalog
	ts = s.visibleTitles(u, ts)
	ctx = s.withSeriesRows(ctx, u, ts)
	if !pinned {
		ts = s.availableForViewer(ctx, u, ts)
	}
	for _, t := range ts {
		_, admitted := shelfMember(ctx, t.Key)
		if (admitted || s.admittedTitle(&t)) && s.playableTitle(ctx, u, &t) {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) visibleTitle(ctx context.Context, u *frontend.User, t *frontend.Title) bool {
	return s.visibleTitleWith(ctx, s.visibility(), u, t)
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
	return s.visibility().Playable(ctx, u, t) && s.plexMetaRatingAllowed(u, t)
}

// pluginAwarePlayable is playableTitle for plugin items. A plugin item's
// canonical library is its own; the visibility policy's SectionFor maps a
// projected MediaType onto a built-in section, which would wrongly test a
// music/photo item against Movies or TV Shows. Plugin items are authorized by
// the Experience when the metadata lookup succeeds (it checks the viewer
// against the owning library), so no built-in section check applies.
func (s *Server) pluginAwarePlayable(ctx context.Context, u *frontend.User, t *frontend.Title) bool {
	if t != nil && t.Key.IsPluginKey() {
		return s.plexMetaRatingAllowed(u, t)
	}
	return s.playableTitle(ctx, u, t)
}
func (s *Server) availableForViewer(ctx context.Context, u *frontend.User, ts []frontend.Title) []frontend.Title {
	return s.visibility().Available(ctx, u, ts)
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
