package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/store"
)

// releasedMainCommit is origin/main 4588b9893, the released migration set a
// v0.3 upgrade must open. testdata/main-upgrade is that commit's registered
// SQL, extracted from source. Prism-only versions are absent.
const releasedMainCommit = "4588b98939bd5becd5136e1eac37da13b14d50c0"

func TestUpgradeFromReleasedMain(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, db *sql.DB, dialect store.Dialect) {
		t.Helper()
		applyReleasedMain(t, db, dialect)
		if _, err := db.ExecContext(ctx, `INSERT INTO playback_resume(user_id,rating_key,release_id,release_json,nzb_hash,updated_at,original_language) VALUES(7,1000000000010001,'rel','{}','abc',3,'ja')`); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx, db); err != nil {
			t.Fatalf("upgrade from released main: %v", err)
		}
		var lang string
		if err := db.QueryRowContext(ctx, `SELECT original_language FROM playback_resume WHERE user_id=7 AND rating_key=1000000000010001`).Scan(&lang); err != nil || lang != "ja" {
			t.Fatalf("original_language=%q err=%v", lang, err)
		}
		for _, table := range []string{"playback_payload_incarnations", "playback_payload_owners", "playback_payload_deleting"} {
			if !upgradeTableExists(t, db, dialect, table) {
				t.Fatalf("missing prism table %s", table)
			}
		}
		if !upgradeColumnExists(t, db, dialect, "playback_resume", "original_language") {
			t.Fatal("missing released column playback_resume.original_language")
		}
		for version := 1; version <= 4; version++ {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE component=? AND version=?`, "playback", version).Scan(&n); err != nil || n != 1 {
				t.Fatalf("playback/%d recorded=%d err=%v", version, n, err)
			}
		}
		applied, err := store.AppliedMigrations(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if known := store.KnownMigrations(); !reflect.DeepEqual(applied, known) {
			t.Fatalf("migration inventory got %v want %v", applied, known)
		}
		if err := store.Migrate(ctx, db); err != nil {
			t.Fatalf("second open: %v", err)
		}
	}

	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		db := openSQLiteRW(t, filepath.Join(dir, "ether.db"))
		t.Cleanup(func() { db.Close() })
		run(t, db, store.SQLite)
	})
	if dsn := os.Getenv(store.TestPostgresEnv); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			run(t, newPostgresSchema(t, dsn), store.Postgres)
		})
	}
}

func applyReleasedMain(t *testing.T, db sqlQuery, dialect store.Dialect) {
	t.Helper()
	steps := loadReleasedMain(t, dialect)
	ctx := context.Background()
	create := `CREATE TABLE schema_migrations (component TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY (component, version))`
	if dialect == store.Postgres {
		create = store.TranslateDDL(create)
	}
	if _, err := db.ExecContext(ctx, create); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if _, err := db.ExecContext(ctx, step.SQL); err != nil {
			t.Fatalf("apply %s/%d: %v", step.Component, step.Version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(component, version) VALUES (?, ?)`, step.Component, step.Version); err != nil {
			t.Fatal(err)
		}
	}
	if got := maxMigration(t, db, "playback"); got != 3 {
		t.Fatalf("released playback head=%d, want 3", got)
	}
}

func loadReleasedMain(t *testing.T, dialect store.Dialect) []deliveredStep {
	t.Helper()
	dir := filepath.Join("testdata", "main-upgrade")
	filename := "schema.sql"
	if dialect == store.Postgres {
		filename = "schema-postgres.sql"
	}
	schema, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(schema)
	var manifest struct {
		Commit         string `json:"commit"`
		SHA256         string `json:"sha256"`
		PostgresSHA256 string `json:"postgres_sha256"`
		Migrations     []struct {
			Component string `json:"component"`
			Version   int    `json:"version"`
		} `json:"migrations"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	expected := manifest.SHA256
	if dialect == store.Postgres {
		expected = manifest.PostgresSHA256
	}
	if manifest.Commit != releasedMainCommit || expected != hex.EncodeToString(sum[:]) {
		t.Fatalf("fixture provenance commit %s sha %s", manifest.Commit, expected)
	}
	var playback3 string
	parts := strings.Split(string(schema), "\n-- prism-upgrade-migration-end\n")
	var steps []deliveredStep
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "-- migration ") {
			continue
		}
		line, sqlText, ok := strings.Cut(part, "\n")
		for ok && strings.HasPrefix(strings.TrimSpace(line), "--") && !strings.HasPrefix(strings.TrimSpace(line), "-- migration ") {
			line, sqlText, ok = strings.Cut(sqlText, "\n")
		}
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "-- migration "))
		if len(fields) != 2 {
			t.Fatalf("migration header %q", line)
		}
		var version int
		if _, err := parseVersion(fields[1], &version); err != nil {
			t.Fatal(err)
		}
		sqlText = strings.TrimSpace(sqlText)
		if fields[0] == "playback" && version == 3 {
			playback3 = sqlText
		}
		steps = append(steps, deliveredStep{Component: fields[0], Version: version, SQL: sqlText})
	}
	if !strings.Contains(playback3, "original_language") || strings.Contains(playback3, "playback_payload") {
		t.Fatalf("released playback/3 is %q", playback3)
	}
	if len(steps) != len(manifest.Migrations) {
		t.Fatalf("schema has %d migrations, manifest %d", len(steps), len(manifest.Migrations))
	}
	for i, step := range steps {
		if step.Component != manifest.Migrations[i].Component || step.Version != manifest.Migrations[i].Version {
			t.Fatalf("migration %d is %s/%d, manifest %s/%d", i, step.Component, step.Version, manifest.Migrations[i].Component, manifest.Migrations[i].Version)
		}
	}
	return steps
}

func upgradeTableExists(t *testing.T, db *sql.DB, dialect store.Dialect, name string) bool {
	t.Helper()
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`
	if dialect == store.Postgres {
		query = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?`
	}
	var n int
	if err := db.QueryRowContext(context.Background(), query, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func upgradeColumnExists(t *testing.T, db *sql.DB, dialect store.Dialect, table, column string) bool {
	t.Helper()
	query := `SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name=?`
	args := []any{column}
	if dialect == store.Postgres {
		query = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=? AND column_name=?`
		args = []any{table, column}
	}
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}
