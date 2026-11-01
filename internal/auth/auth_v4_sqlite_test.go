package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Frozen delivered registry from thr_42ixw28kzm commit afdcb3f7.
// schema.sql is that commit's extracted 9503 migrations, not a database this binary wrote.
const delivered9503SchemaSHA = "a7448e985f863e8ea98668764568645786cd849c6d021a7b7edce27c0faf3346"
const delivered9503Commit = "9503ece9d544e5f111f2afa531e87a2c3ad0e6c1"

const (
	deliveredOwnerID  = int64(7)
	deliveredFriendID = int64(8)
)

var deliveredNow = time.Unix(1_800_000_000, 0)

func TestSQLiteAuthV4PreservesDeliveredSessions(t *testing.T) {
	ctx := context.Background()
	live := "ether-admin-fixture-session"
	expired := "ether-admin-expired-session"
	revoked := "ether-admin-revoked-session"
	path, before := prepareDeliveredAuth(t, ctx, func(db *sql.DB) {
		insertDeliveredSessions(t, ctx, db, live, expired, revoked)
	})
	if before.sessions != 2 {
		t.Fatalf("fixture sessions %d", before.sessions)
	}

	db := openUpgraded(t, ctx, path)
	after := readAuthSnapshot(t, ctx, db)
	if after.sessions != 2 || after.live != before.live || after.expired != before.expired {
		t.Fatalf("sessions after auth/4 = %+v; before %+v", after, before)
	}
	if after.revoked != 0 {
		t.Fatalf("revoked session reappeared (%d)", after.revoked)
	}
	if after.sessionTable != before.sessionTable || after.sessionIndex != before.sessionIndex {
		t.Fatalf("auth_sessions schema changed\n table %q\n index %q", after.sessionTable, after.sessionIndex)
	}
	if after.token != 1 || after.delegation != 1 {
		t.Fatalf("token %d delegation %d", after.token, after.delegation)
	}
	if after.authVersion != 8 || after.sessionVersion != 1 {
		t.Fatalf("versions auth=%d sessions=%d", after.authVersion, after.sessionVersion)
	}
	var fkParent, onDelete string
	if err := db.QueryRowContext(ctx, `SELECT "table", on_delete FROM pragma_foreign_key_list('auth_sessions')`).Scan(&fkParent, &onDelete); err != nil {
		t.Fatal(err)
	}
	if fkParent != "auth_users" || !strings.EqualFold(onDelete, "CASCADE") {
		t.Fatalf("session fk %s on delete %s", fkParent, onDelete)
	}
	var violations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign_key_check rows %d", violations)
	}
	var saved int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_temp_master WHERE name='auth_sessions_saved'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Fatal("auth_sessions_saved temp table survived the migration")
	}

	svc, err := New(ctx, db, Options{Now: func() time.Time { return deliveredNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := svc.OwnerSession(ctx, live)
	if err != nil || owner.ID != deliveredOwnerID || !owner.IsOwner {
		t.Fatalf("live session: %+v %v", owner, err)
	}
	if _, err := svc.OwnerSession(ctx, expired); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired session err %v", err)
	}
	if _, err := svc.OwnerSession(ctx, revoked); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked session err %v", err)
	}
	if err := svc.RevokeOwnerSession(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OwnerSession(ctx, live); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked live session err %v", err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, HashToken(live)).Scan(&left); err != nil || left != 0 {
		t.Fatalf("revoke left %d rows (%v)", left, err)
	}
}

func TestSQLiteAuthV4RefusesUnhandledChild(t *testing.T) {
	ctx := context.Background()
	path, before := prepareDeliveredAuth(t, ctx, func(db *sql.DB) {
		insertDeliveredSessions(t, ctx, db, "ether-admin-fixture-session", "ether-admin-expired-session", "ether-admin-revoked-session")
		if _, err := db.ExecContext(ctx, `CREATE TABLE partial_identity_grant_probe (
 hash TEXT PRIMARY KEY,
 viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE
);
INSERT INTO partial_identity_grant_probe(hash, viewer_id) VALUES('grant', ?)`, deliveredOwnerID); err != nil {
			t.Fatal(err)
		}
	})
	_, err := store.OpenPath(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "partial_identity_grant_probe") || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("open err %v", err)
	}
	db := openRawSQLite(t, path)
	defer db.Close()
	after := readAuthSnapshot(t, ctx, db)
	if after.sessions != before.sessions || after.authVersion != 3 {
		t.Fatalf("refused upgrade changed sessions=%d auth=%d", after.sessions, after.authVersion)
	}
	var grants int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM partial_identity_grant_probe`).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("probe rows %d (%v)", grants, err)
	}
}

func TestSQLiteAuthV4RejectsUnexpectedSessionColumns(t *testing.T) {
	ctx := context.Background()
	path, before := prepareDeliveredAuth(t, ctx, func(db *sql.DB) {
		insertDeliveredSessions(t, ctx, db, "ether-admin-fixture-session", "ether-admin-expired-session", "ether-admin-revoked-session")
		if _, err := db.ExecContext(ctx, `ALTER TABLE auth_sessions ADD COLUMN note TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
	})
	_, err := store.OpenPath(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "auth_sessions columns") {
		t.Fatalf("open err %v", err)
	}
	db := openRawSQLite(t, path)
	defer db.Close()
	after := readAuthSnapshot(t, ctx, db)
	if after.sessions != before.sessions || after.authVersion != 3 {
		t.Fatalf("rejected shape changed sessions=%d auth=%d", after.sessions, after.authVersion)
	}
	var note string
	if err := db.QueryRowContext(ctx, `SELECT name FROM pragma_table_info('auth_sessions') WHERE name='note'`).Scan(&note); err != nil || note != "note" {
		t.Fatalf("note column %q (%v)", note, err)
	}
}

func TestSQLiteAuthV4RebuildFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	path, before := prepareDeliveredAuth(t, ctx, func(db *sql.DB) {
		insertDeliveredSessions(t, ctx, db, "ether-admin-fixture-session", "ether-admin-expired-session", "ether-admin-revoked-session")
		if _, err := db.ExecContext(ctx, `CREATE TABLE auth_users_new (id INTEGER)`); err != nil {
			t.Fatal(err)
		}
	})
	_, err := store.OpenPath(ctx, path)
	if err == nil {
		t.Fatal("auth/4 succeeded despite auth_users_new already existing")
	}
	db := openRawSQLite(t, path)
	defer db.Close()
	after := readAuthSnapshot(t, ctx, db)
	if after.sessions != before.sessions || after.live != before.live || after.expired != before.expired {
		t.Fatalf("rolled back sessions %+v want %+v", after, before)
	}
	if after.authVersion != 3 || after.token != 1 || after.delegation != 1 {
		t.Fatalf("rollback auth=%d token=%d delegation=%d", after.authVersion, after.token, after.delegation)
	}
	var users int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users`).Scan(&users); err != nil || users != 2 {
		t.Fatalf("users %d (%v)", users, err)
	}
}

func TestSQLiteAuthV4FreshDatabaseCreatesSessions(t *testing.T) {
	ctx := context.Background()
	db := openDBAt(t, filepath.Join(t.TempDir(), "ether.db"))
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='auth_sessions'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("auth_sessions tables %d (%v)", tables, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fresh sessions %d (%v)", n, err)
	}
	var parent, onDelete, indexName string
	if err := db.QueryRowContext(ctx, `SELECT "table", on_delete FROM pragma_foreign_key_list('auth_sessions')`).Scan(&parent, &onDelete); err != nil {
		t.Fatal(err)
	}
	if parent != "auth_users" || !strings.EqualFold(onDelete, "CASCADE") {
		t.Fatalf("fresh fk %s %s", parent, onDelete)
	}
	if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name='auth_sessions_user'`).Scan(&indexName); err != nil || indexName != "auth_sessions_user" {
		t.Fatalf("index %q (%v)", indexName, err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO auth_users(plex_id, created_at, updated_at) VALUES(99, 1, 1)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash, user_id, created_at, expires_at) VALUES('h', ?, 1, 2)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM auth_users WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade left %d (%v)", n, err)
	}
}

type deliveredSnapshot struct {
	sessions       int
	live           int64
	expired        int64
	revoked        int
	token          int
	delegation     int
	authVersion    int
	sessionVersion int
	sessionTable   string
	sessionIndex   string
}

func prepareDeliveredAuth(t *testing.T, ctx context.Context, alter func(*sql.DB)) (string, deliveredSnapshot) {
	t.Helper()
	blocks := loadDelivered9503(t)
	var parents []string
	for _, b := range blocks {
		if strings.Contains(b.sql, "REFERENCES auth_users") {
			parents = append(parents, b.component+"/"+strconv.Itoa(b.version))
		}
	}
	if strings.Join(parents, ",") != "auth/1,auth/3,auth_sessions/1" {
		t.Fatalf("9503 tables referencing auth_users: %s", strings.Join(parents, ","))
	}
	path := filepath.Join(t.TempDir(), "ether.db")
	db := openRawSQLite(t, path)
	for _, b := range blocks {
		if b.component == "auth" && b.version <= 3 || b.component == "auth_sessions" && b.version == 1 {
			if _, err := db.ExecContext(ctx, b.sql); err != nil {
				t.Fatalf("apply %s/%d: %v", b.component, b.version, err)
			}
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(
 id, plex_id, uuid, username, title, email, thumb, is_owner, is_home, restricted, active, origin,
 plex_sections, stream_limit, max_content_rating, subtitle_languages, audio_languages, allowed_sections,
 created_at, updated_at, last_seen_at)
 VALUES
 (7, 4242, '', 'owner', '', '', '', 1, 0, 0, 1, 'owner', '[]', 0, '', '[]', '[]', '[]', ?, ?, 0),
 (8, 4343, '', 'friend', '', '', '', 0, 0, 0, 1, 'friend', '[]', 0, '', '[]', '[]', '[]', ?, ?, 0)`,
		deliveredNow.Unix(), deliveredNow.Unix(), deliveredNow.Unix(), deliveredNow.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash, user_id, source, created_at) VALUES(?, ?, 'server', ?)`,
		HashToken("fixture-server-token"), deliveredOwnerID, deliveredNow.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_delegations(token_hash, user_id, expires_at) VALUES(?, ?, ?)`,
		HashToken("ether-cast-fixture"), deliveredOwnerID, deliveredNow.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (
 component TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY (component, version))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(component, version) VALUES
 ('auth', 1), ('auth', 2), ('auth', 3), ('auth_sessions', 1)`); err != nil {
		t.Fatal(err)
	}
	if alter != nil {
		alter(db)
	}
	snap := readAuthSnapshot(t, ctx, db)
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, snap
}

func insertDeliveredSessions(t *testing.T, ctx context.Context, db *sql.DB, live, expired, revoked string) {
	t.Helper()
	created := deliveredNow.Add(-48 * time.Hour).Unix()
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash, user_id, created_at, expires_at) VALUES
 (?, ?, ?, ?), (?, ?, ?, ?), (?, ?, ?, ?)`,
		HashToken(live), deliveredOwnerID, created, deliveredNow.Add(10*24*time.Hour).Unix(),
		HashToken(expired), deliveredOwnerID, created, deliveredNow.Add(-time.Hour).Unix(),
		HashToken(revoked), deliveredOwnerID, created, deliveredNow.Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx, `DELETE FROM auth_sessions WHERE token_hash=?`, HashToken(revoked))
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		t.Fatalf("revoke setup deleted %d (%v)", n, err)
	}
}

func openUpgraded(t *testing.T, ctx context.Context, path string) *sql.DB {
	t.Helper()
	db, err := store.OpenPath(ctx, path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readAuthSnapshot(t *testing.T, ctx context.Context, db *sql.DB) deliveredSnapshot {
	t.Helper()
	var s deliveredSnapshot
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions`).Scan(&s.sessions); err != nil {
		t.Fatal(err)
	}
	s.live = sessionExpiry(t, ctx, db, "ether-admin-fixture-session")
	s.expired = sessionExpiry(t, ctx, db, "ether-admin-expired-session")
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions WHERE token_hash=?`, HashToken("ether-admin-revoked-session")).Scan(&s.revoked); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_tokens WHERE token_hash=?`, HashToken("fixture-server-token")).Scan(&s.token); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_delegations WHERE token_hash=?`, HashToken("ether-cast-fixture")).Scan(&s.delegation); err != nil {
		t.Fatal(err)
	}
	s.authVersion = migrationVersion(t, ctx, db, "auth")
	s.sessionVersion = migrationVersion(t, ctx, db, "auth_sessions")
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='auth_sessions'`).Scan(&s.sessionTable); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name='auth_sessions_user'`).Scan(&s.sessionIndex); err != nil {
		t.Fatal(err)
	}
	return s
}

func sessionExpiry(t *testing.T, ctx context.Context, db *sql.DB, token string) int64 {
	t.Helper()
	var expires int64
	err := db.QueryRowContext(ctx, `SELECT expires_at FROM auth_sessions WHERE token_hash=?`, HashToken(token)).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return expires
}

func migrationVersion(t *testing.T, ctx context.Context, db *sql.DB, component string) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations WHERE component=?`, component).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

type fixtureMigration struct {
	component string
	version   int
	sql       string
}

func loadDelivered9503(t *testing.T) []fixtureMigration {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "app", "testdata", "prism-upgrade")
	raw, err := os.ReadFile(filepath.Join(dir, "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != delivered9503SchemaSHA {
		t.Fatalf("schema.sql sha256 %x", sum)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Commit string `json:"commit"`
		SHA    string `json:"sha256"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Commit != delivered9503Commit || manifest.SHA != delivered9503SchemaSHA {
		t.Fatalf("manifest commit %s sha %s", manifest.Commit, manifest.SHA)
	}
	parts := strings.Split(string(raw), "-- prism-upgrade-migration-end")
	var out []fixtureMigration
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, "-- migration ")
		if idx < 0 {
			t.Fatalf("fixture block missing migration header: %.80q", part)
		}
		header, body, found := strings.Cut(part[idx:], "\n")
		if !found {
			t.Fatalf("fixture header %q", header)
		}
		fields := strings.Fields(header)
		if len(fields) != 4 || fields[0] != "--" || fields[1] != "migration" {
			t.Fatalf("fixture header %q", header)
		}
		version, err := strconv.Atoi(fields[3])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fixtureMigration{component: fields[2], version: version, sql: strings.TrimSpace(body)})
	}
	if len(out) != 83 {
		t.Fatalf("fixture migrations %d", len(out))
	}
	return out
}
