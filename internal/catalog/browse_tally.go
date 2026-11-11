package catalog

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A library page's total (Plex totalSize, Jellyfin TotalRecordCount) is the
// number of titles its filter admits. Counting them walks every browsable
// title of the type, so a cache miss on a large library cost seconds.
//
// catalog_browse_tally keeps that number instead, per (type, genre, content
// rating rank), for the titles every library listing starts from: movies and
// series that are browsable, not adult and have a known release. The empty
// genre row is every title of the type. A total is then a few tally rows, corrected over
// the titles where the listing's predicate and the tally's can disagree:
// those not yet released (the "released by now" cut moves with the clock),
// the availability hidden set (the evidence window) and the viewer's
// excluded keys. Each is a short index range, so the cost follows the
// releases of the coming days and the evidence window, never the library.
//
// Writers append signed deltas to catalog_browse_tally_delta from triggers
// rather than update the few tally rows in place: every title write would
// otherwise queue on the same rows (PostgreSQL row locks held to commit),
// as catalog_genre_pairs avoids with per-title witnesses. A read sums the
// tally and the pending deltas in one statement; the catalogue's background
// loop folds deltas into the tally. Reads are exact either way.
//
// The tally depends only on stored columns, never on the clock, the viewer
// or settings, so it needs no expiry: the triggers keep it exact in the same
// transaction as the write.
// tallyMemberSQL is a title's tally membership. alias is "NEW." or "OLD."
// in triggers and "t." in queries. release_known is generated from
// released_ts<>0; triggers test the stored column itself.
func tallyMemberSQL(a string) string {
	return a + "type IN ('movie','show') AND " + a + "browsable=1 AND " + a + "adult=0 AND " + a + "released_ts<>0"
}

var tallyBackfill = `
INSERT INTO catalog_browse_tally(type,genre,rating_rank,n)
 SELECT t.type,'',t.content_rating_rank,COUNT(*) FROM catalog_titles t WHERE ` + tallyMemberSQL("t.") + ` GROUP BY t.type,t.content_rating_rank;
INSERT INTO catalog_browse_tally(type,genre,rating_rank,n)
 SELECT t.type,g.genre,t.content_rating_rank,COUNT(*) FROM catalog_genres g JOIN catalog_titles t ON t.key=g.key WHERE g.genre<>'' AND ` + tallyMemberSQL("t.") + ` GROUP BY t.type,g.genre,t.content_rating_rank;`

func init() {
	member := tallyMemberSQL
	// Title rows contribute to the '' row and to one row per stored genre;
	// genre rows contribute while their title is a member.
	titleDelta := func(alias, sign string) string {
		return `INSERT INTO catalog_browse_tally_delta(type,genre,rating_rank,d) SELECT ` + alias + `type,'',` + alias + `content_rating_rank,` + sign + ` WHERE ` + member(alias) + `;
 INSERT INTO catalog_browse_tally_delta(type,genre,rating_rank,d) SELECT ` + alias + `type,genre,` + alias + `content_rating_rank,` + sign + ` FROM catalog_genres WHERE key=` + alias + `key AND genre<>'' AND ` + member(alias) + `;`
	}
	genreDelta := func(alias, sign string) string {
		return `INSERT INTO catalog_browse_tally_delta(type,genre,rating_rank,d) SELECT t.type,` + alias + `genre,t.content_rating_rank,` + sign + ` FROM catalog_titles t WHERE t.key=` + alias + `key AND ` + alias + `genre<>'' AND ` + member("t.") + `;`
	}
	changed := `OLD.key IS NOT NEW.key OR OLD.type IS NOT NEW.type OR OLD.browsable IS NOT NEW.browsable OR OLD.adult IS NOT NEW.adult OR (OLD.released_ts<>0) IS NOT (NEW.released_ts<>0) OR OLD.content_rating_rank IS NOT NEW.content_rating_rank`
	schema := `CREATE TABLE catalog_browse_tally(type TEXT NOT NULL, genre TEXT NOT NULL, rating_rank INTEGER NOT NULL, n INTEGER NOT NULL, PRIMARY KEY(type,genre,rating_rank));
CREATE TABLE catalog_browse_tally_delta(id INTEGER PRIMARY KEY, type TEXT NOT NULL, genre TEXT NOT NULL, rating_rank INTEGER NOT NULL, d INTEGER NOT NULL);
CREATE INDEX catalog_browse_tally_delta_dims ON catalog_browse_tally_delta(type,genre,rating_rank);` + tallyBackfill
	store.Register("catalog", 44, schema+`
CREATE TRIGGER catalog_browse_tally_title_insert AFTER INSERT ON catalog_titles BEGIN `+titleDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_browse_tally_title_delete AFTER DELETE ON catalog_titles BEGIN `+titleDelta("OLD.", "-1")+` END;
CREATE TRIGGER catalog_browse_tally_title_update AFTER UPDATE OF key,type,browsable,adult,released_ts,content_rating_rank ON catalog_titles
WHEN `+changed+` BEGIN `+titleDelta("OLD.", "-1")+` `+titleDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_browse_tally_genre_insert AFTER INSERT ON catalog_genres BEGIN `+genreDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_browse_tally_genre_delete AFTER DELETE ON catalog_genres BEGIN `+genreDelta("OLD.", "-1")+` END;
CREATE TRIGGER catalog_browse_tally_genre_update AFTER UPDATE ON catalog_genres BEGIN `+genreDelta("OLD.", "-1")+` `+genreDelta("NEW.", "1")+` END;`)
	// PostgreSQL runs row triggers at the end of the statement under READ
	// COMMITTED, so a genre write and a concurrent write to its title could
	// each miss the other's uncommitted row. A genre trigger first takes a
	// share lock on its title: it waits for a title write in flight and
	// then reads the committed row, and a later title write waits for the
	// genre write's commit before its own trigger reads the genres. The
	// migration excludes old-version writers until the backfill and the
	// triggers are visible together.
	pgChanged := pgDistinct(changed)
	store.RegisterDialect("catalog", 44, store.Postgres, `LOCK TABLE catalog_titles, catalog_genres IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE catalog_browse_tally(type TEXT NOT NULL, genre TEXT NOT NULL, rating_rank BIGINT NOT NULL, n BIGINT NOT NULL, PRIMARY KEY(type,genre,rating_rank));
CREATE TABLE catalog_browse_tally_delta(id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, type TEXT NOT NULL, genre TEXT NOT NULL, rating_rank BIGINT NOT NULL, d BIGINT NOT NULL);
CREATE INDEX catalog_browse_tally_delta_dims ON catalog_browse_tally_delta(type,genre,rating_rank);`+tallyBackfill+`
CREATE FUNCTION catalog_browse_tally_title() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF TG_OP<>'INSERT' THEN `+titleDelta("OLD.", "-1")+` END IF;
IF TG_OP<>'DELETE' THEN `+titleDelta("NEW.", "1")+` END IF;
RETURN NULL; END $$;
CREATE FUNCTION catalog_browse_tally_genre() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF TG_OP<>'INSERT' THEN PERFORM 1 FROM catalog_titles WHERE key=OLD.key FOR SHARE; `+genreDelta("OLD.", "-1")+` END IF;
IF TG_OP<>'DELETE' THEN PERFORM 1 FROM catalog_titles WHERE key=NEW.key FOR SHARE; `+genreDelta("NEW.", "1")+` END IF;
RETURN NULL; END $$;
CREATE TRIGGER catalog_browse_tally_title AFTER INSERT OR DELETE ON catalog_titles FOR EACH ROW EXECUTE FUNCTION catalog_browse_tally_title();
CREATE TRIGGER catalog_browse_tally_title_update AFTER UPDATE OF key,type,browsable,adult,released_ts,content_rating_rank ON catalog_titles
 FOR EACH ROW WHEN (`+pgChanged+`) EXECUTE FUNCTION catalog_browse_tally_title();
CREATE TRIGGER catalog_browse_tally_genre AFTER INSERT OR DELETE OR UPDATE ON catalog_genres FOR EACH ROW EXECUTE FUNCTION catalog_browse_tally_genre();`)
	// A logical restore writes the tally verbatim with triggers suspended,
	// or not at all from an older dump; either way it is rebuilt from the
	// restored titles before commit.
	store.RegisterRestore("catalog_browse_tally", func(context.Context, *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return func(ctx context.Context, tx *sql.Tx) error { return rebuildBrowseTally(ctx, tx) }, nil
	}, "catalog_browse_tally", "catalog_browse_tally_delta")
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func rebuildBrowseTally(ctx context.Context, db execer) error {
	for _, q := range []string{`DELETE FROM catalog_browse_tally_delta`, `DELETE FROM catalog_browse_tally`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, q := range strings.Split(strings.TrimSpace(tallyBackfill), ";") {
		if q = strings.TrimSpace(q); q == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// RebuildLibraryCounts recounts the trigger-kept library totals (the browse
// and dashboard tallies and the series' episode rating histograms) from the
// stored titles, and the vocabularies' facet identifiers (facet_ids.go),
// for a bulk load that set the catalogue's triggers aside (the performance
// fixture).
func RebuildLibraryCounts(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := rebuildBrowseTally(ctx, tx); err != nil {
		return err
	}
	if err := rebuildStatsTally(ctx, tx); err != nil {
		return err
	}
	if err := rebuildFacetTally(ctx, tx); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM catalog_show_episode_ratings`, showEpisodeRatingsBackfill} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if err := rebuildFacetIDs(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// pgDistinct spells a trigger WHEN clause's null-safe comparisons for
// PostgreSQL.
func pgDistinct(sqlite string) string {
	return strings.ReplaceAll(sqlite, " IS NOT ", " IS DISTINCT FROM ")
}

// tallyFoldChunk bounds one fold transaction, so it never holds the write
// lock for long after a bulk ingestion.
const tallyFoldChunk = 20000

// libraryTally is a trigger-kept count table folded by the background
// loop: name holds the counts, name+"_delta" the pending signed deltas, and
// dims is its key.
type libraryTally struct{ name, dims string }

var libraryTallies = []libraryTally{
	{"catalog_browse_tally", "type,genre,rating_rank"},
	{"catalog_stats_tally", "kind,browsable,home,year_pending"},
	{"catalog_facet_tally", "type,facet,value,rating_rank"},
}

// foldLibraryTallies moves every tally's pending deltas into it, one
// bounded chunk per transaction, and reports how many it moved. A
// concurrent read sees the deltas before or after the fold, never both or
// neither.
func (c *Catalog) foldLibraryTallies(ctx context.Context) (int64, error) {
	var moved int64
	for _, tally := range libraryTallies {
		for ctx.Err() == nil {
			var pending int
			err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM `+tally.name+`_delta LIMIT 1) p`).Scan(&pending)
			if err != nil {
				return moved, err
			}
			if pending == 0 {
				break
			}
			n, err := c.foldTallyChunk(ctx, tally)
			if err != nil {
				return moved, err
			}
			moved += n
			if n < tallyFoldChunk {
				break
			}
		}
	}
	return moved, ctx.Err()
}

func (c *Catalog) foldTallyChunk(ctx context.Context, tally libraryTally) (int64, error) {
	d := store.DialectOf(c.db)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	table, delta, dims := tally.name, tally.name+"_delta", tally.dims
	var n int64
	if d == store.Postgres {
		// One statement deletes the deltas it adds, so a delta committed
		// meanwhile is either moved whole or left for the next fold. The
		// lock keeps two nodes' folds from crossing on the tally rows.
		if err := store.LockKey(ctx, d, tx, strings.ReplaceAll(table, "_", "-")+"-fold"); err != nil {
			return 0, err
		}
		if err := tx.QueryRowContext(ctx, `WITH moved AS (DELETE FROM `+delta+` WHERE id IN (SELECT id FROM `+delta+` ORDER BY id LIMIT ?) RETURNING `+dims+`,d),
summed AS (INSERT INTO `+table+`(`+dims+`,n) SELECT `+dims+`,SUM(d) FROM moved GROUP BY `+dims+` ORDER BY `+dims+`
 ON CONFLICT(`+dims+`) DO UPDATE SET n=`+table+`.n+excluded.n)
SELECT COUNT(*) FROM moved`, tallyFoldChunk).Scan(&n); err != nil {
			return 0, err
		}
	} else {
		var hi int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM (SELECT id FROM `+delta+` ORDER BY id LIMIT ?) p`, tallyFoldChunk).Scan(&hi); err != nil {
			return 0, err
		}
		if hi == 0 {
			return 0, nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(`+dims+`,n)
 SELECT `+dims+`,SUM(d) FROM `+delta+` WHERE id<=? GROUP BY `+dims+`
 ON CONFLICT(`+dims+`) DO UPDATE SET n=`+table+`.n+excluded.n`, hi); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM `+delta+` WHERE id<=?`, hi)
		if err != nil {
			return 0, err
		}
		n, _ = res.RowsAffected()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE n=0`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// tallyFoldEvery spaces the background folds. Pending deltas only lengthen
// reads, so the interval bounds a read's extra rows to the writes of a few
// seconds.
const tallyFoldEvery = 10 * time.Second

func (c *Catalog) foldLibraryTalliesLoop(ctx context.Context) {
	ticker := time.NewTicker(tallyFoldEvery)
	defer ticker.Stop()
	for {
		if _, err := c.foldLibraryTallies(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("catalog: fold library totals", "err", err)
		}
		if _, err := c.nameQueuedFacets(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("catalog: name new studios and people", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tallyShape is what of a browse filter the tally answers directly.
type tallyShape struct {
	types   []string
	genre   string
	country string // a single country filter, answered by catalog_facet_tally
	// studioNames is one positive studio filter's name vocabulary subquery,
	// answered by catalog_facet_tally's studio rows (summed over the names
	// it returns) rather than counting every matching title.
	studioNames     string
	studioNamesArgs []any
	// years is a year range (yearLo..yearHi, 0 for open), answered by
	// catalog_facet_tally's year rows.
	years          bool
	yearLo, yearHi int
	// yearSet is a list of years (Jellyfin's Years=1984,1999), answered
	// by the same year rows.
	yearSet []int
	// first is a sort-name range on jump-bar bucket edges (Plex
	// firstCharacter, Jellyfin NameStartsWith=B / NameLessThan=A),
	// answered by catalog_facet_tally's 'first' rows for those buckets.
	first  []any
	names  sortNameRange
	ranked bool
	lo, hi int
}

// tallyShape reports whether f's predicate, outside the titles tallyTotal
// re-checks, is exactly tally membership plus the shape's dimensions.
// Anything else (owner or viewer rules, blocked genres, share allow lists,
// rare filters, digital-mode movie gating outside the availability paging)
// is counted directly.
func (c *Catalog) tallyShape(f core.Filter) (tallyShape, bool) {
	var s tallyShape
	switch f.Type {
	case core.TypeMovie, core.TypeShow:
		s.types = []string{string(f.Type)}
	case "":
		s.types = []string{string(core.TypeMovie), string(core.TypeShow)}
	default:
		return s, false
	}
	moviesOnlyGate := false
	cfg := c.config()
	if cfg.ReleaseMode == config.ModeDigital && !(f.DigitalDiscovery && f.HiddenMovieSeq != 0) {
		moviesOnlyGate = true
	}
	if f.DigitalDiscovery && f.HiddenMovieSeq == 0 {
		moviesOnlyGate = true
	}
	if moviesOnlyGate && f.Type != core.TypeShow {
		// The home-release window gates movies on dates the tally does not
		// keep; series pass it unconditionally.
		return s, false
	}
	switch {
	case len(f.Genres) > 1:
		return s, false
	case len(f.Genres) == 1:
		if f.Genres[0] == "" {
			return s, false
		}
		s.genre = f.Genres[0]
	}
	if f.Kids {
		if s.genre != "" && s.genre != "Kids" {
			return s, false
		}
		s.genre = "Kids"
	}
	switch {
	case len(f.Countries) > 1 || len(f.Countries) == 1 && (f.Countries[0] == "" || s.genre != ""):
		return s, false
	case len(f.Countries) == 1:
		s.country = f.Countries[0]
	}
	if f.Studio != "" || len(f.StudioFilters) > 0 {
		// A dense studio filter is the one filter whose direct count walks
		// the whole matching set; its total comes from the studio tally rows
		// instead. Only as the filter's sole dimension, mirroring country.
		if s.genre != "" || s.country != "" {
			return s, false
		}
		names, namesArgs, ok := c.positiveStudioNames(f)
		if !ok {
			return s, false
		}
		s.studioNames, s.studioNamesArgs = names, namesArgs
	}
	if f.YearFrom > 0 || f.YearTo > 0 {
		if s.genre != "" || s.country != "" || s.studioNames != "" {
			return s, false
		}
		s.years, s.yearLo, s.yearHi = true, f.YearFrom, f.YearTo
	}
	if len(f.Years) > 0 {
		if s.genre != "" || s.country != "" || s.studioNames != "" || s.years {
			return s, false
		}
		s.yearSet = slices.Compact(slices.Sorted(slices.Values(f.Years)))
	}
	if names := sortNameRangeOf(f); names.bounded {
		buckets, ok := jumpBuckets(names)
		if !ok || s.genre != "" || s.country != "" || s.studioNames != "" || s.years || s.yearSet != nil {
			return s, false
		}
		s.first, s.names = buckets, names
	}
	s.lo, s.hi = 0, int(^uint(0)>>1)
	if f.MaxContentRating != "" {
		level := contentrating.LevelOf(f.MaxContentRating)
		if level == contentrating.Unrated || (f.SaferPlexRating && c.saferRating != nil) {
			return s, false
		}
		s.ranked, s.lo, s.hi = true, max(s.lo, 1), min(s.hi, int(level))
	}
	if f.MinContentRating != "" {
		level := contentrating.LevelOf(f.MinContentRating)
		if level == contentrating.Unrated {
			return s, false
		}
		s.ranked, s.lo = true, max(s.lo, int(level), 1)
	}
	if !f.CatalogCandidates {
		if policy := c.viewerFilter(f.CatalogViewerID, false); policy != nil {
			if q, _ := policy.SQLHidden(); q != "0=1" {
				return s, false
			}
		}
	}
	if f.IncludeKeys != nil {
		// Even empty: browseWhere matches nothing for an empty list.
		return s, false
	}
	rest := f
	rest.Type, rest.Genres, rest.Kids, rest.Countries = "", nil, false, nil
	rest.Studio, rest.StudioFilters = "", nil
	rest.YearFrom, rest.YearTo, rest.Years = 0, 0, nil
	rest.FirstCharacter, rest.NameStartsWith, rest.NameStartsWithOrGreater, rest.NameLessThan = "", "", "", ""
	rest.MaxContentRating, rest.MinContentRating, rest.SaferPlexRating = "", "", false
	rest.CatalogCandidates, rest.CatalogViewerID, rest.CatalogRuleRevision = false, 0, 0
	rest.DigitalDiscovery, rest.DigitalDateEvidence, rest.HomeReleaseOffsetDays, rest.AvailabilityKeepKeys = false, false, 0, nil
	clearGateFields(&rest)
	rest.HiddenMovieSeq, rest.HiddenShowSeq, rest.HiddenEpisodeSeq = 0, 0, 0
	rest.ExcludeKeys, rest.KnownAddedDate = nil, false
	return s, filterUnset(rest)
}

// filterUnset reports whether every field of f is unset; an empty slice is
// unset, as browseWhere treats every slice but IncludeKeys. A field added to core.Filter later is
// therefore counted directly until tallyShape learns it.
func filterUnset(f core.Filter) bool {
	v := reflect.ValueOf(f)
	for i := range v.NumField() {
		field := v.Field(i)
		switch field.Kind() {
		case reflect.Slice, reflect.Map:
			if field.Len() > 0 {
				return false
			}
		default:
			if !field.IsZero() {
				return false
			}
		}
	}
	return true
}

// tallyTotal answers a browse total from the tally when tallyShape admits
// the filter; where and args are browseWhere(f). ok is false otherwise.
//
// For a title outside U (released by now, not in an availability hidden
// set the filter reads, not excluded by key), browseWhere holds exactly
// when the title is a tally member matching the shape. So
//
//	total = tally(shape) - |U ∩ member(shape)| + |U ∩ where|
//
// U is read from three indexes: catalog_plex_shelf_order (type, released_ts),
// the hidden set's primary key and the titles' own key.
func (c *Catalog) tallyTotal(ctx context.Context, f core.Filter, where string, args []any) (int, bool, error) {
	if g := f.Group; g != nil && unwatchedOnly(*g) {
		// Unwatched titles are the listing's titles less the ones the
		// viewer watched: the tally, less a count over the viewer's own
		// watch rows, never a walk of the library.
		all := f
		all.Group = nil
		allWhere, allArgs, err := c.browseWhere(all)
		if err != nil {
			return 0, false, err
		}
		total, ok, err := c.tallyTotal(ctx, all, allWhere, allArgs)
		if err != nil || !ok {
			return 0, ok, err
		}
		var watched int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE t.key IN (SELECT w.rating_key FROM state_watch w WHERE w.user_id=? AND w.view_count>0) AND `+allWhere, append([]any{g.UserID}, allArgs...)...).Scan(&watched); err != nil {
			return 0, false, err
		}
		return total - watched, true, nil
	}
	s, ok := c.tallyShape(f)
	if !ok {
		return 0, false, nil
	}
	table, dims, dimArgs, member, memberArgs := "catalog_browse_tally", "", []any(nil), "", []any(nil)
	switch {
	case s.country != "":
		table = "catalog_facet_tally"
		dims, dimArgs, member, memberArgs = s.facetDims("country", s.country)
	case s.studioNames != "":
		// A positive studio filter: sum the studio tally rows of the name
		// the vocabulary subquery returns, never counting each title. Only
		// when it matches a single name: a title carrying two matching
		// studios would be counted twice across their rows, where the
		// listing counts it once. A substring matching several names is
		// counted directly (rare next to an exact studio name).
		var names int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+s.studioNames+`)`, s.studioNamesArgs...).Scan(&names); err != nil {
			return 0, false, err
		}
		if names > 1 {
			return 0, false, nil
		}
		table = "catalog_facet_tally"
		dims, dimArgs, member, memberArgs = s.facetDims("studio", "")
		dims += ` AND value IN (` + s.studioNames + `)`
		dimArgs = append(dimArgs, s.studioNamesArgs...)
		member += ` AND EXISTS (SELECT 1 FROM catalog_studios st WHERE st.key=t.key AND st.studio IN (` + s.studioNames + `))`
		memberArgs = append(memberArgs, s.studioNamesArgs...)
	case s.first != nil:
		// An alphabet jump: the jump-bar rows of the buckets it spans.
		table = "catalog_facet_tally"
		dims, dimArgs, member, memberArgs = s.facetDims("first", "")
		dims += ` AND value IN (` + placeholders(len(s.first)) + `)`
		dimArgs = append(dimArgs, s.first...)
		names, namesArgs := s.names.sql("t.sort_title")
		member += ` AND ` + names
		memberArgs = append(memberArgs, namesArgs...)
	case s.years:
		// A year or decade filter: the year rows in the range.
		table = "catalog_facet_tally"
		dims, dimArgs, member, memberArgs = s.facetDims("year", "")
		lo, hi := max(s.yearLo, 1), s.yearHi
		if hi <= 0 {
			hi = int(^uint32(0) >> 1)
		}
		dims += ` AND CAST(value AS INTEGER)>=? AND CAST(value AS INTEGER)<=?`
		dimArgs = append(dimArgs, lo, hi)
		member += ` AND t.year>=? AND t.year<=? AND t.year>0`
		memberArgs = append(memberArgs, lo, hi)
	case s.yearSet != nil:
		table = "catalog_facet_tally"
		dims, dimArgs, member, memberArgs = s.facetDims("year", "")
		dims += ` AND CAST(value AS INTEGER) IN (` + placeholders(len(s.yearSet)) + `)`
		member += ` AND t.year IN (` + placeholders(len(s.yearSet)) + `)`
		for _, y := range s.yearSet {
			dimArgs = append(dimArgs, y)
			memberArgs = append(memberArgs, y)
		}
	default:
		dims, dimArgs, member, memberArgs = s.genreDims()
	}
	unsettled, unsettledArgs := c.tallyUnsettled(s, f)
	query := `SELECT (SELECT CAST(COALESCE(SUM(n),0) AS BIGINT) FROM (SELECT n FROM ` + table + ` WHERE ` + dims + ` UNION ALL SELECT d FROM ` + table + `_delta WHERE ` + dims + `) s)
 + (SELECT COUNT(CASE WHEN ` + where + ` THEN 1 END) - COUNT(CASE WHEN ` + member + ` THEN 1 END) FROM catalog_titles t WHERE t.key IN (` + unsettled + `))`
	all := make([]any, 0, 2*len(dimArgs)+len(args)+len(memberArgs)+len(unsettledArgs))
	all = append(append(append(append(append(all, dimArgs...), dimArgs...), args...), memberArgs...), unsettledArgs...)
	var total int
	if err := c.db.QueryRowContext(ctx, query, all...).Scan(&total); err != nil {
		return 0, false, err
	}
	return total, true, nil
}

// unwatchedOnly reports whether g is a viewer's "unwatched" state filter
// and nothing else.
func unwatchedOnly(g core.FilterGroup) bool {
	return g.Unwatched != nil && *g.Unwatched && g.UserID > 0 && g.InProgress == nil && g.LastViewedAfter == 0 && g.LastViewedBefore == 0 &&
		g.OnWatchlist == nil && g.StateField == "" && !g.ParentOnly && g.Operator == "" && g.Filter == nil && len(g.Children) == 0
}

// genreDims is the browse tally's WHERE for s, and the membership a title
// must meet to be counted in it.
func (s tallyShape) genreDims() (dims string, dimArgs []any, member string, memberArgs []any) {
	dims = `type IN (` + placeholders(len(s.types)) + `) AND genre=?`
	for _, typ := range s.types {
		dimArgs = append(dimArgs, typ)
	}
	dimArgs = append(dimArgs, s.genre)
	member, memberArgs = s.typeMember()
	if s.genre != "" {
		member += ` AND EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key=t.key AND g.genre=?)`
		memberArgs = append(memberArgs, s.genre)
	}
	dims, dimArgs, member, memberArgs = s.rankDims(dims, dimArgs, member, memberArgs)
	return
}

// facetDims is catalog_facet_tally's WHERE for one facet of s (value "" for
// every value of it), and the membership a title must meet to be counted.
func (s tallyShape) facetDims(facet, value string) (dims string, dimArgs []any, member string, memberArgs []any) {
	dims = `type IN (` + placeholders(len(s.types)) + `) AND facet=?`
	for _, typ := range s.types {
		dimArgs = append(dimArgs, typ)
	}
	dimArgs = append(dimArgs, facet)
	member, memberArgs = s.typeMember()
	if value != "" {
		dims += ` AND value=?`
		dimArgs = append(dimArgs, value)
		if facet == "country" {
			member += ` AND EXISTS (SELECT 1 FROM catalog_countries co WHERE co.key=t.key AND co.country=?)`
			memberArgs = append(memberArgs, value)
		}
	}
	dims, dimArgs, member, memberArgs = s.rankDims(dims, dimArgs, member, memberArgs)
	return
}

func (s tallyShape) typeMember() (string, []any) {
	member := tallyMemberSQL("t.") + ` AND t.type IN (` + placeholders(len(s.types)) + `)`
	var args []any
	for _, typ := range s.types {
		args = append(args, typ)
	}
	return member, args
}

func (s tallyShape) rankDims(dims string, dimArgs []any, member string, memberArgs []any) (string, []any, string, []any) {
	if s.ranked {
		dims += ` AND rating_rank>=? AND rating_rank<=?`
		dimArgs = append(dimArgs, s.lo, s.hi)
		member += ` AND t.content_rating_rank>=? AND t.content_rating_rank<=?`
		memberArgs = append(memberArgs, s.lo, s.hi)
	}
	return dims, dimArgs, member, memberArgs
}

// tallyUnsettled is U: the titles of s's types where the listing's predicate
// and the tally's can disagree.
func (c *Catalog) tallyUnsettled(s tallyShape, f core.Filter) (string, []any) {
	var unsettled []string
	var unsettledArgs []any
	// An hour early: browseWhere read the clock a moment ago, and a title
	// released in between must be re-checked rather than taken from the
	// tally. U may hold more titles than needed, never fewer.
	baseCutoff := c.now().Add(-time.Hour).Unix()
	if f.GatePolicy != "" && (f.MovieGateCutoff != 0 || f.ShowGateCutoff != 0) {
		for _, typ := range s.types {
			cutoff := baseCutoff
			if typ == string(core.TypeMovie) && f.MovieGateCutoff != 0 && f.MovieGateCutoff < cutoff {
				cutoff = f.MovieGateCutoff
			}
			if typ == string(core.TypeShow) && f.ShowGateCutoff != 0 && f.ShowGateCutoff < cutoff {
				cutoff = f.ShowGateCutoff
			}
			unsettled = append(unsettled, `SELECT key FROM catalog_titles WHERE type=? AND released_ts>?`)
			unsettledArgs = append(unsettledArgs, typ, cutoff)
		}
	} else {
		unsettled = append(unsettled, `SELECT key FROM catalog_titles WHERE type IN (`+placeholders(len(s.types))+`) AND released_ts>?`)
		for _, typ := range s.types {
			unsettledArgs = append(unsettledArgs, typ)
		}
		unsettledArgs = append(unsettledArgs, baseCutoff)
	}
	for _, hidden := range []struct {
		kind string
		seq  int64
	}{
		{string(core.TypeMovie), f.HiddenMovieSeq}, {string(core.TypeShow), f.HiddenShowSeq},
	} {
		if hidden.seq != 0 {
			unsettled = append(unsettled, `SELECT rating_key FROM availability_hidden WHERE kind=? AND from_seq<=? AND to_seq>?`)
			unsettledArgs = append(unsettledArgs, hidden.kind, hidden.seq, hidden.seq)
		}
	}
	if len(f.ExcludeKeys) > 0 {
		unsettled = append(unsettled, `SELECT key FROM catalog_titles WHERE key IN (`+placeholders(len(f.ExcludeKeys))+`)`)
		for _, k := range f.ExcludeKeys {
			unsettledArgs = append(unsettledArgs, k)
		}
	}
	return strings.Join(unsettled, ` UNION `), unsettledArgs
}
