package catalog

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// ScheduledEpisodes is deliberately separate from Children/Get: discovery,
// autoplay, downloads and playback must never acquire future episodes through it.
func (c *Catalog) ScheduledEpisodes(ctx context.Context, key core.RatingKey) ([]core.Title, error) {
	if !c.config().ShowUpcomingEpisodes || key.Kind() != core.KindSeason || key.Season() <= 0 {
		return nil, core.ErrNotFound
	}
	show, err := c.Get(ctx, core.ShowKey(key.TMDB()))
	if err != nil {
		return nil, err
	}
	if _, err := c.Get(ctx, key); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND show_key=? AND season=? AND adult=0 AND released_ts>? ORDER BY episode ASC LIMIT 1000`, int64(core.ShowKey(key.TMDB())), key.Season(), c.now().Unix())
	if err != nil {
		return nil, err
	}
	var out []core.Title
	for rows.Next() {
		st, err := scanTitle(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, st.t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err := c.hydrateSlice(ctx, out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].ContentRating == "" {
			out[i].ContentRating = show.ContentRating
		}
	}
	return out, nil
}
