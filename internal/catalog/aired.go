package catalog

import (
	"context"
	"database/sql"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// seasonAired is what the mirror knows about one season's aired episodes.
type seasonAired struct {
	date       int64 // zero means unknown; negative values are historical dates
	released   bool  // the season's own air date is known and not in the future
	stored     int   // TMDB episode_count from the season summary
	mirrored   bool  // the season's episodes are in the mirror
	airedCount int   // mirrored episodes with a known, past air date
}

// count is the season's aired episodes: exact once its episodes are
// mirrored, else TMDB's episode count for a season that has premiered.
// A season without its own past air date counts nothing, as it is not
// listed (Children filters it the same way).
func (s seasonAired) count() int {
	if !s.released {
		return 0
	}
	if s.mirrored {
		return s.airedCount
	}
	return s.stored
}

// applyAiredCounts replaces the child and leaf counts of shows and seasons,
// which TMDB reports including announced but unaired episodes, with the
// episodes viewers can actually list and play. It is applied when titles are
// read rather than stored, so an episode moves into the counts the moment
// its air date passes, without waiting for a refresh. A show the mirror
// holds no seasons for keeps its stored counts.
func applyAiredCounts(ctx context.Context, db *sql.DB, now time.Time, items []*core.Title) error {
	var showArgs []any
	seen := map[core.RatingKey]bool{}
	for _, t := range items {
		var show core.RatingKey
		switch t.Type {
		case core.TypeShow:
			show = t.Key
		case core.TypeSeason:
			show = core.ShowKey(t.Key.TMDB())
		default:
			continue
		}
		if show.Kind() == core.KindShow && !seen[show] {
			seen[show] = true
			showArgs = append(showArgs, int64(show))
		}
	}
	if len(showArgs) == 0 {
		return nil
	}
	type seasonID struct {
		show   core.RatingKey
		season int
	}
	seasons := map[seasonID]*seasonAired{}
	var order []seasonID
	nowTS := now.Unix()
	// Query in batches (same 200 as hydrateSlice in store.go): a single
	// IN (...) over a large slice overflows SQLite's bound-variable limit.
	// Batches partition the deduplicated showArgs, so seasons/order keep
	// accumulating in the same order as one unchunked query.
	const batch = 200
	for start := 0; start < len(showArgs); start += batch {
		part := showArgs[start:min(start+batch, len(showArgs))]
		ph := placeholders(len(part))
		rows, err := db.QueryContext(ctx, `SELECT show_key,season,released_ts,child_count FROM catalog_titles WHERE type='season' AND show_key IN (`+ph+`)`, part...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var show, released int64
			var season, child int
			if err := rows.Scan(&show, &season, &released, &child); err != nil {
				rows.Close()
				return err
			}
			if !episodeKeyFits(season, 0) {
				continue
			}
			id := seasonID{core.RatingKey(show), season}
			// Negative Unix timestamps are valid pre-1970 air dates; only zero is unknown.
			seasons[id] = &seasonAired{date: released, released: released != 0 && released <= nowTS, stored: child}
			order = append(order, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for start := 0; start < len(showArgs); start += batch {
		part := showArgs[start:min(start+batch, len(showArgs))]
		ph := placeholders(len(part))
		// COUNT(CASE ...) rather than SUM so both SQLite and PostgreSQL return
		// an integer.
		rows, err := db.QueryContext(ctx, `SELECT show_key,season,COUNT(*),COUNT(CASE WHEN released_ts<>0 AND released_ts<=? AND adult=0 THEN 1 END),
			COALESCE(MIN(CASE WHEN released_ts<>0 AND released_ts<=? AND adult=0 THEN released_ts END),0)
			FROM catalog_titles WHERE type='episode' AND show_key IN (`+ph+`) GROUP BY show_key,season`, append([]any{nowTS, nowTS}, part...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var show int64
			var earliest int64
			var season, total, aired int
			if err := rows.Scan(&show, &season, &total, &aired, &earliest); err != nil {
				rows.Close()
				return err
			}
			if s := seasons[seasonID{core.RatingKey(show), season}]; s != nil && total > 0 {
				s.mirrored, s.airedCount = true, aired
				// Specials often have no season premiere. A dated aired child is
				// sufficient to open this container, but never overrides a future date.
				if season == 0 && s.date == 0 && earliest != 0 {
					s.date, s.released = earliest, true
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	type showAired struct{ seasons, episodes int }
	shows := map[core.RatingKey]*showAired{}
	for _, id := range order {
		sh := shows[id.show]
		if sh == nil {
			sh = &showAired{}
			shows[id.show] = sh
		}
		// Show totals keep TMDB's meaning (number_of_seasons/episodes):
		// regular seasons only. Specials are listed, but "3 seasons" and
		// a limited series' single season must not count them.
		if n := seasons[id].count(); n > 0 && id.season > 0 {
			sh.seasons++
			sh.episodes += n
		}
	}
	for _, t := range items {
		switch t.Type {
		case core.TypeShow:
			if sh := shows[t.Key]; sh != nil {
				t.ChildCount, t.LeafCount = sh.seasons, sh.episodes
			}
		case core.TypeSeason:
			if s := seasons[seasonID{core.ShowKey(t.Key.TMDB()), t.Key.Season()}]; s != nil {
				t.ChildCount = s.count()
				if t.Released.IsZero() && t.Key.Season() == 0 && s.date != 0 {
					t.Released = time.Unix(s.date, 0).UTC()
				}
			}
		}
	}
	return nil
}

// airedCounts is applyAiredCounts for a slice of catalogue titles.
func (c *Catalog) airedCounts(ctx context.Context, items []core.Title) error {
	ptrs := make([]*core.Title, 0, len(items))
	for i := range items {
		if items[i].Type == core.TypeShow || items[i].Type == core.TypeSeason {
			ptrs = append(ptrs, &items[i])
		}
	}
	if len(ptrs) == 0 {
		return nil
	}
	return applyAiredCounts(ctx, c.db, c.now(), ptrs)
}

// listableSeason reports whether a season belongs in listings and navigation:
// its own air date has passed and at least one of its episodes has aired.
// t must carry aired counts (applyAiredCounts).
func listableSeason(t *core.Title, now time.Time) bool {
	return t.IsReleased(now) && t.ChildCount > 0
}
