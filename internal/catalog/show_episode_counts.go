package catalog

import (
	"context"
	"strings"

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
// filter that constrains the series, and the episodes only by the viewer's
// rating cap and share filter: the sum of each admitted series' known
// episodes (under a restriction, of its rating histogram rows the
// restriction admits), less those not yet aired. ok is false when the filter
// also constrains the episodes otherwise; the caller counts those directly.
func (c *Catalog) countStoredEpisodesBySeries(ctx context.Context, f core.Filter) (int, bool, error) {
	show, childExclude := splitStoredFilter(f, core.KindEpisode)
	if !c.histogramVisible(f.ChildVisibility) {
		return 0, false, nil
	}
	// The restriction's episode conditions, re-checked on the few episodes
	// subtracted below.
	vis, visArgs := c.childVisibilityConds(f.ChildVisibility)
	// The availability hidden set is the one episode condition counted here:
	// it is few episodes (the evidence window), subtracted below, where a
	// direct count would join every stored episode.
	rest := f
	rest.HiddenEpisodeGeneration, rest.AvailabilityKeepKeys = "", nil
	if conds, _ := c.childFilterConds("e", rest, nil); len(conds) > 0 {
		return 0, false, nil
	}
	if f.Group != nil && !f.Group.ParentOnly {
		return 0, false, nil
	}
	where, args, err := c.browseWhere(show)
	if err != nil {
		return 0, false, err
	}
	if f.Group != nil {
		clause, vals, err := c.compileBrowseGroup(f.Group, "t", 0)
		if err != nil {
			return 0, false, err
		}
		where += " AND (" + clause + ")"
		args = append(args, vals...)
	}
	var known int
	if len(vis) == 0 {
		err = c.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.known),0) FROM catalog_titles t JOIN catalog_show_episodes a ON a.show_key=t.key WHERE `+where, args...).Scan(&known)
	} else {
		// The same conditions over each admitted series' histogram rows.
		hist, histArgs := c.childVisibilityCondsOn(f.ChildVisibility, childCols{rating: "h.content_rating", rank: "h.rating_rank", collection: "h.collection_name"})
		err = c.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(h.known),0) FROM catalog_titles t JOIN catalog_show_episode_ratings h ON h.show_key=t.key WHERE `+strings.Join(hist, " AND ")+` AND `+where,
			append(append([]any{}, histArgs...), args...)...).Scan(&known)
	}
	if err != nil {
		return 0, false, err
	}
	// Episodes subtracted below must pass the restriction too.
	episodes, episodeArgs := where, args
	if len(vis) > 0 {
		episodes = strings.Join(vis, " AND ") + " AND " + where
		episodeArgs = append(append([]any{}, visArgs...), args...)
	}
	// Unaired episodes are few; the recent-episode index reads only them.
	var unaired int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key
		WHERE e.type='episode' AND e.release_known=1 AND e.released_ts>? AND e.adult=0 AND e.show_key<>0 AND `+episodes,
		append([]any{c.now().Unix()}, episodeArgs...)...).Scan(&unaired); err != nil {
		return 0, false, err
	}
	hidden := 0
	if len(childExclude) > 0 {
		vals := make([]any, 0, len(childExclude)+1+len(episodeArgs))
		for _, k := range childExclude {
			vals = append(vals, k)
		}
		vals = append(vals, c.now().Unix())
		vals = append(vals, episodeArgs...)
		q := `SELECT COUNT(*) FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key IN (` + placeholders(len(childExclude)) + `) AND e.type='episode' AND e.adult=0 AND e.release_known=1 AND e.released_ts<=? AND ` + episodes
		if err := c.db.QueryRowContext(ctx, q, vals...).Scan(&hidden); err != nil {
			return 0, false, err
		}
	}
	if f.HiddenEpisodeGeneration != "" {
		// Aired episodes of admitted series in the published hidden set,
		// less those kept and those already excluded above.
		vals := []any{f.HiddenEpisodeGeneration, c.now().Unix()}
		q := `SELECT COUNT(*) FROM availability_hidden ah JOIN catalog_titles e ON e.key=ah.rating_key JOIN catalog_titles t ON t.key=e.show_key
		WHERE ah.kind='episode' AND ah.generation=? AND e.type='episode' AND e.adult=0 AND e.release_known=1 AND e.released_ts<=? AND ` + episodes
		vals = append(vals, episodeArgs...)
		for _, keys := range [][]core.RatingKey{f.AvailabilityKeepKeys, childExclude} {
			if len(keys) == 0 {
				continue
			}
			q += ` AND e.key NOT IN (` + placeholders(len(keys)) + `)`
			for _, k := range keys {
				vals = append(vals, k)
			}
		}
		var unavailable int
		if err := c.db.QueryRowContext(ctx, q, vals...).Scan(&unavailable); err != nil {
			return 0, false, err
		}
		hidden += unavailable
	}
	return max(known-unaired-hidden, 0), true, nil
}
