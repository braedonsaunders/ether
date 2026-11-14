package catalog

import (
	"context"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// A missing season date says nothing about its individually dated episodes.
// Only aired children can establish a missing premiere; explicit dates stand.
func dateSeasonFromAiredEpisodes(season *core.Title, episodes []core.Title, now time.Time) {
	if !season.Released.IsZero() {
		return
	}
	for _, episode := range episodes {
		if !episode.Adult && episode.IsReleased(now) && (season.Released.IsZero() || episode.Released.Before(season.Released)) {
			season.Released = episode.Released
		}
	}
	if !season.Released.IsZero() {
		season.Year = season.Released.Year()
	}
}

// dateSeasonSummaries opens undated numbered seasons on a show refresh so
// browsing them can fetch their episodes. It reads only this show's stored
// episodes, once, and never fetches seasons eagerly. An inferred premiere is
// provisional: the season payload replaces it with the first aired child.
func (c *Catalog) dateSeasonSummaries(ctx context.Context, show core.Title, details *tmdb.TVDetails, seasons []core.Title) error {
	needed := false
	for _, season := range seasons {
		needed = needed || season.Season > 0 && season.ChildCount > 0 && season.Released.IsZero()
	}
	if !needed {
		return nil
	}
	now := c.now()
	stored := map[int]time.Time{}
	rows, err := c.db.QueryContext(ctx, `SELECT season,MIN(released_ts) FROM catalog_titles WHERE show_key=? AND `+airedTypeColumn(c.db)+`='episode' AND release_known=1 AND released_ts<=? AND adult=0 GROUP BY season`, int64(show.Key), now.Unix())
	if err != nil {
		return err
	}
	for rows.Next() {
		var season int
		var date int64
		if err := rows.Scan(&season, &date); err != nil {
			rows.Close()
			return err
		}
		stored[season] = time.Unix(date, 0).UTC()
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	lastAired := 0
	if last := details.LastEpisodeToAir; last != nil {
		date := tmdb.ParseDate(last.AirDate)
		if !date.IsZero() && !date.After(now) {
			lastAired = last.SeasonNumber
		}
	}
	order := make([]int, len(seasons))
	for i := range seasons {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return seasons[order[i]].Season < seasons[order[j]].Season })
	previous := show.Released
	if previous.After(now) {
		previous = time.Time{}
	}
	for _, i := range order {
		season := &seasons[i]
		if season.Season <= 0 {
			continue
		}
		if season.Released.IsZero() && season.ChildCount > 0 {
			if date := stored[season.Season]; !date.IsZero() {
				season.Released = date
			} else if season.Season <= lastAired {
				season.Released = previous
			}
			if !season.Released.IsZero() {
				season.Year = season.Released.Year()
			}
		}
		if !season.Released.IsZero() && !season.Released.After(now) {
			previous = season.Released
		}
	}
	return nil
}
