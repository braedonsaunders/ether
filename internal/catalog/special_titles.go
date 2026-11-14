package catalog

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// SpecialTitles reads only this show's stored season-zero titles through the
// (show_key, season, episode) index. It never loads a show or contacts TMDB.
func (c *Catalog) SpecialTitles(ctx context.Context, showID int64) []string {
	if showID <= 0 {
		return nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT title FROM catalog_titles WHERE show_key=? AND season=0 AND type='episode' ORDER BY episode LIMIT 1000`, int64(core.ShowKey(showID)))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var titles []string
	for rows.Next() {
		var title string
		if rows.Scan(&title) != nil {
			return nil
		}
		titles = append(titles, title)
	}
	if rows.Err() != nil {
		return nil
	}
	return titles
}
