package catalog

import (
	"context"
	"database/sql"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// The owner dashboard shows how many movies, series, seasons, episodes and
// franchises the mirror holds, how many movies and series are browsable,
// how far the home-release check has got and how many movie years are still
// to correct. Counted directly, each walked every title of its type (the
// home-release split also joined every movie to its check): seconds on a
// large library, for every cache miss of every dashboard.
//
// catalog_stats_tally keeps those numbers instead, per (kind, browsable,
// home, year_pending):
//
//   - kind is the title's type, or statsCollectionsKind for a franchise row
//     of catalog_collections;
//   - browsable is the title's stored flag (0 for a franchise);
//   - home is, for a movie that is not adult and has a known release, its
//     home-release check state (catalog_home_release.checked, 0 without a
//     row), and -1 for everything else;
//   - year_pending is 1 for a movie whose year is still to correct
//     (year_source='').
//
// Like catalog_browse_tally, writers append signed deltas to
// catalog_stats_tally_delta from triggers in the writing transaction, never
// updating the shared rows in place, and the catalogue's background loop
// folds them in. A read sums the few tally rows and the pending deltas in
// one statement, so it is exact whatever has been folded.
//
// Only the home-release split reads the clock (a movie counts once it is
// released by now). The tally holds every known release; the read takes
// back, in the same statement, the movies dated after now, a short range of
// catalog_plex_shelf_order (type, released_ts) that follows upcoming
// releases, not the library.

// statsCollectionsKind is the tally kind of catalog_collections rows. No
// title type contains a space.
const statsCollectionsKind = "franchise row"

// statsHomeEligible is the home-release check's population: movies that are
// not adult and have a known release. alias is "NEW.", "OLD." or "t.".
func statsHomeEligible(a string) string {
	return a + "type='movie' AND " + a + "adult=0 AND " + a + "released_ts<>0"
}

// statsHomeSQL is a title's home dimension.
func statsHomeSQL(a string) string {
	return `CASE WHEN ` + statsHomeEligible(a) + ` THEN COALESCE((SELECT h.checked FROM catalog_home_release h WHERE h.key=` + a + `key),0) ELSE -1 END`
}

// statsYearSQL is a title's year_pending dimension.
func statsYearSQL(a string) string {
	return `CASE WHEN ` + a + `type='movie' AND ` + a + `year_source='' THEN 1 ELSE 0 END`
}

var statsTallyBackfill = `
INSERT INTO catalog_stats_tally(kind,browsable,home,year_pending,n)
 SELECT kind,browsable,home,year_pending,COUNT(*) FROM (SELECT t.type AS kind,t.browsable AS browsable,` + statsHomeSQL("t.") + ` AS home,` + statsYearSQL("t.") + ` AS year_pending FROM catalog_titles t) m
 GROUP BY kind,browsable,home,year_pending;
INSERT INTO catalog_stats_tally(kind,browsable,home,year_pending,n)
 SELECT '` + statsCollectionsKind + `',0,-1,0,COUNT(*) FROM catalog_collections HAVING COUNT(*)>0;`

func init() {
	titleDelta := func(alias, sign string) string {
		return `INSERT INTO catalog_stats_tally_delta(kind,browsable,home,year_pending,d) VALUES(` + alias + `type,` + alias + `browsable,` + statsHomeSQL(alias) + `,` + statsYearSQL(alias) + `,` + sign + `);`
	}
	// A check row moves its movie from home 0 (no row, or unchecked) to its
	// checked state; sign -1 moves it back.
	homeDelta := func(alias, sign string) string {
		return `INSERT INTO catalog_stats_tally_delta(kind,browsable,home,year_pending,d)
 SELECT t.type,t.browsable,v.home,` + statsYearSQL("t.") + `,v.d FROM catalog_titles t,
 (SELECT ` + alias + `checked AS home,` + sign + ` AS d UNION ALL SELECT 0,-(` + sign + `)) v
 WHERE t.key=` + alias + `key AND ` + alias + `checked<>0 AND ` + statsHomeEligible("t.") + `;`
	}
	collectionDelta := func(sign string) string {
		return `INSERT INTO catalog_stats_tally_delta(kind,browsable,home,year_pending,d) VALUES('` + statsCollectionsKind + `',0,-1,0,` + sign + `);`
	}
	titleColumns := `key,type,browsable,adult,released_ts,year_source`
	titleChanged := `OLD.key IS NOT NEW.key OR OLD.type IS NOT NEW.type OR OLD.browsable IS NOT NEW.browsable OR OLD.adult IS NOT NEW.adult OR (OLD.released_ts<>0) IS NOT (NEW.released_ts<>0) OR (OLD.year_source='') IS NOT (NEW.year_source='')`
	homeChanged := `OLD.key IS NOT NEW.key OR OLD.checked IS NOT NEW.checked`
	store.Register("catalog", 46, `CREATE TABLE catalog_stats_tally(kind TEXT NOT NULL, browsable INTEGER NOT NULL, home INTEGER NOT NULL, year_pending INTEGER NOT NULL, n INTEGER NOT NULL, PRIMARY KEY(kind,browsable,home,year_pending));
CREATE TABLE catalog_stats_tally_delta(id INTEGER PRIMARY KEY, kind TEXT NOT NULL, browsable INTEGER NOT NULL, home INTEGER NOT NULL, year_pending INTEGER NOT NULL, d INTEGER NOT NULL);`+statsTallyBackfill+`
CREATE TRIGGER catalog_stats_tally_title_insert AFTER INSERT ON catalog_titles BEGIN `+titleDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_stats_tally_title_delete AFTER DELETE ON catalog_titles BEGIN `+titleDelta("OLD.", "-1")+` END;
CREATE TRIGGER catalog_stats_tally_title_update AFTER UPDATE OF `+titleColumns+` ON catalog_titles
WHEN `+titleChanged+` BEGIN `+titleDelta("OLD.", "-1")+` `+titleDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_stats_tally_home_insert AFTER INSERT ON catalog_home_release WHEN NEW.checked<>0 BEGIN `+homeDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_stats_tally_home_delete AFTER DELETE ON catalog_home_release WHEN OLD.checked<>0 BEGIN `+homeDelta("OLD.", "-1")+` END;
CREATE TRIGGER catalog_stats_tally_home_update AFTER UPDATE OF key,checked ON catalog_home_release
WHEN `+homeChanged+` BEGIN `+homeDelta("OLD.", "-1")+` `+homeDelta("NEW.", "1")+` END;
CREATE TRIGGER catalog_stats_tally_collection_insert AFTER INSERT ON catalog_collections BEGIN `+collectionDelta("1")+` END;
CREATE TRIGGER catalog_stats_tally_collection_delete AFTER DELETE ON catalog_collections BEGIN `+collectionDelta("-1")+` END;`)
	// As for catalog_browse_tally's genres: a check-row trigger first takes
	// a share lock on its title, so it waits for a title write in flight and
	// then reads the committed row, and a later title write waits for the
	// check row's commit before its own trigger reads the check. (Check rows
	// are only written for stored titles, so a title and its check are never
	// both first inserted by concurrent transactions.) The migration
	// excludes old-version writers until the backfill and the triggers are
	// visible together.
	store.RegisterDialect("catalog", 46, store.Postgres, `LOCK TABLE catalog_titles, catalog_home_release, catalog_collections IN SHARE ROW EXCLUSIVE MODE;
CREATE TABLE catalog_stats_tally(kind TEXT NOT NULL, browsable BIGINT NOT NULL, home BIGINT NOT NULL, year_pending BIGINT NOT NULL, n BIGINT NOT NULL, PRIMARY KEY(kind,browsable,home,year_pending));
CREATE TABLE catalog_stats_tally_delta(id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, kind TEXT NOT NULL, browsable BIGINT NOT NULL, home BIGINT NOT NULL, year_pending BIGINT NOT NULL, d BIGINT NOT NULL);`+statsTallyBackfill+`
CREATE FUNCTION catalog_stats_tally_title() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF TG_OP<>'INSERT' THEN `+titleDelta("OLD.", "-1")+` END IF;
IF TG_OP<>'DELETE' THEN `+titleDelta("NEW.", "1")+` END IF;
RETURN NULL; END $$;
CREATE FUNCTION catalog_stats_tally_home() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF TG_OP<>'INSERT' AND OLD.checked<>0 THEN PERFORM 1 FROM catalog_titles WHERE key=OLD.key FOR SHARE; `+homeDelta("OLD.", "-1")+` END IF;
IF TG_OP<>'DELETE' AND NEW.checked<>0 THEN PERFORM 1 FROM catalog_titles WHERE key=NEW.key FOR SHARE; `+homeDelta("NEW.", "1")+` END IF;
RETURN NULL; END $$;
CREATE FUNCTION catalog_stats_tally_collection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF TG_OP='INSERT' THEN `+collectionDelta("1")+` ELSE `+collectionDelta("-1")+` END IF;
RETURN NULL; END $$;
CREATE TRIGGER catalog_stats_tally_title AFTER INSERT OR DELETE ON catalog_titles FOR EACH ROW EXECUTE FUNCTION catalog_stats_tally_title();
CREATE TRIGGER catalog_stats_tally_title_update AFTER UPDATE OF `+titleColumns+` ON catalog_titles
 FOR EACH ROW WHEN (`+pgDistinct(titleChanged)+`) EXECUTE FUNCTION catalog_stats_tally_title();
CREATE TRIGGER catalog_stats_tally_home AFTER INSERT OR DELETE ON catalog_home_release FOR EACH ROW EXECUTE FUNCTION catalog_stats_tally_home();
CREATE TRIGGER catalog_stats_tally_home_update AFTER UPDATE OF key,checked ON catalog_home_release
 FOR EACH ROW WHEN (`+pgDistinct(homeChanged)+`) EXECUTE FUNCTION catalog_stats_tally_home();
CREATE TRIGGER catalog_stats_tally_collection AFTER INSERT OR DELETE ON catalog_collections FOR EACH ROW EXECUTE FUNCTION catalog_stats_tally_collection();`)
	// A logical restore rebuilds the tally from the restored rows, as it
	// does catalog_browse_tally.
	store.RegisterRestore("catalog_stats_tally", func(context.Context, *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return func(ctx context.Context, tx *sql.Tx) error { return rebuildStatsTally(ctx, tx) }, nil
	}, "catalog_stats_tally", "catalog_stats_tally_delta")
}

func rebuildStatsTally(ctx context.Context, db execer) error {
	for _, q := range []string{`DELETE FROM catalog_stats_tally_delta`, `DELETE FROM catalog_stats_tally`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, q := range strings.Split(strings.TrimSpace(statsTallyBackfill), ";") {
		if q = strings.TrimSpace(q); q == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// statsSumSQL sums the tally and its pending deltas per dimension.
const statsSumSQL = `SELECT kind,browsable,home,year_pending,CAST(SUM(n) AS BIGINT) AS n FROM (SELECT kind,browsable,home,year_pending,n FROM catalog_stats_tally UNION ALL SELECT kind,browsable,home,year_pending,d FROM catalog_stats_tally_delta) s GROUP BY kind,browsable,home,year_pending`

// tallyStats fills s's library counts and returns the movies whose year is
// still to correct, in one statement: the tally plus the correction for
// movies not released by now.
func (c *Catalog) tallyStats(ctx context.Context, s *Stats) (yearPending int, err error) {
	rows, err := c.db.QueryContext(ctx, `SELECT 0,kind,browsable,home,year_pending,n FROM (`+statsSumSQL+`) tally
UNION ALL
SELECT 1,'movie',0,COALESCE(h.checked,0),0,COUNT(*) FROM catalog_titles t LEFT JOIN catalog_home_release h ON h.key=t.key
 WHERE t.type='movie' AND t.released_ts>? AND t.adult=0 GROUP BY COALESCE(h.checked,0)`, c.now().Unix())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var unreleased int
		var kind string
		var browsable, home, pending, n int64
		if err := rows.Scan(&unreleased, &kind, &browsable, &home, &pending, &n); err != nil {
			return 0, err
		}
		count := int(n)
		if unreleased == 1 {
			// Counted in the tally's home split, not yet released.
			switch home {
			case 1:
				s.HomeReleaseChecked -= count
			case 0:
				s.HomeReleasePending -= count
			}
			continue
		}
		switch kind {
		case "movie":
			s.Movies += count
			if browsable == 1 {
				s.BrowsableMovies += count
			}
		case "show":
			s.Shows += count
			if browsable == 1 {
				s.BrowsableShows += count
			}
		case "season":
			s.Seasons += count
		case "episode":
			s.Episodes += count
		case statsCollectionsKind:
			s.Collections += count
		}
		switch home {
		case 1:
			s.HomeReleaseChecked += count
		case 0:
			s.HomeReleasePending += count
		}
		if pending == 1 {
			yearPending += count
		}
	}
	return yearPending, rows.Err()
}
