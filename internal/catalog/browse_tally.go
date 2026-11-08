package catalog

import (
	"context"
	"database/sql"
	"reflect"
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
// stored titles,
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
	for _, q := range []string{`DELETE FROM catalog_show_episode_ratings`, showEpisodeRatingsBackfill} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
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
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tallyShape is what of a browse filter the tally answers directly.
type tallyShape struct {
	types  []string
	genre  string
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
	if cfg.ReleaseMode == config.ModeDigital && !(f.DigitalDiscovery && f.HiddenMovieGeneration != "") {
		moviesOnlyGate = true
	}
	if f.DigitalDiscovery && f.HiddenMovieGeneration == "" {
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
	rest.Type, rest.Genres, rest.Kids = "", nil, false
	rest.MaxContentRating, rest.MinContentRating, rest.SaferPlexRating = "", "", false
	rest.CatalogCandidates, rest.CatalogViewerID, rest.CatalogRuleRevision = false, 0, 0
	rest.DigitalDiscovery, rest.DigitalDateEvidence, rest.AvailabilityKeepKeys = false, false, nil
	rest.HiddenMovieGeneration, rest.HiddenShowGeneration, rest.HiddenEpisodeGeneration = "", "", ""
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
	s, ok := c.tallyShape(f)
	if !ok {
		return 0, false, nil
	}
	dims := `type IN (` + placeholders(len(s.types)) + `) AND genre=?`
	var dimArgs []any
	for _, typ := range s.types {
		dimArgs = append(dimArgs, typ)
	}
	dimArgs = append(dimArgs, s.genre)
	member := tallyMemberSQL("t.") + ` AND t.type IN (` + placeholders(len(s.types)) + `)`
	var memberArgs []any
	for _, typ := range s.types {
		memberArgs = append(memberArgs, typ)
	}
	if s.genre != "" {
		member += ` AND EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key=t.key AND g.genre=?)`
		memberArgs = append(memberArgs, s.genre)
	}
	if s.ranked {
		dims += ` AND rating_rank>=? AND rating_rank<=?`
		dimArgs = append(dimArgs, s.lo, s.hi)
		member += ` AND t.content_rating_rank>=? AND t.content_rating_rank<=?`
		memberArgs = append(memberArgs, s.lo, s.hi)
	}
	unsettled := []string{`SELECT key FROM catalog_titles WHERE type IN (` + placeholders(len(s.types)) + `) AND released_ts>?`}
	var unsettledArgs []any
	for _, typ := range s.types {
		unsettledArgs = append(unsettledArgs, typ)
	}
	// An hour early: browseWhere read the clock a moment ago, and a title
	// released in between must be re-checked rather than taken from the
	// tally. U may hold more titles than needed, never fewer.
	unsettledArgs = append(unsettledArgs, c.now().Add(-time.Hour).Unix())
	for _, hidden := range []struct{ kind, generation string }{
		{string(core.TypeMovie), f.HiddenMovieGeneration}, {string(core.TypeShow), f.HiddenShowGeneration},
	} {
		if hidden.generation != "" {
			unsettled = append(unsettled, `SELECT rating_key FROM availability_hidden WHERE kind=? AND generation=?`)
			unsettledArgs = append(unsettledArgs, hidden.kind, hidden.generation)
		}
	}
	if len(f.ExcludeKeys) > 0 {
		unsettled = append(unsettled, `SELECT key FROM catalog_titles WHERE key IN (`+placeholders(len(f.ExcludeKeys))+`)`)
		for _, k := range f.ExcludeKeys {
			unsettledArgs = append(unsettledArgs, k)
		}
	}
	query := `SELECT (SELECT CAST(COALESCE(SUM(n),0) AS BIGINT) FROM (SELECT n FROM catalog_browse_tally WHERE ` + dims + ` UNION ALL SELECT d FROM catalog_browse_tally_delta WHERE ` + dims + `) s)
 + (SELECT COUNT(CASE WHEN ` + where + ` THEN 1 END) - COUNT(CASE WHEN ` + member + ` THEN 1 END) FROM catalog_titles t WHERE t.key IN (` + strings.Join(unsettled, ` UNION `) + `))`
	all := make([]any, 0, 2*len(dimArgs)+len(args)+len(memberArgs)+len(unsettledArgs))
	all = append(append(append(append(append(all, dimArgs...), dimArgs...), args...), memberArgs...), unsettledArgs...)
	var total int
	if err := c.db.QueryRowContext(ctx, query, all...).Scan(&total); err != nil {
		return 0, false, err
	}
	return total, true, nil
}
