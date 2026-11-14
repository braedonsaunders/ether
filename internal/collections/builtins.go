package collections

import (
	"context"

	"encoding/json"

	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// builtin is a collection Ether ships: a live TMDB list. Owners can
// disable it, retarget it, rotate it and change its artwork; everything
// else is fixed.
type builtin struct {
	id    int64
	slug  string
	title string
	list  string
	media string
}

var builtins = []builtin{
	{builtinBase + 1, "trending-week-movie", "Trending Movies This Week", "trending_week", MediaMovie},
	{builtinBase + 2, "trending-week-show", "Trending Shows This Week", "trending_week", MediaShow},
	{builtinBase + 3, "trending-day-movie", "Trending Movies Today", "trending_day", MediaMovie},
	{builtinBase + 4, "trending-day-show", "Trending Shows Today", "trending_day", MediaShow},
	{builtinBase + 5, "top-rated-movie", "Top Rated Movies", "top_rated", MediaMovie},
	{builtinBase + 6, "top-rated-show", "Top Rated Shows", "top_rated", MediaShow},
	{builtinBase + 7, "popular-movie", "Popular Movies", "popular", MediaMovie},
	{builtinBase + 8, "popular-show", "Popular Shows", "popular", MediaShow},
	{builtinBase + 9, "now-playing-movie", "Now Playing", "now_playing", MediaMovie},
	{builtinBase + 10, "upcoming-movie", "Upcoming Movies", "upcoming", MediaMovie},
	{builtinBase + 11, "airing-today-show", "Airing Today", "airing_today", MediaShow},
	{builtinBase + 12, "on-the-air-show", "On The Air", "on_the_air", MediaShow},
	{builtinBase + 13, "new-releases-movie", "New Releases", "now_playing", MediaMovie},
}

func (b builtin) spec() core.CollectionSpec {
	sp := core.CollectionSpec{
		Name: b.title, MediaType: b.media,
		Sources: []core.CollectionSource{{Kind: "tmdbList", List: b.list, MediaType: b.media}},
		Match:   MatchAll, Sort: "source", Limit: 100, Refresh: RefreshHourly, Enabled: true,
		Artwork: core.CollectionArtwork{Mode: "auto"},
		Targets: core.CollectionTargets{CollectionsTab: false, Users: core.AllUsers()},
	}
	Normalize(&sp)
	return sp
}

// seedBuiltins inserts missing built-ins and refreshes the fixed parts of
// existing ones, keeping the owner's edits to editable fields.
func (s *Service) seedBuiltins(ctx context.Context) error {
	for _, b := range builtins {
		want := b.spec()
		row, err := s.rowByID(ctx, idString(b.id))
		if err == nil {
			merged := mergeBuiltin(want, row.spec)
			js, err := specJSON(merged)
			if err != nil {
				return err
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE collections_specs SET slug=?, builtin=1, spec_json=? WHERE id=?`, b.slug, js, b.id); err != nil {
				return err
			}
			continue
		}
		js, err := specJSON(want)
		if err != nil {
			return err
		}
		now := s.now().Unix()
		// Another node starting at the same time may insert it first.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO collections_specs(id,slug,builtin,owner_user_id,spec_json,enabled,created_at,updated_at)
			VALUES(?,?,1,0,?,1,?,?) ON CONFLICT(id) DO NOTHING`, b.id, b.slug, js, now, now); err != nil {
			return err
		}
	}
	return nil
}

// mergeBuiltin keeps a built-in's fixed definition and the owner's edits.
func mergeBuiltin(fixed, edited core.CollectionSpec) core.CollectionSpec {
	out := fixed
	out.Enabled = edited.Enabled
	out.PreviewOnFocus = edited.PreviewOnFocus
	out.Targets = edited.Targets
	out.Artwork = edited.Artwork
	out.Rotate = edited.Rotate
	if edited.Limit > 0 && edited.Limit <= 100 {
		out.Limit = edited.Limit
	}
	return out
}

func mkRule(field, op string, v any) core.CollectionRule {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("collections: rule value: %v", err))
	}
	return core.CollectionRule{Field: field, Op: op, Value: b}
}

func ratingExceeds(rating, max string) bool { return contentrating.Exceeds(rating, max) }

// refreshInterval is how often a scheduled collection is rebuilt.
func refreshInterval(r string) time.Duration {
	switch r {
	case RefreshHourly:
		return time.Hour
	case RefreshWeekly:
		return 7 * 24 * time.Hour
	case RefreshLive:
		return 0
	}
	return 24 * time.Hour
}
