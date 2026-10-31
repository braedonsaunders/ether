package catalog

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// ReleaseMatches joins a feed page to local metadata in one narrow scan. It
// never spends a metadata API call per feed item. Ambiguous names are ignored;
// explicit identifier conflicts always defeat a textual match. Season packs are
// expanded only to locally known episodes, never to invented episode identities.
func (c *Catalog) ReleaseMatches(ctx context.Context, releases []core.Release) (map[string][]core.Title, error) {
	byName := map[string][]int{}
	byID := map[string][]int{}
	for i, r := range releases {
		n := releaseparse.NormalizeTitle(r.Info.Title)
		if n != "" {
			byName[n] = append(byName[n], i)
		}
		for _, id := range feedIDs(r.TitleIDs) {
			byID[id] = append(byID[id], i)
		}
	}
	rows, err := c.db.QueryContext(ctx, `SELECT key,type,tmdb_id,imdb,tvdb,title,original_title,year FROM catalog_titles WHERE type IN ('movie','show') AND adult=0`)
	if err != nil {
		return nil, err
	}
	matches := map[int][]core.RatingKey{}
	for rows.Next() {
		var t core.Title
		if err = rows.Scan(&t.Key, &t.Type, &t.IDs.TMDB, &t.IDs.IMDB, &t.IDs.TVDB, &t.Title, &t.OriginalTitle, &t.Year); err != nil {
			rows.Close()
			return nil, err
		}
		indices := map[int]bool{}
		for _, n := range []string{t.Title, t.OriginalTitle} {
			for _, i := range byName[releaseparse.NormalizeTitle(n)] {
				indices[i] = true
			}
		}
		for _, id := range feedIDs(t.IDs) {
			for _, i := range byID[id] {
				indices[i] = true
			}
		}
		for i := range indices {
			r := releases[i]
			tv := len(r.Info.Seasons) > 0 || len(r.Info.Episodes) > 0 || r.Info.FullSeason || r.Category/1000 == 5
			if (t.Type == core.TypeShow) != tv {
				continue
			}
			same, conflict := releaseparse.CompareTitleIDs(r.TitleIDs, t.IDs)
			if conflict || (!same && (!releaseparse.YearMatches(r.Info.Year, t.Year) || !(releaseparse.TitleMatches(r.Info, t.Title) || releaseparse.TitleMatches(r.Info, t.OriginalTitle)))) {
				continue
			}
			// A film without either a reported identifier or a year is too ambiguous.
			if !same && t.Type == core.TypeMovie && r.Info.Year == 0 {
				continue
			}
			matches[i] = append(matches[i], t.Key)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := map[string][]core.Title{}
	for i, keys := range matches {
		if len(keys) != 1 {
			continue
		}
		r := releases[i]
		if keys[0].Kind() == core.KindMovie {
			if t, e := c.CachedTitle(ctx, keys[0]); e == nil && t.IsReleased(c.now()) {
				out[r.ID] = []core.Title{*t}
			}
			continue
		}
		eps, e := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE show_key=? AND type='episode' AND adult=0 AND release_known=1 AND released_ts<=? ORDER BY season,episode LIMIT 2000`, int64(keys[0]), c.now().Unix())
		if e != nil {
			return nil, e
		}
		for _, t := range eps {
			season := false
			for _, s := range r.Info.Seasons {
				if s == t.Season {
					season = true
				}
			}
			if releaseparse.EpisodeMatches(r.Info, t.Season, t.Episode) || (r.Info.FullSeason && season) {
				out[r.ID] = append(out[r.ID], t)
			}
		}
	}
	return out, nil
}
func feedIDs(ids core.IDs) []string {
	var out []string
	if ids.TMDB > 0 {
		out = append(out, fmt.Sprintf("tmdb:%d", ids.TMDB))
	}
	if ids.TVDB > 0 {
		out = append(out, fmt.Sprintf("tvdb:%d", ids.TVDB))
	}
	if ids.IMDB != "" {
		out = append(out, "imdb:"+ids.IMDB)
	}
	return out
}
