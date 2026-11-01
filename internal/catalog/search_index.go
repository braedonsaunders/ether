package catalog

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Title search uses a trigram index, so a substring match costs a lookup of
// the query's trigrams instead of a LIKE over every catalogue row. On SQLite
// it is an external-content FTS5 table kept in step by triggers; on
// PostgreSQL a pg_trgm GIN index, which the planner applies to the same
// ILIKE predicate, when the server has the extension (otherwise searches
// fall back to a scan, as before).
func init() {
	store.Register("catalog", 16, `
CREATE VIRTUAL TABLE catalog_titles_fts USING fts5(title, original_title, content='catalog_titles', content_rowid='key', tokenize='trigram');
INSERT INTO catalog_titles_fts(catalog_titles_fts) VALUES('rebuild');
CREATE TRIGGER catalog_titles_fts_insert AFTER INSERT ON catalog_titles BEGIN
 INSERT INTO catalog_titles_fts(rowid, title, original_title) VALUES (new.key, new.title, new.original_title);
END;
CREATE TRIGGER catalog_titles_fts_delete AFTER DELETE ON catalog_titles BEGIN
 INSERT INTO catalog_titles_fts(catalog_titles_fts, rowid, title, original_title) VALUES ('delete', old.key, old.title, old.original_title);
END;
CREATE TRIGGER catalog_titles_fts_update AFTER UPDATE OF title, original_title ON catalog_titles
WHEN old.title IS NOT new.title OR old.original_title IS NOT new.original_title BEGIN
 INSERT INTO catalog_titles_fts(catalog_titles_fts, rowid, title, original_title) VALUES ('delete', old.key, old.title, old.original_title);
 INSERT INTO catalog_titles_fts(rowid, title, original_title) VALUES (new.key, new.title, new.original_title);
END;
CREATE VIRTUAL TABLE catalog_alt_titles_fts USING fts5(title, content='catalog_alt_titles', tokenize='trigram');
INSERT INTO catalog_alt_titles_fts(catalog_alt_titles_fts) VALUES('rebuild');
CREATE TRIGGER catalog_alt_titles_fts_insert AFTER INSERT ON catalog_alt_titles BEGIN
 INSERT INTO catalog_alt_titles_fts(rowid, title) VALUES (new.rowid, new.title);
END;
CREATE TRIGGER catalog_alt_titles_fts_delete AFTER DELETE ON catalog_alt_titles BEGIN
 INSERT INTO catalog_alt_titles_fts(catalog_alt_titles_fts, rowid, title) VALUES ('delete', old.rowid, old.title);
END;
CREATE TRIGGER catalog_alt_titles_fts_update AFTER UPDATE OF title ON catalog_alt_titles WHEN old.title IS NOT new.title BEGIN
 INSERT INTO catalog_alt_titles_fts(catalog_alt_titles_fts, rowid, title) VALUES ('delete', old.rowid, old.title);
 INSERT INTO catalog_alt_titles_fts(rowid, title) VALUES (new.rowid, new.title);
END;`)
	store.RegisterDialect("catalog", 16, store.Postgres, `
DO $$
BEGIN
 CREATE EXTENSION IF NOT EXISTS pg_trgm;
 CREATE INDEX IF NOT EXISTS catalog_titles_title_trgm ON catalog_titles USING gin (title gin_trgm_ops, original_title gin_trgm_ops);
 CREATE INDEX IF NOT EXISTS catalog_alt_titles_title_trgm ON catalog_alt_titles USING gin (title gin_trgm_ops);
EXCEPTION WHEN OTHERS THEN
 RAISE NOTICE 'catalog: pg_trgm unavailable, title search scans: %', SQLERRM;
END
$$;`)
}

// People search uses a word-prefix index over every credited person's name.
// catalog_person_names holds one row per person (their latest credited name
// and how many credits they have), kept in step with catalog_credits by
// triggers. On SQLite an FTS5 table over it answers "every query word is a
// prefix of a name word"; on PostgreSQL a pg_trgm index serves the same
// candidate lookup.
func init() {
	store.Register("catalog", 17, `
CREATE TABLE catalog_person_names (person_tmdb INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '', credits INTEGER NOT NULL DEFAULT 0);
INSERT INTO catalog_person_names(person_tmdb, name, credits)
 SELECT person_tmdb, MAX(name), COUNT(*) FROM catalog_credits WHERE person_tmdb>0 GROUP BY person_tmdb;
CREATE VIRTUAL TABLE catalog_person_names_fts USING fts5(name, content='catalog_person_names', content_rowid='person_tmdb', tokenize='unicode61 remove_diacritics 0', prefix='1 2 3');
INSERT INTO catalog_person_names_fts(catalog_person_names_fts) VALUES('rebuild');
CREATE TRIGGER catalog_person_names_fts_insert AFTER INSERT ON catalog_person_names BEGIN
 INSERT INTO catalog_person_names_fts(rowid, name) VALUES (new.person_tmdb, new.name);
END;
CREATE TRIGGER catalog_person_names_fts_delete AFTER DELETE ON catalog_person_names BEGIN
 INSERT INTO catalog_person_names_fts(catalog_person_names_fts, rowid, name) VALUES ('delete', old.person_tmdb, old.name);
END;
CREATE TRIGGER catalog_person_names_fts_update AFTER UPDATE OF name ON catalog_person_names WHEN old.name IS NOT new.name BEGIN
 INSERT INTO catalog_person_names_fts(catalog_person_names_fts, rowid, name) VALUES ('delete', old.person_tmdb, old.name);
 INSERT INTO catalog_person_names_fts(rowid, name) VALUES (new.person_tmdb, new.name);
END;
CREATE TRIGGER catalog_person_names_credit_insert AFTER INSERT ON catalog_credits WHEN new.person_tmdb>0 BEGIN
 INSERT INTO catalog_person_names(person_tmdb, name, credits) VALUES (new.person_tmdb, new.name, 1)
 ON CONFLICT(person_tmdb) DO UPDATE SET credits=catalog_person_names.credits+1,
  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE catalog_person_names.name END;
END;
CREATE TRIGGER catalog_person_names_credit_delete AFTER DELETE ON catalog_credits WHEN old.person_tmdb>0 BEGIN
 UPDATE catalog_person_names SET credits=credits-1 WHERE person_tmdb=old.person_tmdb;
END;
CREATE TRIGGER catalog_person_names_credit_rename AFTER UPDATE OF name ON catalog_credits WHEN new.person_tmdb>0 AND new.name<>'' AND old.name IS NOT new.name BEGIN
 UPDATE catalog_person_names SET name=new.name WHERE person_tmdb=new.person_tmdb;
END;`)
	store.RegisterDialect("catalog", 17, store.Postgres, `
CREATE TABLE catalog_person_names (person_tmdb BIGINT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', credits BIGINT NOT NULL DEFAULT 0);
INSERT INTO catalog_person_names(person_tmdb, name, credits)
 SELECT person_tmdb, MAX(name), COUNT(*) FROM catalog_credits WHERE person_tmdb>0 GROUP BY person_tmdb;
CREATE FUNCTION catalog_person_names_credit_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF TG_OP = 'INSERT' THEN
  IF NEW.person_tmdb > 0 THEN
   INSERT INTO catalog_person_names(person_tmdb, name, credits) VALUES (NEW.person_tmdb, NEW.name, 1)
   ON CONFLICT(person_tmdb) DO UPDATE SET credits=catalog_person_names.credits+1,
    name=CASE WHEN EXCLUDED.name<>'' THEN EXCLUDED.name ELSE catalog_person_names.name END;
  END IF;
 ELSIF TG_OP = 'DELETE' THEN
  IF OLD.person_tmdb > 0 THEN
   UPDATE catalog_person_names SET credits=credits-1 WHERE person_tmdb=OLD.person_tmdb;
  END IF;
 ELSIF NEW.person_tmdb > 0 AND NEW.name<>'' AND OLD.name IS DISTINCT FROM NEW.name THEN
  UPDATE catalog_person_names SET name=NEW.name WHERE person_tmdb=NEW.person_tmdb;
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER catalog_person_names_credit AFTER INSERT OR DELETE OR UPDATE OF name ON catalog_credits FOR EACH ROW EXECUTE FUNCTION catalog_person_names_credit_fn();
DO $$
BEGIN
 CREATE EXTENSION IF NOT EXISTS pg_trgm;
 CREATE INDEX IF NOT EXISTS catalog_person_names_trgm ON catalog_person_names USING gin (name gin_trgm_ops);
EXCEPTION WHEN OTHERS THEN
 RAISE NOTICE 'catalog: pg_trgm unavailable, people search scans: %', SQLERRM;
END
$$;`)
}

// Browse totals count from an index alone: every browse filters on type,
// browsable, adult and a released date, and the visibility rules probe key
// sets by rowid, so a library-wide count reads one narrow index instead of
// every title row.
func init() {
	store.Register("catalog", 18, `CREATE INDEX catalog_titles_type_browse_count ON catalog_titles(type, browsable, adult, released_ts);`)
}

// Every browse now filters on release_known=1 as well (pre-1970 dates are
// known), and release_known is a generated column the count index did not
// hold, so a library total read every title row again. The index now holds
// it, and sort_title, so the Plex jump bar's per-letter counts read the
// index alone too.
func init() {
	store.Register("catalog", 23, `DROP INDEX IF EXISTS catalog_titles_type_browse_count;
CREATE INDEX catalog_titles_type_browse_count ON catalog_titles(type, browsable, adult, release_known, released_ts, sort_title);`)
}

// titleMatch is the substring match of query against a title's name,
// original name or one of its alternative titles (romanized and regional
// names, so "Sousou no Frieren" finds Frieren). alias qualifies the
// catalogue's columns ("t." or "", meaning catalog_titles itself). On SQLite
// a query of three or more characters with no LIKE metacharacters is answered from
// the trigram index (FTS5 uses the index only without an ESCAPE clause);
// anything else keeps the plain LIKE.
func (c *Catalog) titleMatch(alias, query string) (string, []any) {
	if alias == "" {
		// Qualified, so the alternative-title subquery's key never binds to
		// its own table.
		alias = "catalog_titles."
	}
	like := "%" + escapeLike(query) + "%"
	d := store.DialectOf(c.db)
	if d == store.SQLite && utf8.RuneCountInString(query) >= 3 && !strings.ContainsAny(query, `%_\`) {
		return alias + `key IN (SELECT rowid FROM catalog_titles_fts WHERE title LIKE ? UNION SELECT rowid FROM catalog_titles_fts WHERE original_title LIKE ?
			UNION SELECT a.key FROM catalog_alt_titles a WHERE a.rowid IN (SELECT rowid FROM catalog_alt_titles_fts WHERE title LIKE ?))`, []any{like, like, like}
	}
	op := store.Like(d)
	return "(" + alias + "title " + op + ` ? ESCAPE '\' OR ` + alias + "original_title " + op + ` ? ESCAPE '\' OR EXISTS (SELECT 1 FROM catalog_alt_titles a WHERE a.key=` + alias + "key AND a.title " + op + ` ? ESCAPE '\'))`, []any{like, like, like}
}

// PersonMatch is a credited person a people search may return.
type PersonMatch struct {
	TMDB    int64
	Name    string
	Credits int
}

// MatchPeople returns up to limit credited people, most credited first,
// whose name has a word starting with each of terms (lower-case words, as
// split by the caller). The match is a candidate set: on PostgreSQL it is a
// substring match, so callers recheck names with their own word rules.
func (c *Catalog) MatchPeople(ctx context.Context, terms []string, limit int) ([]PersonMatch, error) {
	if len(terms) == 0 || limit <= 0 {
		return nil, nil
	}
	q, args := c.matchPeopleSQL(terms, limit)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonMatch
	for rows.Next() {
		var m PersonMatch
		if err := rows.Scan(&m.TMDB, &m.Name, &m.Credits); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (c *Catalog) matchPeopleSQL(terms []string, limit int) (string, []any) {
	var q string
	var args []any
	if store.DialectOf(c.db) == store.Postgres {
		conds := make([]string, len(terms))
		for i, term := range terms {
			conds[i] = "name ILIKE ? ESCAPE '\\'"
			args = append(args, "%"+escapeLike(term)+"%")
		}
		q = `SELECT person_tmdb, name, credits FROM catalog_person_names WHERE credits>0 AND ` + strings.Join(conds, " AND ") + ` ORDER BY credits DESC, person_tmdb LIMIT ?`
	} else {
		parts := make([]string, len(terms))
		for i, term := range terms {
			parts[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"*`
		}
		args = append(args, strings.Join(parts, " "))
		q = `SELECT p.person_tmdb, p.name, p.credits FROM catalog_person_names_fts f JOIN catalog_person_names p ON p.person_tmdb=f.rowid
			WHERE catalog_person_names_fts MATCH ? AND p.credits>0 ORDER BY p.credits DESC, p.person_tmdb LIMIT ?`
	}
	args = append(args, limit)
	return q, args
}

// SearchPeople returns up to limit credited people matching terms, most credited first.
func (c *Catalog) SearchPeople(ctx context.Context, terms []string, limit int) ([]core.Person, error) {
	matches, err := c.MatchPeople(ctx, terms, limit)
	if err != nil || len(matches) == 0 {
		return nil, err
	}
	out := make([]core.Person, len(matches))
	for i, m := range matches {
		out[i] = core.Person{TMDB: m.TMDB, Name: m.Name}
	}
	return out, nil
}

// PersonCreditKeys returns up to limit titles of types that credit person,
// most popular first. Like the credit walk it replaces, it includes titles
// opened on demand (not browsable); callers apply the viewer's visibility.
func (c *Catalog) PersonCreditKeys(ctx context.Context, person int64, types []core.MediaType, limit int) ([]core.RatingKey, error) {
	if person <= 0 || limit <= 0 || len(types) == 0 {
		return nil, nil
	}
	q, args := personCreditKeysSQL(person, types, limit)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.RatingKey
	for rows.Next() {
		var key int64
		var popularity float64
		if err := rows.Scan(&key, &popularity); err != nil {
			return nil, err
		}
		out = append(out, core.RatingKey(key))
	}
	return out, rows.Err()
}

func personCreditKeysSQL(person int64, types []core.MediaType, limit int) (string, []any) {
	in := make([]string, 0, len(types))
	args := []any{person}
	for _, t := range types {
		in = append(in, "?")
		args = append(args, string(t))
	}
	args = append(args, limit)
	q := `SELECT DISTINCT t.key, t.popularity FROM catalog_credits cr JOIN catalog_titles t ON t.key=cr.key
		WHERE cr.person_tmdb=? AND t.adult=0 AND t.type IN (` + strings.Join(in, ",") + `)
		ORDER BY t.popularity DESC, t.key LIMIT ?`
	return q, args
}
