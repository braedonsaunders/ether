package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A multi-statement migration exercising every translation rule; it runs on
// whichever backend the store tests use.
func init() {
	Register("storetest", 1, `
CREATE TABLE storetest_queue(id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL DEFAULT '', score REAL NOT NULL DEFAULT 0, blob BLOB, done INTEGER NOT NULL DEFAULT 0);
CREATE INDEX storetest_queue_pending ON storetest_queue(score DESC) WHERE done = 0;
CREATE TABLE storetest_links(id INTEGER PRIMARY KEY, label TEXT NOT NULL);
CREATE TABLE storetest_settings(id INTEGER PRIMARY KEY CHECK(id=1), note TEXT NOT NULL DEFAULT 'semi;colon ''quoted''');
INSERT INTO storetest_settings(id) VALUES(1);
CREATE TABLE storetest_child(link_id INTEGER NOT NULL REFERENCES storetest_links(id) ON DELETE CASCADE, k INTEGER PRIMARY KEY);`)
	Register("storetest", 2, `ALTER TABLE storetest_links ADD COLUMN created INTEGER NOT NULL DEFAULT 0;`)
}

func pgURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv(TestPostgresEnv)
	if u == "" {
		t.Skip("set " + TestPostgresEnv + " to run PostgreSQL tests")
	}
	return u
}

func TestStoreBasicsOnTestBackend(t *testing.T) {
	ctx := context.Background()
	db := OpenTest(t)
	d := DialectOf(db)
	if os.Getenv(TestPostgresEnv) != "" && d != Postgres {
		t.Fatalf("dialect = %s, want postgres", d)
	}

	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO storetest_queue(body, score, blob, done) VALUES(?, ?, ?, ?) RETURNING id`,
		"what's ? here", 1.5, []byte{0, 1, 2, 255}, true).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var body string
	var score float64
	var blob []byte
	var done bool
	if err := db.QueryRowContext(ctx, `SELECT body, score, blob, done FROM storetest_queue WHERE id=? AND body LIKE '%?%'`, id).
		Scan(&body, &score, &blob, &done); err != nil {
		t.Fatal(err)
	}
	if body != "what's ? here" || score != 1.5 || !bytes.Equal(blob, []byte{0, 1, 2, 255}) || !done {
		t.Fatalf("round trip: %q %v %v %v", body, score, blob, done)
	}

	// Generated id after an explicit one; seed row and quoted default.
	if _, err := db.ExecContext(ctx, `INSERT INTO storetest_links(id, label) VALUES(?, ?)`, 1, "a"); err != nil {
		t.Fatal(err)
	}
	var note string
	if err := db.QueryRowContext(ctx, `SELECT note FROM storetest_settings WHERE id=1`).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "semi;colon 'quoted'" {
		t.Fatalf("seed note = %q", note)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO storetest_settings(id) VALUES(2)`); err == nil {
		t.Fatal("singleton CHECK not enforced")
	}
	// ON DELETE CASCADE survives translation.
	if _, err := db.ExecContext(ctx, `INSERT INTO storetest_child(link_id, k) VALUES(1, 7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM storetest_links WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storetest_child`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade: n=%d err=%v", n, err)
	}

	// Prepared statements are rebound too.
	stmt, err := db.PrepareContext(ctx, `INSERT INTO storetest_links(id, label, created) VALUES(?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 10; i < 13; i++ {
		if _, err := stmt.ExecContext(ctx, i, "p", time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()

	// Dialect helpers produce equivalent results on both engines.
	var day, hour, src string
	if err := db.QueryRowContext(ctx, `SELECT `+UnixDay(d, "?")+`, `+UnixHour(d, "?")+`, `+JSONText(d, "?", "sourceId"),
		int64(1700000000), int64(1700000000), `{"sourceId":"s1","x":2}`).Scan(&day, &hour, &src); err != nil {
		t.Fatal(err)
	}
	if day != "2023-11-14" || hour != "22" || src != "s1" {
		t.Fatalf("helpers: day=%q hour=%q src=%q", day, hour, src)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storetest_links WHERE label `+Like(d)+` ?`, "P").Scan(&n); err != nil || n != 3 {
		t.Fatalf("case-insensitive like: n=%d err=%v", n, err)
	}
	if err := Maintain(ctx, db); err != nil {
		t.Fatal(err)
	}
	applied, err := AppliedMigrations(ctx, db)
	if err != nil || applied["storetest"] != 2 {
		t.Fatalf("applied = %v err=%v", applied, err)
	}
}

func TestPostgresLastInsertIDIsAnError(t *testing.T) {
	pgURL(t)
	db := OpenTest(t)
	res, err := db.ExecContext(context.Background(), `INSERT INTO storetest_links(label) VALUES(?)`, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.LastInsertId(); err != ErrNoLastInsertID {
		t.Fatalf("LastInsertId err = %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("RowsAffected = %d, %v", n, err)
	}
}

func TestPostgresConcurrentMigrateOnce(t *testing.T) {
	u := pgURL(t)
	ctx := context.Background()
	db := OpenTest(t) // creates and migrates a schema
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// Forget the migrations so several "nodes" race to apply them again.
	if _, err := db.ExecContext(ctx, `DROP TABLE storetest_queue, storetest_child, storetest_links, storetest_settings; DELETE FROM schema_migrations WHERE component='storetest'`); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	nodeURL := u + sep + "search_path=" + schema
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			node, _, err := OpenWith(ctx, Options{Driver: "postgres", URL: nodeURL})
			if err == nil {
				node.Close()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE component='storetest'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("storetest migrations recorded %d times, err=%v", n, err)
	}
}

func TestPostgresTestSchemaDroppedOnClose(t *testing.T) {
	u := pgURL(t)
	ctx := context.Background()
	db, err := OpenTestPostgres(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(schema, testSchemaPrefix) {
		t.Fatalf("schema = %q", schema)
	}
	db.Close()
	probe := OpenTest(t)
	var n int
	if err := probe.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_namespace WHERE nspname=?`, schema).Scan(&n); err != nil || n != 0 {
		t.Fatalf("schema %s still present (n=%d, err=%v)", schema, n, err)
	}
}

func TestPostgresMigrationFailureRollsBack(t *testing.T) {
	pgURL(t)
	ctx := context.Background()
	db := OpenTest(t)
	regMu.Lock()
	saved := migrations
	migrations = append(append([]migration(nil), saved...),
		migration{component: "storefail", version: 1, sql: `CREATE TABLE storefail_a(v INTEGER)`},
		migration{component: "storefail", version: 2, sql: `CREATE TABLE storefail_b(v NOPE_TYPE)`})
	regMu.Unlock()
	defer func() {
		regMu.Lock()
		migrations = saved
		regMu.Unlock()
	}()
	if err := Migrate(ctx, db); err == nil || !strings.Contains(err.Error(), "storefail/2") {
		t.Fatalf("Migrate err = %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='storefail_a'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration left storefail_a (n=%d err=%v)", n, err)
	}
}

func TestOpenWithSQLite(t *testing.T) {
	dir := t.TempDir()
	db, d, err := OpenWith(context.Background(), Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if d != SQLite || DialectOf(db) != SQLite {
		t.Fatalf("dialect %s / %s", d, DialectOf(db))
	}
	if _, err := os.Stat(filepath.Join(dir, "ether.db")); err != nil {
		t.Fatal(err)
	}
}

func TestOptionsDialect(t *testing.T) {
	for _, c := range []struct {
		o    Options
		want Dialect
	}{
		{Options{}, SQLite},
		{Options{Driver: "sqlite"}, SQLite},
		{Options{Driver: "postgres"}, Postgres},
		{Options{URL: "postgres://u:p@h/db"}, Postgres},
		{Options{URL: "postgresql://h/db"}, Postgres},
	} {
		if got := c.o.Dialect(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.o, got, c.want)
		}
	}
	if _, _, err := OpenWith(context.Background(), Options{Driver: "postgres"}); err == nil {
		t.Fatal("postgres without URL should fail")
	}
}

func TestOpenWithBadURLDoesNotLeakPassword(t *testing.T) {
	_, _, err := OpenWith(context.Background(), Options{URL: "postgres://user:s3cret@host:notaport/db"})
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"": "",
		"postgres://u:pw@h:5432/db?sslmode=disable": "postgres://u:xxxxx@h:5432/db?sslmode=disable",
		"postgres://u@h/db":                         "postgres://u@h/db",
		"postgres://h/db?password=pw":               "postgres://h/db?password=xxxxx",
		"host=h user=u password=pw dbname=db":       "host=h user=u password=xxxxx dbname=db",
		// Unencoded special characters in the password must not leak any
		// of it (url.Parse takes '#' as a fragment).
		"postgres://u:#secret@h/db":                 "postgres://u:xxxxx@h/db",
		"postgres://u:1234#secret@h/db":             "postgres://u:xxxxx@h/db",
		"postgres://u:p@ss#word@h/db":               "postgres://u:xxxxx@h/db",
		"postgres://u:pa/ss@h/db":                   "postgres://u:xxxxx@h/db",
		"postgres://h/db?password=a#bcd":            "postgres://h/db?password=xxxxx",
		"postgres://u:p#w@h/db?x=1&sslpassword=s#t": "postgres://u:xxxxx@h/db?x=1&sslpassword=xxxxx",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileSQLiteKeepsWarmConnections(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	held := make([]*sql.Conn, 4)
	for i := range held {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held[i] = c
	}
	for _, c := range held {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	stats := db.Stats()
	if stats.MaxIdleClosed != 0 || stats.Idle < 4 {
		t.Fatalf("warm pool idle=%d closed=%d", stats.Idle, stats.MaxIdleClosed)
	}
}

func TestDialectOfNonStoreDB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if DialectOf(db) != SQLite || DialectOf(nil) != SQLite {
		t.Fatal("plain sqlite should report SQLite")
	}
	if ParseDialect("PostgreSQL") != Postgres || ParseDialect("") != SQLite {
		t.Fatal("ParseDialect")
	}
}
