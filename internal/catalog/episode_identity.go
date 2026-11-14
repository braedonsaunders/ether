package catalog

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("catalog_episode_identity", 1, `CREATE INDEX IF NOT EXISTS catalog_titles_type_lower_original ON catalog_titles(type, LOWER(original_title), year);`)
}

// EpisodeIdentity reads the local mirror only. Every lookup uses a title or
// show index and a fixed row limit, independent of the library's size. It
// neither fetches missing episodes nor schedules background work.
func (c *Catalog) EpisodeIdentity(ctx context.Context, show core.RatingKey, season, episode int, names []string) (core.EpisodeIdentity, error) {
	out := core.EpisodeIdentity{OtherShows: map[string][]core.EpisodeNamesake{}, Seasons: map[int]string{}}
	if len(names) > 8 {
		names = names[:8]
	}
	for _, name := range names {
		variants := namesakeTitleVariants(name)
		if len(variants) == 0 {
			continue
		}
		args := []any{string(core.TypeShow)}
		for _, v := range variants {
			args = append(args, strings.ToLower(v))
		}
		args = append(args, show)
		seen := map[int64]bool{}
		for _, column := range []string{"title", "original_title"} {
			// The scalar episode lookup uses catalog_titles_show and never fetches
			// remote metadata. Keeping it in SQL avoids per-match round trips.
			queryArgs := []any{season, episode}
			queryArgs = append(queryArgs, args...)
			rows, err := c.db.QueryContext(ctx, `SELECT s.key, s.`+column+`, s.year,
                COALESCE((SELECT e.title FROM catalog_titles e WHERE e.show_key=s.key AND e.season=? AND e.episode=? AND e.type='episode' LIMIT 1), '')
                FROM catalog_titles s WHERE s.type=? AND LOWER(s.`+column+`) IN (`+strings.TrimSuffix(strings.Repeat("?,", len(variants)), ",")+`) AND s.key<>? LIMIT 16`, queryArgs...)
			if err != nil {
				return out, err
			}
			for rows.Next() {
				var key int64
				var title string
				var other core.EpisodeNamesake
				if err = rows.Scan(&key, &title, &other.Year, &other.EpisodeTitle); err != nil {
					break
				}
				if !seen[key] && releaseparse.NormalizeTitle(title) == releaseparse.NormalizeTitle(name) {
					seen[key] = true
					out.OtherShows[name] = append(out.OtherShows[name], other)
				}
			}
			scanErr := rows.Err()
			rows.Close()
			if err != nil {
				return out, err
			}
			if scanErr != nil {
				return out, scanErr
			}
		}
	}
	rows, err := c.db.QueryContext(ctx, `SELECT season, title FROM catalog_titles WHERE show_key=? AND type='season' ORDER BY season LIMIT 128`, show)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		var title string
		if err := rows.Scan(&n, &title); err != nil {
			return out, err
		}
		out.Seasons[n] = title
	}
	return out, rows.Err()
}
