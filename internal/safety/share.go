package safety

import (
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
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
// looking them up on a browse path would scale with library size.
const LabelMapNote = "TMDB collection name only. Owner collections and Plex item labels are not on the title row."

type shareClause struct {
	field  string
	negate bool
	lte    bool
	gte    bool
	values []string
}

func (s *Service) SetPlexShare(fn func(*core.User) PlexShare) {
	s.mu.Lock()
	s.plexShare = fn
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

func shareFilter(t *core.Title, share PlexShare) string {
	if t == nil {
		return ""
	}
	switch t.Type {
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		return share.FilterTelevision
	default:
		return share.FilterMovies
	}
}

func parseShareFilter(spec string) []shareClause {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	var out []shareClause
	for _, raw := range strings.Split(spec, "|") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		c, ok := parseShareClause(raw)
		if ok {
			out = append(out, c)
		}
	}
	return out
}

func parseShareClause(raw string) (shareClause, bool) {
	var c shareClause
	switch {
	case strings.Contains(raw, "!="):
		field, values, ok := strings.Cut(raw, "!=")
		if !ok {
			return c, false
		}
		c.field, c.negate, c.values = strings.TrimSpace(field), true, splitShareValues(values)
	case strings.Contains(raw, "<<="):
		field, values, ok := strings.Cut(raw, "<<=")
		if !ok {
			return c, false
		}
		c.field, c.lte, c.values = strings.TrimSpace(field), true, splitShareValues(values)
	case strings.Contains(raw, ">>="):
		field, values, ok := strings.Cut(raw, ">>=")
		if !ok {
			return c, false
		}
		c.field, c.gte, c.values = strings.TrimSpace(field), true, splitShareValues(values)
	case strings.Contains(raw, "<="):
		field, values, ok := strings.Cut(raw, "<=")
		if !ok {
			return c, false
		}
		c.field, c.lte, c.values = strings.TrimSpace(field), true, splitShareValues(values)
	case strings.Contains(raw, ">="):
		field, values, ok := strings.Cut(raw, ">=")
		if !ok {
			return c, false
		}
		c.field, c.gte, c.values = strings.TrimSpace(field), true, splitShareValues(values)
	default:
		field, values, ok := strings.Cut(raw, "=")
		if !ok {
			return c, false
		}
		c.field, c.values = strings.TrimSpace(field), splitShareValues(values)
	}
	switch c.field {
	case "contentRating", "label":
		return c, true
	default:
		return shareClause{}, false
	}
}

func splitShareValues(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if decoded, err := url.QueryUnescape(part); err == nil {
			part = decoded
		}
		out = append(out, part)
	}
	return out
}

func sharePasses(t *core.Title, spec string) bool {
	if t == nil || spec == "" {
		return true
	}
	for _, c := range parseShareFilter(spec) {
		if !shareClausePasses(t, c) {
			return false
		}
	}
	return true
}

func shareClausePasses(t *core.Title, c shareClause) bool {
	switch c.field {
	case "contentRating":
		return ratingClausePasses(t.ContentRating, c)
	case "label":
		return labelClausePasses(t, c)
	default:
		return true
	}
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
	share := s.shareFor(u)
	if share.AllowSync != nil && !*share.AllowSync {
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
