package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/jackc/pgx/v5"
)

func execDDL(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if store.DialectOf(db) == store.Postgres {
		stmt = store.TranslateDDL(stmt)
	}
	if _, err := db.Exec(stmt); err != nil {
		t.Fatal(err)
	}
}

func migrationKeys(t *testing.T, db *sql.DB) map[string]bool {
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
		got[c+"/"+strconv.Itoa(v)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func requireMigrations(t *testing.T, db *sql.DB, want []string, absent []string) {
	t.Helper()
	got := migrationKeys(t, db)
	for _, k := range want {
		if !got[k] {
			t.Fatalf("missing %s in %v", k, got)
		}
	}
	for _, k := range absent {
		if got[k] {
			t.Fatalf("unexpected %s in %v", k, got)
		}
	}
}

// TestFreshStatePlexwatchSchema migrates an empty database. state/1 and
// state/2 stay the watch tables. Prism already owns state/3 and state/4.
// The account-sync additions are state_plexwatch/1 through /3, not state/5.
func TestFreshStatePlexwatchSchema(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		assertFreshSchema(t, ctx, db)
	})
	t.Run("postgres", func(t *testing.T) {
		raw := os.Getenv(store.TestPostgresEnv)
		if raw == "" {
			t.Skip("upgrade fixture needs PostgreSQL (ETHER_TEST_POSTGRES)")
		}
		db := openUpgradePostgres(t, raw)
		assertFreshSchema(t, ctx, db)
	})
}

func assertFreshSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	requireMigrations(t, db, []string{
		"state/1", "state/2", "state/3", "state/4",
		"state_plexwatch/1", "state_plexwatch/2", "state_plexwatch/3",
	}, []string{"state/5"})
	var watchAt, ratingAt, consent int64
	if err := db.QueryRow(`SELECT watch_at, rating_at FROM state_watch WHERE 0=1`).Scan(&watchAt, &ratingAt); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT consent_generation FROM state_sync_journal WHERE 0=1`).Scan(&consent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	var holder string
	if err := db.QueryRow(`SELECT holder FROM state_sync_lock WHERE 0=1`).Scan(&holder); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}

// TestOldBinaryWriteSurvivesUpgrade starts from the state/2 shape, applies
// the newer migrations, then runs the previous binary's UPDATE (it bumps
// updated_at and version and does not write watch_at or rating_at). A later
// import dated between the backfill and that write must not replace the
// local progress, watched mark, or rating.
func TestOldBinaryWriteSurvivesUpgrade(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		assertUpgradeKeepsOldWrite(t, ctx, db)
	})
	t.Run("postgres", func(t *testing.T) {
		raw := os.Getenv(store.TestPostgresEnv)
		if raw == "" {
			t.Skip("upgrade fixture needs PostgreSQL (ETHER_TEST_POSTGRES)")
		}
		db := openUpgradePostgres(t, raw)
		assertUpgradeKeepsOldWrite(t, ctx, db)
	})
}

func assertUpgradeKeepsOldWrite(t *testing.T, ctx context.Context, db *sql.DB) {
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
		`CREATE TABLE schema_migrations (
  component TEXT NOT NULL,
  version INTEGER NOT NULL,
  PRIMARY KEY (component, version)
)`,
	} {
		execDDL(t, db, stmt)
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
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	requireMigrations(t, db, []string{
		"state/1", "state/2", "state/3", "state/4",
		"state_plexwatch/1", "state_plexwatch/2", "state_plexwatch/3",
	}, []string{"state/5"})
	var watchAt, ratingAt int64
	if err := db.QueryRow(`SELECT watch_at, rating_at FROM state_watch WHERE user_id=7 AND rating_key=?`, int64(key)).Scan(&watchAt, &ratingAt); err != nil {
		t.Fatal(err)
	}
	if watchAt != t1.UnixNano() || ratingAt != t1.UnixNano() {
		t.Fatalf("backfill watch_at %d rating_at %d", watchAt, ratingAt)
	}
	t3 := t1.Add(2 * time.Hour)
	// The previous binary's save: no watch_at or rating_at.
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
	if err := db.QueryRow(`SELECT watch_at, rating_at FROM state_watch WHERE user_id=7`).Scan(&watchAt, &ratingAt); err != nil {
		t.Fatal(err)
	}
	if watchAt != t1.UnixNano() || ratingAt != t1.UnixNano() {
		t.Fatalf("old writer moved the new clocks watch_at %d rating_at %d", watchAt, ratingAt)
	}
	s := New(db, Options{})
	t2 := t1.Add(time.Hour)
	watched := true
	rating := 8.0
	if err := s.ApplyRemote(ctx, 7, key, &watched, t2, &rating, t2); err != ErrLocalNewer {
		t.Fatalf("stale clocks accepted the import: %v", err)
	}
	ws, err := s.Get(ctx, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ViewCount != 2 || ws.ViewOffset != 45*time.Second || ws.UserRating != 10 || !ws.LastViewedAt.Equal(t3) {
		t.Fatalf("local write lost %+v", ws)
	}
	t4 := t3.Add(time.Hour)
	rating = 4
	if err := s.ApplyRemote(ctx, 7, key, &watched, t4, &rating, t4); err != nil {
		t.Fatal(err)
	}
	ws, err = s.Get(ctx, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	if ws.UserRating != 4 || ws.ViewCount != 2 {
		t.Fatalf("newer remote did not apply %+v", ws)
	}
}

// Unreleased builds of this slice registered the journal as state/3, the
// clocks as state/4, and consent_generation as state/5. Those numbers are
// Prism's on origin/prism b4ef791d. No released installation has this slice,
// and the owned databases checked for this repair had not applied it.
// Startup does not rewrite schema_migrations. This fixture is the dev-only
// bookkeeping for a database that already has the objects: record
// state_plexwatch/1-3, drop the unreleased state/3-5 rows, then let Prism's
// state/3 and state/4 create their own tables. Watch rows stay.
func TestDevOnlyUnreleasedIdentityConverts(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		db.SetMaxOpenConns(1)
		assertUnreleasedIdentityConverts(t, ctx, db)
	})
	t.Run("postgres", func(t *testing.T) {
		raw := os.Getenv(store.TestPostgresEnv)
		if raw == "" {
			t.Skip("upgrade fixture needs PostgreSQL (ETHER_TEST_POSTGRES)")
		}
		db := openUpgradePostgres(t, raw)
		assertUnreleasedIdentityConverts(t, ctx, db)
	})
}

func assertUnreleasedIdentityConverts(t *testing.T, ctx context.Context, db *sql.DB) {
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
		`CREATE TABLE schema_migrations (
  component TEXT NOT NULL,
  version INTEGER NOT NULL,
  PRIMARY KEY (component, version)
)`,
		unreleasedJournalSQL,
		unreleasedClockSQL,
		unreleasedConsentSQL,
	} {
		execDDL(t, db, stmt)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(component, version) VALUES
		('state', 1),('state', 2),('state', 3),('state', 4),('state', 5)`); err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	key := core.MovieKey(42)
	if _, err := db.Exec(`INSERT INTO state_watch(user_id, rating_key, view_offset_ns, duration_ns, view_count, counted, last_viewed_at, user_rating, updated_at, version, watch_at, rating_at)
		VALUES(7, ?, ?, ?, 1, 0, ?, 6, ?, 1, ?, ?)`,
		int64(key), int64(20*time.Second), int64(2*time.Hour), t1.UnixNano(), t1.UnixNano(), t1.UnixNano(), t1.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_playlists(user_id, title, updated_at) VALUES(7, 'Keep', ?)`, t1.UnixNano()); err != nil {
		t.Fatal(err)
	}
	convertUnreleasedDevIdentity(t, db)
	for _, stmt := range []string{prismPlaylistMarkersSQL, prismLastSourceSQL} {
		execDDL(t, db, stmt)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(component, version) VALUES('state', 3),('state', 4)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	requireMigrations(t, db, []string{
		"state/1", "state/2", "state/3", "state/4",
		"state_plexwatch/1", "state_plexwatch/2", "state_plexwatch/3",
	}, []string{"state/5"})
	var offset, views int64
	var rating float64
	var title string
	if err := db.QueryRow(`SELECT view_offset_ns, view_count, user_rating FROM state_watch WHERE user_id=7`).Scan(&offset, &views, &rating); err != nil {
		t.Fatal(err)
	}
	if offset != int64(20*time.Second) || views != 1 || rating != 6 {
		t.Fatalf("watch row changed offset=%d views=%d rating=%v", offset, views, rating)
	}
	if err := db.QueryRow(`SELECT title FROM state_playlists WHERE user_id=7`).Scan(&title); err != nil || title != "Keep" {
		t.Fatalf("playlist %q %v", title, err)
	}
	var playlistID int64
	if err := db.QueryRow(`SELECT id FROM state_playlists WHERE user_id=7`).Scan(&playlistID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO state_playlist_markers(user_id, marker, playlist_id) VALUES(7, 'watch', ?)`, playlistID); err != nil {
		t.Fatal(err)
	}
	var sourceUser int64
	if err := db.QueryRow(`SELECT user_id FROM state_last_source WHERE 0=1`).Scan(&sourceUser); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}

func convertUnreleasedDevIdentity(t *testing.T, db *sql.DB) {
	t.Helper()
	var seq, watchAt, consent int64
	if err := db.QueryRow(`SELECT seq FROM state_sync_journal WHERE 0=1`).Scan(&seq); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT watch_at FROM state_watch WHERE 0=1`).Scan(&watchAt); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT consent_generation FROM state_sync_journal WHERE 0=1`).Scan(&consent); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	var playlistID int64
	err := db.QueryRow(`SELECT playlist_id FROM state_playlist_markers LIMIT 1`).Scan(&playlistID)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatal("state_playlist_markers exists; this is not the unreleased journal identity")
	}
	got := migrationKeys(t, db)
	for _, k := range []string{"state/3", "state/4", "state/5"} {
		if !got[k] {
			t.Fatalf("unreleased identity missing %s", k)
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(component, version) VALUES
		('state_plexwatch', 1),('state_plexwatch', 2),('state_plexwatch', 3)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE component='state' AND version IN (3, 4, 5)`); err != nil {
		t.Fatal(err)
	}
}

// SQL as registered by the unreleased slice under component state. The
// objects are identical to state_plexwatch/1-3. Only the recorded identity
// was wrong.
const unreleasedJournalSQL = `
CREATE TABLE state_sync_journal (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  aspect TEXT NOT NULL,
  kind TEXT NOT NULL,
  rating REAL NOT NULL DEFAULT 0,
  occurred_at INTEGER NOT NULL,
  bulk INTEGER NOT NULL DEFAULT 0,
  superseded INTEGER NOT NULL DEFAULT 0,
  claimed_by TEXT NOT NULL DEFAULT '',
  claimed_at INTEGER NOT NULL DEFAULT 0,
  delivered_at INTEGER NOT NULL DEFAULT 0,
  dead INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX state_sync_journal_live ON state_sync_journal(dead, superseded, delivered_at, next_attempt, seq);
CREATE TABLE state_sync_lock (
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  aspect TEXT NOT NULL,
  holder TEXT NOT NULL DEFAULT '',
  until_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, rating_key, aspect)
)`

const unreleasedClockSQL = `
ALTER TABLE state_watch ADD COLUMN watch_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE state_watch ADD COLUMN rating_at INTEGER NOT NULL DEFAULT 0`

const unreleasedConsentSQL = `
ALTER TABLE state_sync_journal ADD COLUMN consent_generation INTEGER NOT NULL DEFAULT 0`

// origin/prism b4ef791d state/3 and state/4, copied so this branch can
// prove both schemas on one database without taking those version numbers.
const prismPlaylistMarkersSQL = `CREATE TABLE state_playlist_markers (user_id INTEGER NOT NULL, marker TEXT NOT NULL, playlist_id INTEGER NOT NULL UNIQUE, PRIMARY KEY(user_id,marker), FOREIGN KEY(playlist_id) REFERENCES state_playlists(id) ON DELETE CASCADE);`

const prismLastSourceSQL = `
CREATE TABLE state_source_sequence (id INTEGER PRIMARY KEY AUTOINCREMENT);
CREATE TABLE state_last_source (
 user_id INTEGER NOT NULL, rating_key INTEGER NOT NULL, session_order INTEGER NOT NULL, sequence INTEGER NOT NULL,
 revision TEXT NOT NULL, release_digest TEXT NOT NULL, video_codec TEXT NOT NULL,
 height INTEGER NOT NULL, hdr INTEGER, PRIMARY KEY(user_id,rating_key)
);`

func openUpgradePostgres(t *testing.T, raw string) *sql.DB {
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
