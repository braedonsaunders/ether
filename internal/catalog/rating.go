package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
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

func encodeCertifications(certs map[string]string) string {
	if len(certs) == 0 {
		return ""
	}
	clean := make(map[string]string, len(certs))
	for k, v := range certs {
		cc := strings.ToUpper(strings.TrimSpace(k))
		lbl := strings.TrimSpace(v)
		if contentrating.Alpha2(cc) == "" || lbl == "" {
			continue
		}
		clean[cc] = lbl
	}
	if len(clean) == 0 {
		return ""
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeCertifications(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// Content ratings. catalog_titles keeps the chosen certification in
// contentrating's stored form (content_rating), its age Level
// (content_rating_rank, 0 = unrated), where it came from
// (content_rating_source), and its US-scale enforcement_rating. Precedence,
// highest first:
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

// enforcementPrecedence is the ON CONFLICT expression for enforcement_rating.
// A summary upsert without certifications or keywords preserves any detailed
// enforcement_rating (including UncertifiedAdult "XXX") already stored.
func enforcementPrecedence() string {
	return `CASE WHEN ?=1 THEN excluded.enforcement_rating
 WHEN catalog_titles.content_rating_source IN ('omdb','mdblist') AND catalog_titles.content_rating_rank>0 THEN catalog_titles.enforcement_rating
 WHEN excluded.detail=1 OR excluded.enforcement_rating<>'' THEN excluded.enforcement_rating
 ELSE catalog_titles.enforcement_rating END`
}

func titleRatingRank(contentRating, enforcementRating string) int {
	if enforcementRating == contentrating.UncertifiedAdult {
		return int(contentrating.Adult)
	}
	return max(int(contentrating.LevelOf(contentRating)), int(contentrating.EnforceLevelOf(enforcementRating)))
}

var (
	strongAdultSQLList  = sqlQuotedStrings(contentrating.StrongAdultKeywords())
	leaningAdultSQLList = sqlQuotedStrings(contentrating.LeaningAdultKeywords())
)

func sqlQuotedStrings(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return strings.Join(quoted, ",")
}

// adultKeywordsExistsSQL probes catalog_keywords by primary key for a single
// title keyExpr to detect strong or multi-hit adult-leaning keywords before
// lazy refresh has populated enforcement_rating.
func adultKeywordsExistsSQL(keyExpr string) string {
	return `(EXISTS (SELECT 1 FROM catalog_keywords ak WHERE ak.key=` + keyExpr + ` AND LOWER(ak.keyword) IN (` + strongAdultSQLList + `))` +
		` OR (SELECT COUNT(DISTINCT LOWER(ak.keyword)) FROM catalog_keywords ak WHERE ak.key=` + keyExpr + ` AND LOWER(ak.keyword) IN (` + leaningAdultSQLList + `))>=2)`
}

// sqlBlockUncertifiedAdult excludes uncertified adult/soft-core titles
// (persisted 'XXX' or pre-refresh uncertified adult-keyword rows) whenever a
// viewer has any content-rating filter active.
func sqlBlockUncertifiedAdult(titleAlias string) string {
	return titleAlias + `.enforcement_rating<>'` + contentrating.UncertifiedAdult + `' AND NOT (` +
		titleAlias + `.content_rating_rank=0 AND ` + titleAlias + `.enforcement_rating='' AND ` + adultKeywordsExistsSQL(titleAlias+`.key`) + `)`
}

// sqlMatchRatings builds a SQL predicate matching values (PlexShare or browse
// ContentRatings / ExcludeContentRatings) against ratingExpr (normalized
// lowercase trimmed content_rating) and rankExpr (content_rating_rank).
// Foreign certifications and display-unrated enforcement overrides match their
// mapped US enforcement band without conflating US MPAA ("PG") and US TV ("TV-PG").
func sqlMatchRatings(ratingExpr, rankExpr string, values []string) (string, []any) {
	unratedExpr := `(` + ratingExpr + `='' OR REPLACE(REPLACE(REPLACE(REPLACE(` + ratingExpr + `,' ',''),'-',''),'_',''),'.','') IN ('nr','notrated','unrated'))`
	var parts []string
	var vals []any
	for _, value := range values {
		v := strings.TrimSpace(value)
		if strings.EqualFold(v, "NR") || strings.EqualFold(v, "None") {
			parts = append(parts, `(`+unratedExpr+` AND `+rankExpr+`=0)`)
			continue
		}
		low := strings.ToLower(v)
		clause := ratingExpr + `=?`
		vals = append(vals, low)
		if contentrating.Parse(v).Country == "US" {
			if lvl := contentrating.EnforceLevelOf(v); lvl != contentrating.Unrated {
				if strings.EqualFold(v, "TV-MA") {
					// US TV Parental Guidelines top out at TV-MA (no NC-17 tier),
					// so foreign 18+/19 TV certifications also match TV-MA.
					clause = `(` + clause + ` OR ((` + ratingExpr + ` LIKE '%/%' OR ` + unratedExpr + `) AND ` + rankExpr + ` IN (?,?))`
					vals = append(vals, int(contentrating.Mature), int(contentrating.Adult))
					overrides := append([]string(nil), contentrating.OverrideLabelsForLevel(contentrating.Mature)...)
					overrides = append(overrides, contentrating.OverrideLabelsForLevel(contentrating.Adult)...)
					if len(overrides) > 0 {
						clause += ` OR ` + ratingExpr + ` IN (` + sqlQuotedStrings(overrides) + `)`
					}
					clause += `)`
				} else {
					clause = `(` + clause + ` OR ((` + ratingExpr + ` LIKE '%/%' OR ` + unratedExpr + `) AND ` + rankExpr + `=?)`
					vals = append(vals, int(lvl))
					if overrides := contentrating.OverrideLabelsForLevel(lvl); len(overrides) > 0 {
						clause += ` OR ` + ratingExpr + ` IN (` + sqlQuotedStrings(overrides) + `)`
					}
					clause += `)`
				}
			}
		}
		parts = append(parts, clause)
	}
	if len(parts) == 0 {
		return "1=0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", vals
}

// ratingModel versions the stored levels and selection. Raising it re-ranks
// every row from its label at startup and re-selects every detailed movie
// and show from TMDB in the background (RefreshRatings).
const (
	// ratingLevels versions content_rating_rank's numbering (contentrating
	// levels); raising it re-ranks every row at startup without a sweep.
	ratingLevels       = "3"
	ratingModel        = "1"
	ratingModelKey     = "content_rating_model"
	ratingRankedKey    = "content_rating_ranked"
	ratingSweepKey     = "content_rating_sweep"
	ratingSweepPerTick = 300
)

// rankRatings recomputes content_rating_rank from content_rating and
// enforcement_rating for every row, once per level numbering (ratingLevels).
func (c *Catalog) rankRatings(ctx context.Context) error {
	if c.getMeta(ctx, ratingRankedKey) == ratingLevels {
		return nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT content_rating, enforcement_rating FROM catalog_titles WHERE content_rating<>'' OR enforcement_rating<>''`)
	if err != nil {
		return err
	}
	type pair struct{ display, enf string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.display, &p.enf); err != nil {
			rows.Close()
			return err
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pairs) == 0 {
		return c.setMeta(ctx, ratingRankedKey, ratingLevels)
	}
	q := `UPDATE catalog_titles SET content_rating_rank=CASE`
	args := make([]any, 0, 3*len(pairs))
	for _, p := range pairs {
		q += ` WHEN content_rating=? AND enforcement_rating=? THEN ?`
		args = append(args, p.display, p.enf, titleRatingRank(p.display, p.enf))
	}
	q += ` ELSE 0 END`
	if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return c.setMeta(ctx, ratingRankedKey, ratingLevels)
}

// childRatingsKey records the last series key whose unrated seasons and
// episodes took its rating; "done" once every series is covered.
const (
	childRatingsKey   = "child_ratings_backfill"
	childRatingsBatch = 500
)

// backfillChildRatings gives unrated seasons and episodes their series'
// rating, a batch of series at a time in key order, resuming from the last
// batch after a restart. Writes now store it; this covers rows stored before.
func (c *Catalog) backfillChildRatings(ctx context.Context) error {
	cursor := c.getMeta(ctx, childRatingsKey)
	if cursor == "done" {
		return nil
	}
	after, _ := strconv.ParseInt(cursor, 10, 64)
	for ctx.Err() == nil {
		n, last, err := c.childRatingsBatch(ctx, after)
		if err != nil {
			return err
		}
		if n == 0 {
			return c.setMeta(ctx, childRatingsKey, "done")
		}
		after = last
		if err := c.setMeta(ctx, childRatingsKey, strconv.FormatInt(after, 10)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// childRatingsBatch covers the next series after key after: it returns how
// many series it read and the last key.
func (c *Catalog) childRatingsBatch(ctx context.Context, after int64) (int, int64, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT key,content_rating,content_rating_source,enforcement_rating,certifications FROM catalog_titles
		WHERE type='show' AND key>? ORDER BY key ASC LIMIT ?`, after, childRatingsBatch)
	if err != nil {
		return 0, 0, err
	}
	type series struct {
		key                     int64
		rating, src, enf, certs string
	}
	var batch []series
	for rows.Next() {
		var s series
		if err := rows.Scan(&s.key, &s.rating, &s.src, &s.enf, &s.certs); err != nil {
			rows.Close()
			return 0, 0, err
		}
		batch = append(batch, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(batch) == 0 {
		return 0, 0, err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var changed int64
	for _, s := range batch {
		if s.rating == "" && s.enf == "" && s.certs == "" {
			continue
		}
		enf := s.enf
		if enf == "" {
			enf = contentrating.EnforceRating(s.rating)
		}
		res, err := tx.ExecContext(ctx, `UPDATE catalog_titles SET content_rating=?,content_rating_rank=?,content_rating_source=?,enforcement_rating=?,certifications=CASE WHEN ?<>'' THEN ? ELSE certifications END
			WHERE show_key=? AND type IN ('season','episode') AND content_rating=''`, s.rating, titleRatingRank(s.rating, enf), s.src, enf, s.certs, s.certs, s.key)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		changed += n
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if changed > 0 {
		c.writeGen.Add(1)
	}
	return len(batch), batch[len(batch)-1].key, nil
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
	if !ok || !c.TMDBConfigured() || limit <= 0 || c.getMeta(ctx, ratingModelKey) == ratingModel {
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
		enf        string
		certs      map[string]string
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
				results[i] = result{sel: contentrating.Select(certs, region), enf: contentrating.EnforceCertifications(certs, region), certs: contentrating.ByCountry(certs), done: true}
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
			if err := c.storeRating(ctx, core.RatingKey(it.key), it.typ == string(core.TypeShow), r.sel.Value, r.enf, r.sel.Source, r.certs, homeRating(r.sel.Value, r.sel.Source, region)); err != nil {
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
func (c *Catalog) storeRating(ctx context.Context, key core.RatingKey, show bool, value, enf, source string, certs map[string]string, home bool) error {
	if enf == "" {
		enf = contentrating.EnforceRating(value)
	}
	rank := titleRatingRank(value, enf)
	certsJSON := encodeCertifications(certs)
	enfExpr := `?`
	if enf == "" {
		enfExpr = `CASE WHEN catalog_titles.enforcement_rating='` + contentrating.UncertifiedAdult + `' OR catalog_titles.adult<>0 OR (` + adultKeywordsExistsSQL("catalog_titles.key") + `) THEN '` + contentrating.UncertifiedAdult + `' ELSE '' END`
	}
	q := `UPDATE catalog_titles SET content_rating=?,content_rating_rank=?,content_rating_source=?,enforcement_rating=` + enfExpr + `,certifications=CASE WHEN ?<>'' THEN ? ELSE certifications END
		WHERE (key=?` + map[bool]string{true: ` OR (show_key=? AND type IN ('season','episode'))`, false: ``}[show] + `)
		AND (?=1 OR NOT (content_rating_source IN ('omdb','mdblist') AND content_rating_rank>0))
		AND (content_rating<>? OR content_rating_source<>? OR content_rating_rank<>? OR enforcement_rating<>(` + enfExpr + `) OR (?<>'' AND certifications<>?))`
	args := []any{value, rank, source}
	if enf != "" {
		args = append(args, enf)
	}
	args = append(args, certsJSON, certsJSON, int64(key))
	if show {
		args = append(args, int64(key))
	}
	homeInt := 0
	if home {
		homeInt = 1
	}
	args = append(args, homeInt, value, source, rank)
	if enf != "" {
		args = append(args, enf)
	}
	args = append(args, certsJSON, certsJSON)
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
	var value, source, certsRaw string
	err := c.db.QueryRowContext(ctx, `SELECT content_rating,content_rating_source,certifications FROM catalog_titles WHERE key=?`, int64(key)).Scan(&value, &source, &certsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if homeRating(value, source, c.config().Region) || (source == "" && value != "" && contentrating.Parse(value).Country == "US" && contentrating.LevelOf(value) != contentrating.Unrated) {
		return nil
	}
	certs := decodeCertifications(certsRaw)
	if certs == nil {
		certs = map[string]string{}
	}
	certs["US"] = label
	qualified := contentrating.Qualify("US", label)
	return c.storeRating(ctx, key, key.Kind() == core.KindShow, qualified, contentrating.EnforceRating(qualified), provider, certs, true)
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
