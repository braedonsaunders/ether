package prismcompat_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/jackc/pgx/v5"

	_ "github.com/braedonsaunders/ether/internal/plexwatch"
)

// TestPrismStateAndPlexwatchSchema migrates Prism state/3 and state/4
// together with state_plexwatch/1-5 and the plexwatch account component.
// Fresh and state/2 baselines both keep their rows, including the final
// pre-clock write repaired by state_plexwatch/5.
func TestPrismStateAndPlexwatchSchema(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		db := openSQLite(t)
		assertCombinedFresh(t, ctx, db)
	})
	t.Run("postgres", func(t *testing.T) {
		raw := os.Getenv(store.TestPostgresEnv)
		if raw == "" {
			t.Skip("combined schema needs PostgreSQL (ETHER_TEST_POSTGRES)")
		}
		db := openSchemaPostgres(t, raw)
		assertCombinedFresh(t, ctx, db)
	})
	t.Run("sqlite_from_state2", func(t *testing.T) {
		db := openSQLite(t)
		assertCombinedFromState2(t, ctx, db)
	})
	t.Run("postgres_from_state2", func(t *testing.T) {
		raw := os.Getenv(store.TestPostgresEnv)
		if raw == "" {
			t.Skip("combined schema needs PostgreSQL (ETHER_TEST_POSTGRES)")
		}
		db := openSchemaPostgres(t, raw)
		assertCombinedFromState2(t, ctx, db)
	})
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func assertCombinedFresh(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertCombinedObjects(t, db)
	if _, err := db.Exec(`INSERT INTO state_playlists(user_id, title) VALUES(7, 'Fresh')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM state_playlists WHERE title='Fresh'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_playlist_markers(user_id, marker, playlist_id) VALUES(7, 'keep', ?)`, id); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(42)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	if _, err := db.Exec(`INSERT INTO state_watch(user_id, rating_key, view_count, user_rating, last_viewed_at, updated_at, watch_at, rating_at)
		VALUES(7, ?, 1, 6, ?, ?, ?, ?)`, int64(key), now, now, now, now); err != nil {
		t.Fatal(err)
	}
	var marker string
	var views int64
	if err := db.QueryRow(`SELECT marker FROM state_playlist_markers WHERE user_id=7`).Scan(&marker); err != nil || marker != "keep" {
		t.Fatalf("marker %q %v", marker, err)
	}
	if err := db.QueryRow(`SELECT view_count FROM state_watch WHERE user_id=7`).Scan(&views); err != nil || views != 1 {
		t.Fatalf("views %d %v", views, err)
	}
}

func assertCombinedFromState2(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE state_watch (
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  view_offset_ns INTEGER NOT NULL DEFAULT 0,
  duration_ns INTEGER NOT NULL DEFAULT 0,
  view_count INTEGER NOT NULL DEFAULT 0,
  counted INTEGER NOT NULL DEFAULT 0,
  last_viewed_at INTEGER NOT NULL DEFAULT 0,
  user_rating REAL NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  version INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, rating_key)
)`,
		`CREATE TABLE state_playlists (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  title TEXT NOT NULL,
  smart INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE TABLE state_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  viewed_at INTEGER NOT NULL,
  completed INTEGER NOT NULL DEFAULT 0,
  imported INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE INDEX state_history_user ON state_history(user_id, viewed_at DESC)`,
		`CREATE UNIQUE INDEX state_history_imported ON state_history(user_id, rating_key, viewed_at) WHERE imported=1`,
		`CREATE INDEX state_watch_inprogress ON state_watch(user_id, last_viewed_at DESC) WHERE view_offset_ns > 0`,
		`CREATE TABLE state_playlist_items (
  playlist_id INTEGER NOT NULL REFERENCES state_playlists(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  PRIMARY KEY (playlist_id, position)
)`,
		`CREATE TABLE schema_migrations (
  component TEXT NOT NULL,
  version INTEGER NOT NULL,
  PRIMARY KEY (component, version)
)`,
	} {
		if store.DialectOf(db) == store.Postgres {
			stmt = store.TranslateDDL(stmt)
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(component, version) VALUES('state', 1),('state', 2)`); err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	key := core.MovieKey(42)
	if _, err := db.Exec(`INSERT INTO state_watch(user_id, rating_key, view_offset_ns, duration_ns, view_count, counted, last_viewed_at, user_rating, updated_at, version)
		VALUES(7, ?, ?, ?, 1, 0, ?, 6, ?, 1)`, int64(key), int64(20*time.Second), int64(2*time.Hour), t1.UnixNano(), t1.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_playlists(user_id, title) VALUES(7, 'Baseline')`); err != nil {
		t.Fatal(err)
	}
	// Establish the old clock-bearing schema, then leave its repair pending.
	// This models a pre-upgrade writer advancing the row but not its clocks.
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE component='state_plexwatch' AND version=5`); err != nil {
		t.Fatal(err)
	}
	t3 := t1.Add(2 * time.Hour)
	res, err := db.Exec(`UPDATE state_watch SET view_offset_ns=?, duration_ns=?, view_count=?, counted=?,
		last_viewed_at=?, user_rating=?, updated_at=?, version=version+1
		WHERE user_id=7 AND rating_key=? AND version=1`,
		int64(45*time.Second), int64(2*time.Hour), 2, 0, t3.UnixNano(), 10.0, t3.UnixNano(), int64(key))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("old writer updated %d", n)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertCombinedObjects(t, db)
	var watchAt, ratingAt, views int64
	if err := db.QueryRow(`SELECT watch_at, rating_at, view_count FROM state_watch WHERE user_id=7`).Scan(&watchAt, &ratingAt, &views); err != nil {
		t.Fatal(err)
	}
	if views != 2 {
		t.Fatalf("view count %d after migration", views)
	}
	if watchAt != t3.UnixNano() || ratingAt != t3.UnixNano() {
		t.Fatalf("backfill watch_at %d rating_at %d", watchAt, ratingAt)
	}
	s := state.New(db, state.Options{})
	watched := true
	rating := 8.0
	t2 := t1.Add(time.Hour)
	if err := s.ApplyRemote(ctx, 7, key, &watched, t2, &rating, t2); err != state.ErrLocalNewer {
		t.Fatalf("stale clocks accepted the import: %v", err)
	}
	ws, err := s.Get(ctx, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ViewCount != 2 || ws.ViewOffset != 45*time.Second || ws.UserRating != 10 || !ws.LastViewedAt.Equal(t3) {
		t.Fatalf("local write lost %+v", ws)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM state_playlists WHERE user_id=7`).Scan(&title); err != nil || title != "Baseline" {
		t.Fatalf("playlist %q %v", title, err)
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM state_playlists WHERE user_id=7`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_playlist_markers(user_id, marker, playlist_id) VALUES(7, 'keep', ?)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_last_source(user_id, rating_key, session_order, sequence, revision, release_digest, video_codec, height)
		VALUES(7, ?, 1, 1, 'rev', 'digest', 'hevc', 1080)`, int64(key)); err != nil {
		t.Fatal(err)
	}
}

func assertCombinedObjects(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT component, version FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			t.Fatal(err)
		}
		got[c+"/"+itoa(v)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"state/1", "state/2", "state/3", "state/4",
		"state_plexwatch/1", "state_plexwatch/2", "state_plexwatch/3", "state_plexwatch/4", "state_plexwatch/5", "state_plexwatch/6",
		"plexwatch/1", "plexwatch/2", "plexwatch/3", "plexwatch/4", "plexwatch/5", "plexwatch/6", "plexwatch/7",
		"watchsend/1", "watchsend/2",
	} {
		if !got[k] {
			t.Fatalf("missing %s in %v", k, got)
		}
	}
	if got["state/5"] {
		t.Fatalf("state/5 still recorded: %v", got)
	}
	probes := []string{
		`SELECT watch_at, rating_at FROM state_watch WHERE 0=1`,
		`SELECT consent_generation, never_sent, offset_ms, duration_ms FROM state_sync_journal WHERE 0=1`,
		`SELECT holder FROM state_sync_lock WHERE 0=1`,
		`SELECT marker FROM state_playlist_markers WHERE 0=1`,
		`SELECT revision FROM state_last_source WHERE 0=1`,
		`SELECT id FROM state_source_sequence WHERE 0=1`,
		`SELECT token FROM plexwatch_account WHERE 0=1`,
		`SELECT inflight_until FROM plexwatch_fence WHERE 0=1`,
		`SELECT watched, viewed_at, view_offset, view_state, rating, leaves, observed_at, ours FROM plexwatch_remote WHERE 0=1`,
		`SELECT after_key FROM plexwatch_expand WHERE 0=1`,
		`SELECT last_sweep, consent, consent_checked, active_after FROM plexwatch_account WHERE 0=1`,
	}
	for _, q := range probes {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows.Close()
	}
	// The scrobbles.plex.tv transport's hold, permit, echo and unmatched
	// tables are gone (plexwatch/6, watchsend/2).
	for _, table := range []string{"plexwatch_attempt", "plexwatch_permit", "plexwatch_sent", "plexwatch_unmapped"} {
		if rows, err := db.Query(`SELECT 1 FROM ` + table + ` WHERE 0=1`); err == nil {
			rows.Close()
			t.Fatalf("%s still exists", table)
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	n := v
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func openSchemaPostgres(t *testing.T, raw string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	schema := "ether_test_upg_" + hex.EncodeToString(rnd[:])
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+ident); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = admin.Exec(cctx, `DROP SCHEMA IF EXISTS `+ident+` CASCADE`)
		admin.Close(context.Background())
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open(store.PostgresDriverName, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(4)
	var path string
	if err := db.QueryRow(`SHOW search_path`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != schema {
		t.Fatalf("search_path %q, refusing to migrate", path)
	}
	return db
}
