package libraries

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// convertPredicates upgrades each old predicate exactly once, atomically. The
// old column is cleared and never consulted by any serving path.
func (s *Service) convertPredicates(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done string
	if err := tx.QueryRowContext(ctx, `SELECT v FROM libraries_state WHERE k='visible-rules-upgrade'`).Scan(&done); err == nil && done == "1" {
		return tx.Commit()
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	// Concurrent cluster starts serialize on this claim before touching schema.
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries_state(k,v) VALUES('visible-rules-upgrade','pending') ON CONFLICT(k) DO NOTHING`); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT v FROM libraries_state WHERE k='visible-rules-upgrade'`).Scan(&done); err != nil {
		return err
	}
	if done == "1" {
		return tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, filter, rules, visibility_default FROM libraries WHERE source='filter'`)
	if err != nil {
		return err
	}
	type upgrade struct {
		id                         int64
		predicate, rules, fallback string
	}
	var pending []upgrade
	for rows.Next() {
		var u upgrade
		if err := rows.Scan(&u.id, &u.predicate, &u.rules, &u.fallback); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, u := range pending {
		var f core.Filter
		if err := json.Unmarshal([]byte(u.predicate), &f); err != nil {
			return fmt.Errorf("library %d predicate: %w", u.id, err)
		}
		if f.Type == "" {
			f.Type = core.TypeMovie
		}
		var rules []core.LibraryExclusion
		if err := json.Unmarshal([]byte(u.rules), &rules); err != nil {
			return err
		}
		conditions := core.LibraryPredicateConditions(f)
		if len(rules) > 0 {
			// A leading outside-predicate hide preserves the intersection with every
			// pre-existing rule, including ordered Show rules and either fallback.
			raw, _ := json.Marshal(conditions)
			rules = append([]core.LibraryExclusion{{Action: "hide", MediaType: string(f.Type), Match: "all", Rules: []core.CollectionRule{{Field: "group", Op: "isNot", Value: raw}}}}, rules...)
		}
		if u.fallback != "hide" {
			rules = append(rules, core.LibraryExclusion{Action: "show", MediaType: string(f.Type), Match: "all", Rules: conditions})
		}
		if _, err := libraryfilter.CompilePolicy(rules, "hide"); err != nil {
			return fmt.Errorf("library %d rules: %w", u.id, err)
		}
		data, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		source := "tmdb-movies"
		if f.Type == core.TypeShow {
			source = "tmdb-tv"
		}
		sources, _ := json.Marshal([]string{source})
		if _, err = tx.ExecContext(ctx, `UPDATE libraries SET source='catalogue', media_type=?, sources=?, rules=?, visibility_default='hide', filter='' WHERE id=?`, string(f.Type), string(sources), string(data), u.id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE libraries SET sources='["collection:' || CAST(collection_id AS TEXT) || '"]' WHERE source='collection'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE libraries DROP COLUMN filter`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE libraries_state SET v='1' WHERE k='visible-rules-upgrade'`); err != nil {
		return err
	}
	return tx.Commit()
}
