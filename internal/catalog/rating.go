package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/schedule"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// Content ratings. catalog_titles keeps the chosen certification in
// contentrating's stored form (content_rating), its age Level
// (content_rating_rank, 0 = unrated) and where it came from
// (content_rating_source). Precedence, highest first:
//
//  1. TMDB's rating for the owner's region or the US ("home");
//  2. an owner-configured provider (OMDb or MDBList: IMDb's US certificate),
//     which is what Plex Media Server falls back to;
//  3. TMDB's rating from another country, kept region-qualified;
//  4. an explicit "not rated", then nothing.

// providerSources are rating sources outside TMDB.
var providerSources = []string{"omdb", "mdblist"}

func isProviderSource(src string) bool {
	for _, p := range providerSources {
		if src == p {
			return true
		}
	}
	return false
}

// homeRating reports whether a rating is TMDB's for the region or the US.
func homeRating(value, source, region string) bool {
	if contentrating.LevelOf(value) == contentrating.Unrated {
		return false
	}
	region = strings.ToUpper(strings.TrimSpace(region))
	for _, c := range []string{"US", region} {
		if c != "" && (source == "tmdb:"+c || strings.HasPrefix(source, "tmdb:"+c+":")) {
			return true
		}
	}
	return false
}

// ratingPrecedence is the ON CONFLICT expression for one rating column. It
// takes one parameter: 1 when the incoming rating is a home rating.
func ratingPrecedence(col string) string {
	return `CASE WHEN ?=1 THEN excluded.` + col + `
 WHEN catalog_titles.content_rating_source IN ('omdb','mdblist') AND catalog_titles.content_rating_rank>0 THEN catalog_titles.` + col + `
 WHEN excluded.detail=1 OR excluded.content_rating<>'' THEN excluded.` + col + `
 ELSE catalog_titles.` + col + ` END`
}

// ratingModel versions the stored levels and selection. Raising it re-ranks
// every row from its label at startup and re-selects every detailed movie
// and show from TMDB in the background (RefreshRatings).
const (
	ratingModel        = "1"
	ratingModelKey     = "content_rating_model"
	ratingRankedKey    = "content_rating_ranked"
	ratingSweepKey     = "content_rating_sweep"
	ratingSweepPerTick = 300
)

// rankRatings recomputes content_rating_rank from content_rating for every
// row, once per rating model. Labels are few (a few hundred distinct), so
// it is one UPDATE with a CASE over them.
func (c *Catalog) rankRatings(ctx context.Context) error {
	if c.getMeta(ctx, ratingRankedKey) == ratingModel {
		return nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT content_rating FROM catalog_titles WHERE content_rating<>''`)
	if err != nil {
		return err
	}
	var labels []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		labels = append(labels, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(labels) == 0 {
		return c.setMeta(ctx, ratingRankedKey, ratingModel)
	}
	q := `UPDATE catalog_titles SET content_rating_rank=CASE content_rating`
	args := make([]any, 0, 2*len(labels))
	for _, v := range labels {
		q += ` WHEN ? THEN ?`
		args = append(args, v, int(contentrating.LevelOf(v)))
	}
	q += ` ELSE 0 END`
	if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return c.setMeta(ctx, ratingRankedKey, ratingModel)
}

// ratingsTMDB is the light TMDB surface for rating re-selection.
// *tmdb.Client satisfies it.
type ratingsTMDB interface {
	MovieReleaseDates(ctx context.Context, id int64) (*tmdb.ReleaseDates, error)
	TVContentRatings(ctx context.Context, id int64) (*tmdb.ContentRatings, error)
}

func movieCertifications(rd *tmdb.ReleaseDates) []contentrating.Certification {
	if rd == nil {
		return nil
	}
	var out []contentrating.Certification
	for _, r := range rd.Results {
		for _, e := range r.ReleaseDate {
			out = append(out, contentrating.Certification{Country: r.ISO3166, Label: e.Certification, ReleaseType: e.Type})
		}
	}
	return out
}

func tvCertifications(cr *tmdb.ContentRatings) []contentrating.Certification {
	if cr == nil {
		return nil
	}
	out := make([]contentrating.Certification, 0, len(cr.Results))
	for _, r := range cr.Results {
		out = append(out, contentrating.Certification{Country: r.ISO3166, Label: r.Rating})
	}
	return out
}

// RefreshRatings re-selects the stored rating of up to limit detailed
// movies and shows from TMDB, walking the catalogue by key until the
// current rating model has covered every title. It is a no-op once done.
// It returns how many titles it checked.
func (c *Catalog) RefreshRatings(ctx context.Context, limit int) (int, error) {
	src, ok := c.tmdb().(ratingsTMDB)
	if !ok || limit <= 0 || c.getMeta(ctx, ratingModelKey) == ratingModel {
		return 0, nil
	}
	if err := c.rankRatings(ctx); err != nil {
		return 0, err
	}
	// The cursor is "model:key", so a new model starts from the beginning.
	var cursor int64
	if m, key, ok := strings.Cut(c.getMeta(ctx, ratingSweepKey), ":"); ok && m == ratingModel {
		cursor, _ = strconv.ParseInt(key, 10, 64)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT key,type,tmdb_id FROM catalog_titles
		WHERE key>? AND type IN ('movie','show') AND detail=1 AND tmdb_id>0
		ORDER BY key ASC LIMIT ?`, cursor, limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		key  int64
		typ  string
		tmdb int64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.key, &it.typ, &it.tmdb); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, c.setMeta(ctx, ratingModelKey, ratingModel)
	}
	region := c.config().Region
	type result struct {
		sel        contentrating.Selection
		done, gone bool // fetched; or TMDB no longer has the title
	}
	results := make([]result, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var certs []contentrating.Certification
			var err error
			if it.typ == string(core.TypeMovie) {
				var rd *tmdb.ReleaseDates
				rd, err = src.MovieReleaseDates(ctx, it.tmdb)
				certs = movieCertifications(rd)
			} else {
				var cr *tmdb.ContentRatings
				cr, err = src.TVContentRatings(ctx, it.tmdb)
				certs = tvCertifications(cr)
			}
			switch {
			case errors.Is(err, core.ErrNotFound):
				results[i] = result{done: true, gone: true}
			case err == nil:
				results[i] = result{sel: contentrating.Select(certs, region), done: true}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	done := 0
	for i, it := range items {
		r := results[i]
		if !r.done {
			break // retry this title and the rest next tick
		}
		if !r.gone {
			if err := c.storeRating(ctx, core.RatingKey(it.key), it.typ == string(core.TypeShow), r.sel.Value, r.sel.Source, homeRating(r.sel.Value, r.sel.Source, region)); err != nil {
				return done, err
			}
		}
		cursor = it.key
		done++
	}
	if done == 0 {
		return 0, nil
	}
	return done, c.setMeta(ctx, ratingSweepKey, ratingModel+":"+strconv.FormatInt(cursor, 10))
}

// refreshRatingsLoop runs RefreshRatings every 90 seconds until the current
// rating model has covered the catalogue.
func (c *Catalog) refreshRatingsLoop(ctx context.Context) {
	for ctx.Err() == nil && c.getMeta(ctx, ratingModelKey) != ratingModel {
		if _, err := c.RefreshRatings(ctx, ratingSweepPerTick); err != nil && ctx.Err() == nil {
			c.log.Warn("catalog: content rating refresh", "err", err)
		}
		if !schedule.Wait(ctx, c.now, 90*time.Second) {
			return
		}
	}
}

// storeRating writes a selected rating to a title (and, for a show, its
// seasons and episodes) with the upsert's precedence: a home rating always
// wins; otherwise a provider rating already stored is kept.
func (c *Catalog) storeRating(ctx context.Context, key core.RatingKey, show bool, value, source string, home bool) error {
	q := `UPDATE catalog_titles SET content_rating=?,content_rating_rank=?,content_rating_source=?
		WHERE (key=?` + map[bool]string{true: ` OR (show_key=? AND type IN ('season','episode'))`, false: ``}[show] + `)
		AND (?=1 OR NOT (content_rating_source IN ('omdb','mdblist') AND content_rating_rank>0))
		AND (content_rating<>? OR content_rating_source<>?)`
	args := []any{value, int(contentrating.LevelOf(value)), source, int64(key)}
	if show {
		args = append(args, int64(key))
	}
	homeInt := 0
	if home {
		homeInt = 1
	}
	args = append(args, homeInt, value, source)
	res, err := c.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		c.writeGen.Add(1)
	}
	return nil
}

// ApplyProviderCertification records a certification an owner-configured
// rating provider (OMDb, MDBList) reports for a movie or show: IMDb's US
// certificate. It fills titles TMDB rates only abroad, "not rated" or not
// at all, and never replaces TMDB's rating for the region or the US.
func (c *Catalog) ApplyProviderCertification(ctx context.Context, key core.RatingKey, label, provider string) error {
	label = strings.TrimSpace(label)
	if !isProviderSource(provider) || contentrating.Lookup("US", label) == contentrating.Unrated {
		return nil
	}
	if key.Kind() != core.KindMovie && key.Kind() != core.KindShow {
		return nil
	}
	var value, source string
	err := c.db.QueryRowContext(ctx, `SELECT content_rating,content_rating_source FROM catalog_titles WHERE key=?`, int64(key)).Scan(&value, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if homeRating(value, source, c.config().Region) || (source == "" && value != "" && contentrating.Parse(value).Country == "US" && contentrating.LevelOf(value) != contentrating.Unrated) {
		return nil
	}
	return c.storeRating(ctx, key, key.Kind() == core.KindShow, contentrating.Qualify("US", label), provider, true)
}

// UnratedTitles lists browsable, detailed movies and shows without a rated
// certification, by key after the given one, for a rating provider to look
// up. Titles carry only their key, type and ids.
func (c *Catalog) UnratedTitles(ctx context.Context, after core.RatingKey, limit int) ([]core.Title, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT key,type,tmdb_id,imdb FROM catalog_titles
		WHERE key>? AND type IN ('movie','show') AND browsable=1 AND adult=0 AND detail=1 AND content_rating_rank=0
		ORDER BY key ASC LIMIT ?`, int64(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Title
	for rows.Next() {
		var key int64
		var t core.Title
		if err := rows.Scan(&key, &t.Type, &t.IDs.TMDB, &t.IDs.IMDB); err != nil {
			return nil, err
		}
		t.Key = core.RatingKey(key)
		out = append(out, t)
	}
	return out, rows.Err()
}
