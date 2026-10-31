package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// AirDates returns dates from locally mirrored regular episodes, excluding future air dates.
// For premieres, the highest numbered season with an aired episode must have an aired episode one.
func (m *Mirror) AirDates(ctx context.Context, now time.Time, premiere bool) (map[core.RatingKey]int64, error) {
	cutoff := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC).Unix()
	query := `SELECT show_key, MAX(released_ts) FROM catalog_titles
		WHERE type = 'episode' AND season > 0 AND episode > 0 AND release_known = 1 AND released_ts < ? GROUP BY show_key`
	if premiere {
		query = `SELECT show_key, released_ts FROM catalog_titles e
			WHERE type = 'episode' AND season > 0 AND episode = 1 AND release_known = 1 AND released_ts < ?
			AND season = (SELECT MAX(season) FROM catalog_titles x WHERE x.show_key = e.show_key AND x.type = 'episode' AND x.season > 0 AND x.episode > 0 AND x.release_known = 1 AND x.released_ts < ?)`
	}
	args := []any{cutoff}
	if premiere {
		args = append(args, cutoff)
	}
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dates := make(map[core.RatingKey]int64)
	for rows.Next() {
		var key core.RatingKey
		var date int64
		if err := rows.Scan(&key, &date); err != nil {
			return nil, err
		}
		if prev, ok := dates[key]; !ok || date > prev {
			dates[key] = date
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dates, nil
}
