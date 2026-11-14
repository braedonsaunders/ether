package catalog

import (
	"context"
	"database/sql"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"strings"
)

// This metadata-only projection is a safe upper bound for every viewer/type/
// date policy. A missing pair proves an AND impossible; a present pair never
// bypasses the ordinary policy query. Per-title witnesses avoid global counter
// hot spots during bulk ingestion. Writes touch only a title's own genres.
func init() {
	schema := `CREATE TABLE catalog_genre_pairs(a TEXT NOT NULL,b TEXT NOT NULL,key BIGINT NOT NULL,PRIMARY KEY(a,b,key));
CREATE INDEX catalog_genre_pairs_title ON catalog_genre_pairs(key);
INSERT INTO catalog_genre_pairs(a,b,key) SELECT a.genre,b.genre,a.key FROM catalog_genres a JOIN catalog_genres b ON b.key=a.key;`
	add := `INSERT INTO catalog_genre_pairs(a,b,key) SELECT NEW.genre,genre,NEW.key FROM catalog_genres WHERE key=NEW.key ON CONFLICT(a,b,key) DO NOTHING;
INSERT INTO catalog_genre_pairs(a,b,key) SELECT genre,NEW.genre,NEW.key FROM catalog_genres WHERE key=NEW.key AND genre<>NEW.genre ON CONFLICT(a,b,key) DO NOTHING;`
	remove := `DELETE FROM catalog_genre_pairs WHERE key=OLD.key AND (a=OLD.genre OR b=OLD.genre);`
	store.Register("catalog", 40, schema+`
CREATE TRIGGER catalog_genre_pairs_insert AFTER INSERT ON catalog_genres BEGIN `+add+` END;
CREATE TRIGGER catalog_genre_pairs_delete BEFORE DELETE ON catalog_genres BEGIN `+remove+` END;
CREATE TRIGGER catalog_genre_pairs_update_before BEFORE UPDATE ON catalog_genres BEGIN `+remove+` END;
CREATE TRIGGER catalog_genre_pairs_update_after AFTER UPDATE ON catalog_genres BEGIN `+add+` END;`)
	// The migration transaction excludes old-version writers until backfill and
	// maintenance triggers become visible together.
	store.RegisterDialect("catalog", 40, store.Postgres, `LOCK TABLE catalog_genres IN SHARE ROW EXCLUSIVE MODE;`+schema+`
CREATE FUNCTION catalog_genre_pairs_add() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
PERFORM pg_advisory_xact_lock(hashtextextended('ether:catalog-genres:' || NEW.key::text,0));
`+add+` RETURN NEW; END $$;
CREATE FUNCTION catalog_genre_pairs_remove() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
IF TG_OP='UPDATE' THEN
 PERFORM pg_advisory_xact_lock(hashtextextended('ether:catalog-genres:' || LEAST(OLD.key,NEW.key)::text,0));
 IF OLD.key<>NEW.key THEN PERFORM pg_advisory_xact_lock(hashtextextended('ether:catalog-genres:' || GREATEST(OLD.key,NEW.key)::text,0)); END IF;
ELSE
 PERFORM pg_advisory_xact_lock(hashtextextended('ether:catalog-genres:' || OLD.key::text,0));
END IF;
`+remove+` IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END $$;
CREATE TRIGGER catalog_genre_pairs_insert AFTER INSERT OR UPDATE ON catalog_genres FOR EACH ROW EXECUTE FUNCTION catalog_genre_pairs_add();
CREATE TRIGGER catalog_genre_pairs_delete BEFORE DELETE OR UPDATE ON catalog_genres FOR EACH ROW EXECUTE FUNCTION catalog_genre_pairs_remove();`)
}

// mandatoryConsoleFilters visits only predicates that must be true. OR branches
// and viewer-state nodes are left to SQL, so no alternative can be pruned.
func mandatoryConsoleFilters(f core.Filter) []core.Filter {
	out := []core.Filter{f}
	var walk func(*core.FilterGroup)
	walk = func(g *core.FilterGroup) {
		if g == nil || groupHasOwnState(g) {
			return
		}
		if g.Filter != nil {
			out = append(out, *g.Filter)
			walk(g.Filter.Group)
			return
		}
		if g.Operator == "and" {
			for i := range g.Children {
				walk(&g.Children[i])
			}
		}
	}
	walk(f.Group)
	return out
}

func (c *Catalog) consoleDomainEmpty(ctx context.Context, f core.Filter) (bool, error) {
	var genres [][]string
	exists := func(query string, args ...any) (bool, error) {
		var one int
		err := c.db.QueryRowContext(ctx, query+" LIMIT 1", args...).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	for _, leaf := range mandatoryConsoleFilters(f) {
		if len(leaf.Genres) > 0 {
			genres = append(genres, leaf.Genres)
		}
		if leaf.Kids {
			genres = append(genres, []string{"Kids"})
		}
		keywords := leaf.Keywords
		if leaf.Anime {
			keywords = append(append([]string{}, keywords...), "anime")
		}
		for _, keyword := range keywords {
			keyword = strings.ToLower(strings.TrimSpace(keyword))
			if keyword == "" {
				continue
			}
			ok, err := exists(`SELECT 1 FROM catalog_keywords WHERE LOWER(keyword)=?`, keyword)
			if err != nil || !ok {
				return !ok, err
			}
		}
		credits := leaf.Credits
		if leaf.Person > 0 && len(credits) == 0 {
			credits = []core.CreditFilter{{People: []int64{leaf.Person}}}
		}
		for _, credit := range credits {
			if credit.Exclude {
				continue
			}
			var parts []string
			var args []any
			if len(credit.People) > 0 {
				parts = append(parts, "person_tmdb IN("+placeholders(len(credit.People))+")")
				for _, id := range credit.People {
					args = append(args, id)
				}
			}
			if len(credit.Names) > 0 {
				parts = append(parts, "person_tmdb IN(SELECT person_tmdb FROM catalog_person_names WHERE "+personNameExpression(store.DialectOf(c.db))+" IN("+placeholders(len(credit.Names))+"))")
				for _, name := range credit.Names {
					args = append(args, asciiPersonName(name))
				}
			}
			if len(parts) == 0 {
				continue
			}
			predicate := "(" + strings.Join(parts, " OR ") + ")"
			if credit.Kind == "composer" {
				predicate += " AND kind='crew' AND role='Composer'"
			} else if credit.Kind != "" {
				predicate += " AND kind=?"
				args = append(args, credit.Kind)
			}
			ok, err := exists("SELECT 1 FROM catalog_credits WHERE "+predicate, args...)
			if err != nil || !ok {
				return !ok, err
			}
		}
	}
	// Ordered probes keep generic PostgreSQL plans on the covering indexes even
	// when common genres make an unordered LIMIT look cheap as a sequential scan.
	for i, left := range genres {
		args := make([]any, 0, len(left))
		for _, v := range left {
			args = append(args, v)
		}
		ok, err := exists("SELECT 1 FROM catalog_genres WHERE genre IN("+placeholders(len(left))+") ORDER BY genre,key", args...)
		if err != nil || !ok {
			return !ok, err
		}
		for _, right := range genres[:i] {
			values := append([]any{}, args...)
			for _, v := range right {
				values = append(values, v)
			}
			ok, err := exists("SELECT 1 FROM catalog_genre_pairs WHERE a IN("+placeholders(len(left))+") AND b IN("+placeholders(len(right))+") ORDER BY a,b,key", values...)
			if err != nil || !ok {
				return !ok, err
			}
		}
	}
	return c.consoleYearEmpty(ctx, f)
}

func consoleHasSparseCandidates(f core.Filter) bool {
	for _, v := range mandatoryConsoleFilters(f) {
		if v.Topic != "" || v.Person > 0 || len(v.Credits) > 0 || len(v.Keywords) > 0 || len(v.Tags) > 0 || v.Anime || v.Collection > 0 || len(v.Collections) > 0 || v.DateAddedFrom != 0 || v.DateAddedTo != 0 || v.EtherAddedFrom != 0 || v.EtherAddedTo != 0 || len(v.ReleasedFilters) > 0 {
			return true
		}
	}
	return false
}
