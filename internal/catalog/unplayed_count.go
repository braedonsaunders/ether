package catalog

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// CountUnplayedAired counts aired episodes a viewer has not played, for each
// show or season key. It uses the aired index and the watch primary key.
// It does not load episode rows or refresh metadata. An empty max rating
// counts every aired episode. A set rating drops unrated and stricter ones.
func (c *Catalog) CountUnplayedAired(ctx context.Context, userID int64, keys []core.RatingKey, maxRating string) (map[core.RatingKey]int, error) {
	out := map[core.RatingKey]int{}
	if c == nil || c.db == nil || userID == 0 || len(keys) == 0 {
		return out, nil
	}
	shows := make([]core.RatingKey, 0, len(keys))
	seen := map[core.RatingKey]bool{}
	for _, key := range keys {
		out[key] = 0
		var show core.RatingKey
		switch key.Kind() {
		case core.KindShow:
			show = key
		case core.KindSeason:
			show = core.ShowKey(key.TMDB())
		default:
			continue
		}
		if !seen[show] {
			seen[show] = true
			shows = append(shows, show)
		}
	}
	if len(shows) == 0 {
		return out, nil
	}
	restrict := strings.TrimSpace(maxRating) != ""
	rank := 0
	if restrict {
		rank = RatingRank(maxRating)
		if rank == 0 {
			return out, nil
		}
	}
	now := c.now().Unix()
	showTotals := map[core.RatingKey]int{}
	seasonTotals := map[core.RatingKey]int{}
	const chunk = 200
	for start := 0; start < len(shows); start += chunk {
		part := shows[start:min(start+chunk, len(shows))]
		args := []any{now}
		for _, show := range part {
			args = append(args, int64(show))
		}
		args = append(args, userID)
		ratingSQL := ""
		if restrict {
			ratingSQL = ` AND e.content_rating_rank>0 AND e.content_rating_rank<=?`
			args = append(args, rank)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT e.show_key, e.season, COUNT(*)
FROM catalog_titles e
WHERE e.type='episode' AND e.adult=0 AND e.released_ts>0 AND e.released_ts<=?
AND e.show_key IN (`+placeholders(len(part))+`)
AND NOT EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id=? AND w.rating_key=e.key AND w.view_count>0)`+ratingSQL+`
GROUP BY e.show_key, e.season`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var show int64
			var season, count int
			if err := rows.Scan(&show, &season, &count); err != nil {
				rows.Close()
				return nil, err
			}
			showKey := core.RatingKey(show)
			showTotals[showKey] += count
			seasonTotals[core.SeasonKey(showKey.TMDB(), season)] += count
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	for _, key := range keys {
		switch key.Kind() {
		case core.KindShow:
			out[key] = showTotals[key]
		case core.KindSeason:
			out[key] = seasonTotals[key]
		}
	}
	return out, nil
}
