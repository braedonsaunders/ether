package safety

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sharefilter"
)

// PlexShare is the cached plex.tv grant for one viewer. Ether owner policy
// for that viewer overrides every field here.
type PlexShare struct {
	FilterMovies     string
	FilterTelevision string
	AllowSync        *bool
}

// LabelMapNote records what a plex.tv label clause can match on Ether.
// Owner collections and Plex item labels are not stored on the title row;
// looking them up on a browse path would scale with library size. A title
// without a TMDB collection name is allowed (the clause is a no-op).
const LabelMapNote = "TMDB collection name only. Titles without a collection name are not hidden. Owner collections and Plex item labels are not on the title row."

type shareClause struct {
	field  string
	negate bool
	lte    bool
	gte    bool
	values []string
}

type compiledFilter struct {
	allowRatings, denyRatings []string
	maxRating, minRating      string
	allowLabels, denyLabels   []string
}

type compiledShare struct {
	movies, tv compiledFilter
	allowSync  *bool
}

type cachedShare struct {
	src PlexShare
	out compiledShare
}

func (s *Service) SetPlexShare(fn func(*core.User) PlexShare) {
	s.mu.Lock()
	s.plexShare = fn
	s.shareCache = nil
	s.mu.Unlock()
}

func (s *Service) SetFriendsDownloads(fn func() bool) {
	s.mu.Lock()
	s.friendsDownloads = fn
	s.mu.Unlock()
}

func (s *Service) shareFor(u *core.User) PlexShare {
	if s == nil || u == nil {
		return PlexShare{}
	}
	s.mu.RLock()
	fn := s.plexShare
	s.mu.RUnlock()
	if fn == nil {
		return PlexShare{}
	}
	return fn(u)
}

func (s *Service) compiledFor(u *core.User) compiledShare {
	if s == nil || u == nil {
		return compiledShare{}
	}
	raw := s.shareFor(u)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shareCache == nil {
		s.shareCache = map[int64]cachedShare{}
	}
	if have, ok := s.shareCache[u.ID]; ok && shareSame(have.src, raw) {
		return have.out
	}
	out := compileShare(raw)
	if have, ok := s.shareCache[u.ID]; !ok || !compiledSame(have.out, out) {
		if ok {
			s.gen.Add(1)
		}
	}
	s.shareCache[u.ID] = cachedShare{src: raw, out: out}
	return out
}

func shareSame(a, b PlexShare) bool {
	if a.FilterMovies != b.FilterMovies || a.FilterTelevision != b.FilterTelevision {
		return false
	}
	if a.AllowSync == nil && b.AllowSync == nil {
		return true
	}
	return a.AllowSync != nil && b.AllowSync != nil && *a.AllowSync == *b.AllowSync
}

func compiledSame(a, b compiledShare) bool {
	return filterSame(a.movies, b.movies) && filterSame(a.tv, b.tv) && shareSame(PlexShare{AllowSync: a.allowSync}, PlexShare{AllowSync: b.allowSync})
}

func filterSame(a, b compiledFilter) bool {
	return strings.Join(a.allowRatings, "\x00") == strings.Join(b.allowRatings, "\x00") &&
		strings.Join(a.denyRatings, "\x00") == strings.Join(b.denyRatings, "\x00") &&
		a.maxRating == b.maxRating && a.minRating == b.minRating &&
		strings.Join(a.allowLabels, "\x00") == strings.Join(b.allowLabels, "\x00") &&
		strings.Join(a.denyLabels, "\x00") == strings.Join(b.denyLabels, "\x00")
}

func compileShare(s PlexShare) compiledShare {
	return compiledShare{
		movies:    compileFilter(s.FilterMovies),
		tv:        compileFilter(s.FilterTelevision),
		allowSync: s.AllowSync,
	}
}

func compileFilter(spec string) compiledFilter {
	var out compiledFilter
	for _, c := range sharefilter.Parse(spec) {
		switch c.Field {
		case "contentRating":
			switch {
			case c.LTE && len(c.Values) > 0:
				out.maxRating = c.Values[0]
			case c.GTE && len(c.Values) > 0:
				out.minRating = c.Values[0]
			case c.Negate:
				out.denyRatings = append(out.denyRatings, c.Values...)
			default:
				out.allowRatings = append(out.allowRatings, c.Values...)
			}
		case "label":
			if c.Negate {
				out.denyLabels = append(out.denyLabels, c.Values...)
			} else {
				out.allowLabels = append(out.allowLabels, c.Values...)
			}
		}
	}
	return out
}

func (c compiledFilter) forType(typ core.MediaType, ownerRating bool) core.Filter {
	var f core.Filter
	if !ownerRating {
		f.ContentRatings = append([]string(nil), c.allowRatings...)
		f.ExcludeContentRatings = append([]string(nil), c.denyRatings...)
		f.MaxContentRating = c.maxRating
		f.MinContentRating = c.minRating
	}
	f.IncludeCollectionNames = append([]string(nil), c.allowLabels...)
	f.ExcludeCollectionNames = append([]string(nil), c.denyLabels...)
	_ = typ
	return f
}

func shareFilterFor(t *core.Title, share compiledShare) compiledFilter {
	if t == nil {
		return compiledFilter{}
	}
	switch t.Type {
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		return share.tv
	default:
		return share.movies
	}
}

func sharePassesCompiled(t *core.Title, f compiledFilter) bool {
	if t == nil {
		return true
	}
	if !ratingCompiledPasses(t.ContentRating, f) {
		return false
	}
	return labelCompiledPasses(t, f)
}

func ratingCompiledPasses(rating string, f compiledFilter) bool {
	if f.maxRating != "" {
		if !ratingClausePasses(rating, shareClause{lte: true, values: []string{f.maxRating}}) {
			return false
		}
	}
	if f.minRating != "" {
		if !ratingClausePasses(rating, shareClause{gte: true, values: []string{f.minRating}}) {
			return false
		}
	}
	if len(f.allowRatings) > 0 && !ratingClausePasses(rating, shareClause{values: f.allowRatings}) {
		return false
	}
	if len(f.denyRatings) > 0 && !ratingClausePasses(rating, shareClause{negate: true, values: f.denyRatings}) {
		return false
	}
	return true
}

func labelCompiledPasses(t *core.Title, f compiledFilter) bool {
	if len(f.allowLabels) > 0 && !labelClausePasses(t, shareClause{values: f.allowLabels}) {
		return false
	}
	if len(f.denyLabels) > 0 && !labelClausePasses(t, shareClause{negate: true, values: f.denyLabels}) {
		return false
	}
	return true
}

func ratingClausePasses(rating string, c shareClause) bool {
	if c.lte || c.gte {
		if len(c.values) == 0 {
			return true
		}
		limit := contentrating.LevelOf(c.values[0])
		have := contentrating.LevelOf(rating)
		if limit == contentrating.Unrated {
			return false
		}
		if have == contentrating.Unrated {
			return false
		}
		if c.lte {
			return have <= limit
		}
		return have >= limit
	}
	match := ratingValueMatch(rating, c.values)
	if c.negate {
		return !match
	}
	return match
}

func ratingValueMatch(rating string, values []string) bool {
	have := strings.TrimSpace(rating)
	for _, want := range values {
		if isNoneRating(want) {
			if have == "" || contentrating.IsNotRated(have) {
				return true
			}
			continue
		}
		if strings.EqualFold(have, want) {
			return true
		}
	}
	return false
}

func isNoneRating(v string) bool {
	return strings.EqualFold(v, "None") || strings.EqualFold(v, "NR")
}

func labelClausePasses(t *core.Title, c shareClause) bool {
	have := strings.TrimSpace(t.CollectionName)
	if have == "" {
		// Unmappable: no TMDB collection name. Do not hide the title.
		return true
	}
	match := false
	for _, want := range c.values {
		if want != "" && strings.EqualFold(have, want) {
			match = true
			break
		}
	}
	if c.negate {
		return !match
	}
	return match
}

func ownerSetsRating(u *core.User, p Policy) bool {
	return (u != nil && strings.TrimSpace(u.MaxContentRating) != "") || strings.TrimSpace(p.MaxContentRating) != ""
}

// AllowDownloads implements core.DownloadAccessPolicy. An owner-set Ether
// flag wins; otherwise plex.tv allowSync=0 refuses the queue. Friends then
// follow the owner's AllowFriends setting.
func (s *Service) AllowDownloads(u *core.User) bool {
	if u == nil {
		return false
	}
	if u.IsOwner {
		return true
	}
	p := s.Get(u.ID)
	if p.AllowDownloads != nil {
		return *p.AllowDownloads
	}
	share := s.compiledFor(u)
	if share.allowSync != nil && !*share.allowSync {
		return false
	}
	s.mu.RLock()
	fn := s.friendsDownloads
	s.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return true
}

// FormatShareFilter turns a plex.tv filter spec into owner-readable parts.
func FormatShareFilter(spec string) []ShareFilterPart {
	parts := sharefilter.Format(spec)
	out := make([]ShareFilterPart, len(parts))
	for i, p := range parts {
		out[i] = ShareFilterPart{Kind: p.Kind, Values: p.Values}
	}
	return out
}

// ShareFilterPart is one owner-readable clause of a plex.tv share filter.
type ShareFilterPart = sharefilter.Part
