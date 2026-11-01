package catalog

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ScheduleEntry is one dated title in the owner's release calendar, read
// from the local mirror only. Playable reports whether the catalogue would
// offer it right now (released, not adult, past the home-release gate).
type ScheduleEntry struct {
	Title core.Title
	// SeasonEpisodes is the highest mirrored episode number of an
	// episode's season (TMDB lists a season's future episodes too), so
	// callers can mark a season finale.
	SeasonEpisodes int
	// ShowPoster is an episode's or season's series poster path.
	ShowPoster string
	Playable   bool
}

// scheduleBatch bounds SQLite parameters per statement.
const scheduleBatch = 300

// ScheduledShows returns the dated episodes of shows released in
// [from, until), including future ones, and each episode's season size.
// Episodes inherit their show's content rating so callers can apply viewer
// policy. It never contacts TMDB: only mirrored seasons are listed (see
// RefreshSchedule).
func (c *Catalog) ScheduledShows(ctx context.Context, shows []core.RatingKey, from, until time.Time, limit int) ([]ScheduleEntry, error) {
	if len(shows) == 0 || !until.After(from) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5000
	}
	var items []core.Title
	for start := 0; start < len(shows); start += scheduleBatch {
		batch := shows[start:min(start+scheduleBatch, len(shows))]
		args := make([]any, 0, len(batch)+3)
		for _, k := range batch {
			args = append(args, int64(k))
		}
		args = append(args, from.Unix(), until.Unix(), limit)
		got, err := c.scheduleRows(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND season>0 AND adult=0 AND show_key IN (`+placeholders(len(batch))+`) AND released_ts>=? AND released_ts<? ORDER BY released_ts, key LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		items = append(items, got...)
	}
	entries, err := c.scheduleEntries(ctx, items)
	if err != nil {
		return nil, err
	}
	sizes, err := c.seasonSizes(ctx, entries)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].SeasonEpisodes = sizes[entries[i].Title.Key.Parent()]
	}
	return entries, nil
}

// seasonSizes reads the highest mirrored episode number of each season
// the entries belong to.
func (c *Catalog) seasonSizes(ctx context.Context, entries []ScheduleEntry) (map[core.RatingKey]int, error) {
	seen := map[core.RatingKey]bool{}
	var shows []any
	for _, e := range entries {
		if k := e.Title.ShowKey; k != 0 && !seen[k] {
			seen[k] = true
			shows = append(shows, int64(k))
		}
	}
	out := map[core.RatingKey]int{}
	for start := 0; start < len(shows); start += scheduleBatch {
		batch := shows[start:min(start+scheduleBatch, len(shows))]
		rows, err := c.db.QueryContext(ctx, `SELECT show_key, season, MAX(episode) FROM catalog_titles WHERE type='episode' AND season>0 AND show_key IN (`+placeholders(len(batch))+`) GROUP BY show_key, season`, batch...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var show int64
			var season, last int
			if err := rows.Scan(&show, &season, &last); err != nil {
				rows.Close()
				return nil, err
			}
			out[core.SeasonKey(core.RatingKey(show).TMDB(), season)] = last
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ScheduledPremieres returns announced seasons of shows premiering in
// [from, until) whose episodes are not mirrored yet, so a calendar can
// still mark the premiere.
func (c *Catalog) ScheduledPremieres(ctx context.Context, shows []core.RatingKey, from, until time.Time) ([]ScheduleEntry, error) {
	if len(shows) == 0 || !until.After(from) {
		return nil, nil
	}
	var items []core.Title
	for start := 0; start < len(shows); start += scheduleBatch {
		batch := shows[start:min(start+scheduleBatch, len(shows))]
		args := make([]any, 0, len(batch)+2)
		for _, k := range batch {
			args = append(args, int64(k))
		}
		args = append(args, from.Unix(), until.Unix())
		got, err := c.scheduleRows(ctx, `SELECT `+titleCols+` FROM catalog_titles s WHERE s.type='season' AND s.season>0 AND s.adult=0 AND s.show_key IN (`+placeholders(len(batch))+`) AND s.released_ts>=? AND s.released_ts<?
AND NOT EXISTS (SELECT 1 FROM catalog_titles e WHERE e.type='episode' AND e.show_key=s.show_key AND e.season=s.season)`, args...)
		if err != nil {
			return nil, err
		}
		items = append(items, got...)
	}
	return c.scheduleEntries(ctx, items)
}

// ScheduledMovies returns the given movies released in [from, until).
func (c *Catalog) ScheduledMovies(ctx context.Context, keys []core.RatingKey, from, until time.Time) ([]ScheduleEntry, error) {
	if len(keys) == 0 || !until.After(from) {
		return nil, nil
	}
	var items []core.Title
	for start := 0; start < len(keys); start += scheduleBatch {
		batch := keys[start:min(start+scheduleBatch, len(keys))]
		args := make([]any, 0, len(batch)+2)
		for _, k := range batch {
			args = append(args, int64(k))
		}
		args = append(args, from.Unix(), until.Unix())
		got, err := c.scheduleRows(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='movie' AND adult=0 AND key IN (`+placeholders(len(batch))+`) AND released_ts>=? AND released_ts<?`, args...)
		if err != nil {
			return nil, err
		}
		items = append(items, got...)
	}
	return c.scheduleEntries(ctx, items)
}

// PopularScheduledMovies returns the most popular mirrored movies released
// in [from, until), most popular first.
func (c *Catalog) PopularScheduledMovies(ctx context.Context, from, until time.Time, limit int) ([]ScheduleEntry, error) {
	if !until.After(from) || limit <= 0 {
		return nil, nil
	}
	items, err := c.scheduleRows(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='movie' AND adult=0 AND released_ts>=? AND released_ts<? AND popularity>0 ORDER BY popularity DESC, key LIMIT ?`, from.Unix(), until.Unix(), limit)
	if err != nil {
		return nil, err
	}
	return c.scheduleEntries(ctx, items)
}

func (c *Catalog) scheduleRows(ctx context.Context, q string, args ...any) ([]core.Title, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Title
	for rows.Next() {
		st, err := scanTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st.t)
	}
	return out, rows.Err()
}

func (c *Catalog) scheduleEntries(ctx context.Context, items []core.Title) ([]ScheduleEntry, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := c.hydrateSlice(ctx, items); err != nil {
		return nil, err
	}
	shows := map[core.RatingKey]*core.Title{}
	out := make([]ScheduleEntry, 0, len(items))
	for i := range items {
		t := items[i]
		if t.ShowKey != 0 {
			show, ok := shows[t.ShowKey]
			if !ok {
				if st, err := c.loadByKey(ctx, t.ShowKey); err == nil {
					show = &st.t
				}
				shows[t.ShowKey] = show
			}
			if show != nil {
				if t.ContentRating == "" {
					t.ContentRating = show.ContentRating
				}
				if show.Adult {
					t.Adult = true
				}
				if t.ShowTitle == "" {
					t.ShowTitle = show.Title
				}
				if t.BackdropPath == "" {
					t.BackdropPath = show.BackdropPath
				}
			}
		}
		if t.Adult {
			continue
		}
		e := ScheduleEntry{Title: t, Playable: c.visibleTitle(&t)}
		if show := shows[t.ShowKey]; show != nil {
			e.ShowPoster = show.PosterPath
		}
		out = append(out, e)
	}
	return out, nil
}

// scheduleQuietAfter is how long after its last episode an ended or
// cancelled show stops being refreshed for the calendar.
const scheduleQuietAfter = 60 * 24 * time.Hour

// RefreshSchedule mirrors a followed show's current and announced seasons,
// so the calendar can list their episodes without contacting TMDB on its
// request path. Fresh shows and seasons cost no request (Get's TTLs apply:
// an hour for a returning series' newest season); a settled show is left
// alone.
func (c *Catalog) RefreshSchedule(ctx context.Context, show core.RatingKey) error {
	if show.Kind() != core.KindShow {
		return nil
	}
	t, err := c.getShow(ctx, show.TMDB())
	if err != nil {
		return err
	}
	if t.Adult {
		return nil
	}
	ended := strings.EqualFold(t.Status, "Ended") || strings.EqualFold(t.Status, "Canceled")
	if ended && !t.LastAirDate.IsZero() && c.now().Sub(t.LastAirDate) > scheduleQuietAfter {
		return nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT season, released_ts FROM catalog_titles WHERE type='season' AND show_key=? AND season>0 ORDER BY season DESC LIMIT 2`, int64(show))
	if err != nil {
		return err
	}
	type season struct {
		n        int
		released int64
	}
	var seasons []season
	for rows.Next() {
		var s season
		if err := rows.Scan(&s.n, &s.released); err != nil {
			rows.Close()
			return err
		}
		seasons = append(seasons, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seasons) == 0 {
		return nil
	}
	// The newest season always; the one before it too while the newest has
	// not premiered, because that is the season still airing.
	want := []int{seasons[0].n}
	if len(seasons) > 1 && (seasons[0].released == 0 || seasons[0].released > c.now().Unix()) {
		want = append(want, seasons[1].n)
	}
	for _, n := range want {
		if !episodeKeyFits(n, 0) {
			continue
		}
		if _, err := c.fetchSeason(ctx, show.TMDB(), n); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
