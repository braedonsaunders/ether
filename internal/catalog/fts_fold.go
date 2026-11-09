package catalog

import (
	"context"
	"database/sql"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// Bulk title writes fold their new rows into the title search index in one
// statement per transaction instead of one FTS5 insert per row from the
// trigger: on a 54k catalogue the per-row FTS upkeep was about half of a new
// title's insert cost, and FTS5 indexes a batch several times cheaper than
// the same rows one call at a time.
//
// catalog_fts_bulk holds a row only inside a bulk transaction, which the
// FTS insert trigger checks: other writers never see it (it is gone before
// commit), so every other insert path keeps the trigger. Search reads the
// folded rows as soon as the transaction commits. Updates and deletes keep
// their triggers; only rows a bulk transaction inserts are folded.
func init() {
	store.RegisterRetiredTable("catalog_fts_bulk")
	store.RegisterAfter("catalog_bulk", "catalog")
	store.Register("catalog_bulk", 1, "SELECT 1;")
	store.RegisterSQLiteApply("catalog_bulk", 1, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='catalog_titles_fts'`).Scan(&n); err != nil || n == 0 {
			return err
		}
		_, err := tx.ExecContext(ctx, `CREATE TABLE catalog_fts_bulk(id INTEGER PRIMARY KEY CHECK(id=1));
DROP TRIGGER IF EXISTS catalog_titles_fts_insert;
CREATE TRIGGER catalog_titles_fts_insert AFTER INSERT ON catalog_titles WHEN NOT EXISTS (SELECT 1 FROM catalog_fts_bulk) BEGIN
 INSERT INTO catalog_titles_fts(rowid, title, original_title) VALUES (new.key, new.title, new.original_title);
END;`)
		return err
	})
	// PostgreSQL's title search is a pg_trgm index on catalog_titles itself.
	store.RegisterDialect("catalog_bulk", 1, store.Postgres, "SELECT 1;")

	// v2: person names fold the same way. Each new credit counted itself
	// into catalog_person_names (and each new person into its FTS index)
	// one row at a time; a bulk transaction now does both in a statement or
	// two after its credits (foldPersonNames). The credit trigger keeps
	// catalog 47's split (count, then rename only when the name differs).
	store.Register("catalog_bulk", 2, "SELECT 1;")
	store.RegisterSQLiteApply("catalog_bulk", 2, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('catalog_fts_bulk','catalog_person_names','catalog_person_names_fts')`).Scan(&n); err != nil || n < 3 {
			return err
		}
		_, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS catalog_person_names_credit_insert;
CREATE TRIGGER catalog_person_names_credit_insert AFTER INSERT ON catalog_credits WHEN new.person_tmdb>0 AND NOT EXISTS (SELECT 1 FROM catalog_fts_bulk) BEGIN
 INSERT INTO catalog_person_names(person_tmdb, name, credits) VALUES (new.person_tmdb, new.name, 1)
 ON CONFLICT(person_tmdb) DO UPDATE SET credits=catalog_person_names.credits+1;
 UPDATE catalog_person_names SET name=new.name WHERE person_tmdb=new.person_tmdb AND new.name<>'' AND name<>new.name;
END;
DROP TRIGGER IF EXISTS catalog_person_names_fts_insert;
CREATE TRIGGER catalog_person_names_fts_insert AFTER INSERT ON catalog_person_names WHEN NOT EXISTS (SELECT 1 FROM catalog_fts_bulk) BEGIN
 INSERT INTO catalog_person_names_fts(rowid, name) VALUES (new.person_tmdb, new.name);
END;`)
		return err
	})
	store.RegisterDialect("catalog_bulk", 2, store.Postgres, "SELECT 1;")
}

// personFoldAvailable reports whether both person-name insert triggers
// honour the bulk marker.
func personFoldAvailable(ctx context.Context, db *sql.DB) bool {
	if store.DialectOf(db) == store.Postgres {
		return false
	}
	for _, name := range []string{"catalog_person_names_credit_insert", "catalog_person_names_fts_insert"} {
		var def string
		if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&def); err != nil || !strings.Contains(def, "catalog_fts_bulk") {
			return false
		}
	}
	return true
}

// personCredit is one inserted credit's person and name, in insert order.
type personCredit struct {
	person int64
	name   string
}

// foldPersonNames does, for credits a bulk transaction inserted, what the
// per-row trigger does: count each credit, and leave each person with the
// last non-empty name credited (a new person starts with its first
// credit's name, empty when none is set). New people are indexed for
// search in one statement.
func foldPersonNames(ctx context.Context, tx *sql.Tx, credits []personCredit) error {
	count := map[int64]int{}
	last := map[int64]string{}
	var people []int64
	for _, c := range credits {
		if c.person <= 0 {
			continue
		}
		if _, ok := count[c.person]; !ok {
			people = append(people, c.person)
		}
		count[c.person]++
		if c.name != "" {
			last[c.person] = c.name
		}
	}
	if len(people) == 0 {
		return nil
	}
	existing := map[int64]string{}
	for start := 0; start < len(people); start += 500 {
		part := people[start:min(start+500, len(people))]
		args := make([]any, len(part))
		for i, p := range part {
			args[i] = p
		}
		rows, err := tx.QueryContext(ctx, `SELECT person_tmdb, name FROM catalog_person_names WHERE person_tmdb IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p int64
			var name string
			if err := rows.Scan(&p, &name); err != nil {
				rows.Close()
				return err
			}
			existing[p] = name
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	var counted, inserted, renamed []any
	var fresh []any
	for _, p := range people {
		name, had := existing[p]
		if !had {
			inserted = append(inserted, p, last[p], count[p])
			fresh = append(fresh, p)
			continue
		}
		counted = append(counted, p, count[p])
		if n := last[p]; n != "" && n != name {
			renamed = append(renamed, p, n)
		}
	}
	for start := 0; start < len(inserted); start += 300 {
		part := inserted[start:min(start+300, len(inserted))]
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_person_names(person_tmdb, name, credits) VALUES `+valueRows(len(part)/3, 3), part...); err != nil {
			return err
		}
	}
	// Counts only: assigning name would rewrite its index entry for nothing.
	for start := 0; start < len(counted); start += 400 {
		part := counted[start:min(start+400, len(counted))]
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_person_names(person_tmdb, credits) VALUES `+valueRows(len(part)/2, 2)+`
 ON CONFLICT(person_tmdb) DO UPDATE SET credits=catalog_person_names.credits+excluded.credits`, part...); err != nil {
			return err
		}
	}
	// Renames are rare; each keeps its own FTS update trigger.
	for i := 0; i < len(renamed); i += 2 {
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_person_names SET name=? WHERE person_tmdb=?`, renamed[i+1], renamed[i]); err != nil {
			return err
		}
	}
	for start := 0; start < len(fresh); start += 500 {
		part := fresh[start:min(start+500, len(fresh))]
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_person_names_fts(rowid, name) SELECT person_tmdb, name FROM catalog_person_names WHERE person_tmdb IN (`+placeholders(len(part))+`)`, part...); err != nil {
			return err
		}
	}
	return nil
}

// ftsFoldMin is the fewest new rows a transaction folds; below it the
// trigger is as cheap.
const ftsFoldMin = 2

// ftsFoldAvailable reports whether the FTS insert trigger honours the bulk
// marker (a database migrated by this binary; never assume it).
func ftsFoldAvailable(ctx context.Context, db *sql.DB) bool {
	if store.DialectOf(db) == store.Postgres {
		return false
	}
	var def string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name='catalog_titles_fts_insert'`).Scan(&def); err != nil {
		return false
	}
	return strings.Contains(def, "catalog_fts_bulk")
}

func beginBulk(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO catalog_fts_bulk(id) VALUES(1)`)
	return err
}

func endBulk(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM catalog_fts_bulk`)
	return err
}

// foldTitleSearch indexes the titles the transaction inserted.
func foldTitleSearch(ctx context.Context, tx *sql.Tx, keys []int64) error {
	for start := 0; start < len(keys); start += 500 {
		part := keys[start:min(start+500, len(keys))]
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = k
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_titles_fts(rowid, title, original_title) SELECT key, title, original_title FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}
