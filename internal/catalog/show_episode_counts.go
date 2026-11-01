package catalog

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// catalog_show_episodes keeps, per series, how many of its stored episodes a
// whole-library listing can show once aired: non-adult with a known release
// (released_ts<>0, which release_known is generated from).
// Triggers keep it current, so a listing's total is a sum over the series its
// filter admits, less the few episodes not yet aired, instead of a join over
// every stored episode.
func init() {
	store.Register("catalog", 24, `
CREATE TABLE catalog_show_episodes(show_key INTEGER PRIMARY KEY, known INTEGER NOT NULL);
INSERT INTO catalog_show_episodes(show_key, known)
 SELECT show_key, COUNT(*) FROM catalog_titles WHERE type='episode' AND adult=0 AND released_ts<>0 AND show_key<>0 GROUP BY show_key;
CREATE TRIGGER catalog_show_episodes_insert AFTER INSERT ON catalog_titles
WHEN new.type='episode' AND new.adult=0 AND new.released_ts<>0 AND new.show_key<>0 BEGIN
 INSERT INTO catalog_show_episodes(show_key, known) VALUES (new.show_key, 1)
  ON CONFLICT(show_key) DO UPDATE SET known=known+1;
END;
CREATE TRIGGER catalog_show_episodes_delete AFTER DELETE ON catalog_titles
WHEN old.type='episode' AND old.adult=0 AND old.released_ts<>0 AND old.show_key<>0 BEGIN
 UPDATE catalog_show_episodes SET known=known-1 WHERE show_key=old.show_key;
END;
CREATE TRIGGER catalog_show_episodes_update AFTER UPDATE OF type, adult, released_ts, show_key ON catalog_titles
WHEN (old.type='episode' AND old.adult=0 AND old.released_ts<>0 AND old.show_key<>0) IS NOT (new.type='episode' AND new.adult=0 AND new.released_ts<>0 AND new.show_key<>0)
 OR old.show_key IS NOT new.show_key BEGIN
 UPDATE catalog_show_episodes SET known=known-1
  WHERE show_key=old.show_key AND old.type='episode' AND old.adult=0 AND old.released_ts<>0;
 INSERT INTO catalog_show_episodes(show_key, known)
  SELECT new.show_key, 1 WHERE new.type='episode' AND new.adult=0 AND new.released_ts<>0 AND new.show_key<>0
  ON CONFLICT(show_key) DO UPDATE SET known=known+1;
END;`)
	store.RegisterDialect("catalog", 24, store.Postgres, `
CREATE TABLE catalog_show_episodes(show_key BIGINT PRIMARY KEY, known BIGINT NOT NULL);
INSERT INTO catalog_show_episodes(show_key, known)
 SELECT show_key, COUNT(*) FROM catalog_titles WHERE type='episode' AND adult=0 AND released_ts<>0 AND show_key<>0 GROUP BY show_key;
CREATE FUNCTION catalog_show_episodes_count() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP IN ('UPDATE','DELETE') AND OLD.type='episode' AND OLD.adult=0 AND OLD.released_ts<>0 AND OLD.show_key<>0 THEN
  UPDATE catalog_show_episodes SET known=known-1 WHERE show_key=OLD.show_key;
 END IF;
 IF TG_OP IN ('INSERT','UPDATE') AND NEW.type='episode' AND NEW.adult=0 AND NEW.released_ts<>0 AND NEW.show_key<>0 THEN
  INSERT INTO catalog_show_episodes(show_key, known) VALUES (NEW.show_key, 1)
   ON CONFLICT(show_key) DO UPDATE SET known=catalog_show_episodes.known+1;
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_show_episodes_count AFTER INSERT OR DELETE OR UPDATE OF type, adult, released_ts, show_key ON catalog_titles
 FOR EACH ROW EXECUTE FUNCTION catalog_show_episodes_count();`)
}

// countStoredEpisodesBySeries counts what StoredEpisodesByKey pages for a
// filter that only constrains the series: the sum of each admitted series'
// known episodes, less those not yet aired. ok is false when the filter also
// constrains the episodes themselves; the caller counts those directly.
func (c *Catalog) countStoredEpisodesBySeries(ctx context.Context, f core.Filter) (int, bool, error) {
	show, childExclude := splitStoredFilter(f, core.KindEpisode)
	if conds, _ := c.childFilterConds("e", f, childExclude); len(conds) > 0 {
		return 0, false, nil
	}
	where, args, err := c.browseWhere(show)
	if err != nil {
		return 0, false, err
	}
	var known int
	if err := c.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.known),0) FROM catalog_titles t JOIN catalog_show_episodes a ON a.show_key=t.key WHERE `+where, args...).Scan(&known); err != nil {
		return 0, false, err
	}
	// Unaired episodes are few; the recent-episode index reads only them.
	var unaired int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key
		WHERE e.type='episode' AND e.release_known=1 AND e.released_ts>? AND e.adult=0 AND e.show_key<>0 AND `+where,
		append([]any{c.now().Unix()}, args...)...).Scan(&unaired); err != nil {
		return 0, false, err
	}
	return max(known-unaired, 0), true, nil
}
