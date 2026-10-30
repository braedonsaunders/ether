package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

type upgradeTable struct {
	columns []string
	rows    []string
}

// Capture every delivered table, including empty tables and default seed rows.
// Compare its original columns after upgrade so additive columns cannot hide a
// changed credential, preference, cluster lease, or session payload.
func captureUpgradeTables(t *testing.T, db *sql.DB, dialect store.Dialect) map[string]upgradeTable {
	t.Helper()
	query := `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name`
	if dialect == store.Postgres {
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name<>'schema_migrations' ORDER BY table_name`
	}
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	result := make(map[string]upgradeTable)
	for _, name := range names {
		rows, err := db.Query(`SELECT * FROM ` + quoteUpgradeName(name) + ` WHERE 1=0`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		result[name] = upgradeTable{columns: columns, rows: readUpgradeRows(t, db, name, columns)}
	}
	return result
}

func quoteUpgradeName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func readUpgradeRows(t *testing.T, db *sql.DB, table string, columns []string) []string {
	t.Helper()
	quoted := make([]string, len(columns))
	for i, col := range columns {
		quoted[i] = quoteUpgradeName(col)
	}
	rows, err := db.Query(`SELECT ` + strings.Join(quoted, ",") + ` FROM ` + quoteUpgradeName(table))
	if err != nil {
		t.Fatalf("table %s: %v", table, err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		values, dest := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			if b, ok := value.([]byte); ok {
				values[i] = string(b)
			}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, string(encoded))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(result)
	return result
}

func assertUpgradeTables(t *testing.T, db *sql.DB, before map[string]upgradeTable) {
	t.Helper()
	for name, table := range before {
		if got := readUpgradeRows(t, db, name, table.columns); !reflect.DeepEqual(got, table.rows) {
			// Fixture credentials are synthetic; include the whole row to identify the lost field.
			t.Errorf("delivered table %s columns %v: got %v want %v", name, table.columns, got, table.rows)
		}
	}
}

func assertUpgradeInventory(t *testing.T, db *sql.DB) {
	t.Helper()
	assertPrismCandidateBoundary(t)
	known := store.KnownMigrations()
	for _, reserved := range []string{"state_plexwatch", "plexwatch"} {
		if _, ok := known[reserved]; ok {
			t.Fatalf("reserved component %s is registered", reserved)
		}
	}
	applied, err := store.AppliedMigrations(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, known) {
		t.Fatalf("migration inventory got %v want %v", applied, known)
	}
	for _, step := range loadDelivered9503(t) {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component=? AND version=?`, step.Component, step.Version).Scan(&count); err != nil || count != 1 {
			t.Fatalf("delivered migration record %s/%d: %d %v", step.Component, step.Version, count, err)
		}
	}
}

func TestPrismUpgradeSQLiteLateFailureRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	buildDeliveredDB(t, dir, func(q sqlQuery) {
		if _, err := q.ExecContext(ctx, `CREATE INDEX catalog_titles_episode_aired ON catalog_titles(key)`); err != nil {
			t.Fatal(err)
		}
	})
	path := filepath.Join(dir, "ether.db")
	original := openSQLiteRO(t, path)
	before := captureUpgradeTables(t, original, store.SQLite)
	original.Close()
	db, _, err := store.OpenWith(ctx, store.Options{DataDir: dir})
	if err == nil {
		db.Close()
		t.Fatal("index collision did not fail catalog/10")
	}
	if !strings.Contains(err.Error(), "catalog/10") {
		t.Fatal(err)
	}
	raw := openSQLiteRW(t, path)
	assertUpgradeTables(t, raw, before)
	if got := maxMigration(t, raw, "auth"); got != 7 {
		t.Fatalf("completed auth steps lost: %d", got)
	}
	if got := maxMigration(t, raw, "catalog"); got != 9 {
		t.Fatalf("failed catalog step was recorded: %d", got)
	}
	// The second index in catalog/10 must not exist after the first fails.
	var indexes int
	if err := raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='catalog_titles_season_episode_aired'`).Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("failed step retained index: %d %v", indexes, err)
	}
	if _, err := raw.Exec(`DROP INDEX catalog_titles_episode_aired`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, _, err = store.OpenWith(ctx, store.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("repaired upgrade: %v", err)
	}
	defer db.Close()
	assertUpgradeTables(t, db, before)
	assertUpgradeInventory(t, db)
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
	assertUpgradeTables(t, db, before)
}

// Fail bookkeeping for the final pending migration, after all preceding
// Prism components have run. This covers more than an early auth/catalog
// failure and automatically follows newly integrated component versions.
func TestPrismUpgradeFinalStepFailure(t *testing.T) {
	for _, dialect := range []store.Dialect{store.SQLite, store.Postgres} {
		t.Run(string(dialect), func(t *testing.T) { verifyPrismMigrationFailure(t, dialect, "", 0) })
	}
}

func verifyPrismMigrationFailure(t *testing.T, dialect store.Dialect, target string, targetVersion int) {
	t.Helper()
	ctx := context.Background()
	var db *sql.DB
	if dialect == store.Postgres {
		dsn := os.Getenv(store.TestPostgresEnv)
		if dsn == "" {
			t.Skip("ETHER_TEST_POSTGRES is not configured")
		}
		db = newPostgresSchema(t, dsn)
		applyDelivered9503(t, db, dialect)
		seedDeliveredRows(t, db)
	} else {
		dir := t.TempDir()
		buildDeliveredDB(t, dir, nil)
		db = openSQLiteRW(t, filepath.Join(dir, "ether.db"))
		defer db.Close()
	}
	before := captureUpgradeTables(t, db, dialect)
	applied, err := store.AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	known := store.KnownMigrations()
	last := ""
	for _, component := range store.ComponentOrder() {
		if known[component] > applied[component] {
			last = component
		}
	}
	if last == "" {
		t.Fatal("fixture has no pending migrations")
	}
	version := known[last]
	if target != "" {
		last = target
		version = targetVersion
	}
	t.Logf("inject failure after DDL for final pending migration %s/%d", last, version)
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_final_migration BEFORE INSERT ON schema_migrations WHEN NEW.component='%s' AND NEW.version=%d BEGIN SELECT RAISE(ABORT,'acceptance final-step failure'); END`, last, version)
	if dialect == store.Postgres {
		trigger = fmt.Sprintf(`CREATE FUNCTION reject_final_migration() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.component='%s' AND NEW.version=%d THEN RAISE EXCEPTION 'acceptance final-step failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_final_migration BEFORE INSERT ON schema_migrations FOR EACH ROW EXECUTE FUNCTION reject_final_migration()`, last, version)
	}
	if _, err := db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	err = store.Migrate(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "acceptance final-step failure") {
		t.Fatalf("injected failure: %v", err)
	}
	assertUpgradeTables(t, db, before)
	got, err := store.AppliedMigrations(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if dialect == store.Postgres {
		if !reflect.DeepEqual(got, applied) || !reflect.DeepEqual(captureUpgradeTables(t, db, dialect), before) {
			t.Fatal("final failure retained pending migrations or changed delivered schema")
		}
	} else {
		beforeFailure := true
		for _, component := range store.ComponentOrder() {
			v := applied[component]
			if component == last {
				v = version - 1
				beforeFailure = false
			} else if beforeFailure {
				v = known[component]
			}
			if got[component] != v {
				t.Fatalf("SQLite completed prefix %s=%d want %d", component, got[component], v)
			}
		}
	}
	drop := `DROP TRIGGER reject_final_migration`
	if dialect == store.Postgres {
		drop += ` ON schema_migrations`
	}
	if _, err := db.Exec(drop); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("final-step recovery: %v", err)
	}
	assertUpgradeTables(t, db, before)
	assertUpgradeInventory(t, db)
}
