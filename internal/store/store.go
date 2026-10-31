// Package store opens Ether's database and applies each component's
// migrations. Components own their tables: they register migrations under
// their own name (table names prefixed with it) and receive *sql.DB.
//
//	func init() {
//		store.Register("catalog", 1, `CREATE TABLE catalog_titles (...)`)
//	}
//
// A standalone server uses SQLite in its data directory. A clustered node
// uses an owner-supplied PostgreSQL database; component SQL is written once
// with `?` placeholders (rebound to $n by the "ether-pgx" driver) and
// migrations are translated from SQLite DDL (see TranslateDDL). DialectOf
// tells a component which engine it is talking to for the few constructs
// that differ.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type migration struct {
	component   string
	version     int
	sql         string
	overrides   map[Dialect]string
	sqliteApply func(context.Context, *sql.Tx) error
}

var (
	regMu        sync.Mutex
	migrations   []migration
	dependencies = map[string]map[string]bool{}
)

// RegisterAfter declares component-level migration dependencies. Every
// migration of a dependency is applied before any migration of component.
// Unrelated components retain their alphabetical order. Dependencies absent
// from a partial binary are ignored; cycles fail migration startup.
func RegisterAfter(component string, dependsOn ...string) {
	regMu.Lock()
	defer regMu.Unlock()
	if dependencies[component] == nil {
		dependencies[component] = map[string]bool{}
	}
	for _, dependency := range dependsOn {
		dependencies[component][dependency] = true
	}
}

// Register adds a migration written in SQLite's dialect. Versions are per
// component and start at 1. Call from init(); a migration must never change
// after release. On PostgreSQL the DDL is translated by TranslateDDL unless
// RegisterDialect supplied an override.
func Register(component string, version int, sql string) {
	regMu.Lock()
	defer regMu.Unlock()
	for i := range migrations {
		// Fill a placeholder left by an earlier RegisterDialect.
		if migrations[i].component == component && migrations[i].version == version && migrations[i].sql == "" {
			migrations[i].sql = sql
			return
		}
	}
	migrations = append(migrations, migration{component: component, version: version, sql: sql})
}

// RegisterDialect overrides one registered migration for dialect d, for DDL
// the mechanical translation cannot express. It may be called before or
// after the matching Register (both from init()).
func RegisterDialect(component string, version int, d Dialect, sql string) {
	regMu.Lock()
	defer regMu.Unlock()
	for i := range migrations {
		if migrations[i].component == component && migrations[i].version == version {
			if migrations[i].overrides == nil {
				migrations[i].overrides = map[Dialect]string{}
			}
			migrations[i].overrides[d] = sql
			return
		}
	}
	migrations = append(migrations, migration{component: component, version: version, overrides: map[Dialect]string{d: sql}})
}

// RegisterSQLiteApply binds apply to a migration Register or RegisterDialect
// already recorded. It does not create a version. SQLite Migrate calls apply
// instead of that version's SQLite SQL. PostgreSQL never calls apply; it
// still runs the registered SQL or the RegisterDialect override. Success and
// failure use the same version bookkeeping as every other migration: the
// schema_migrations row is inserted only after apply returns nil, and an
// error rolls the version's transaction back with no row written. apply does
// not disable foreign keys.
func RegisterSQLiteApply(component string, version int, apply func(context.Context, *sql.Tx) error) {
	if apply == nil {
		panic("store: RegisterSQLiteApply requires a function")
	}
	regMu.Lock()
	defer regMu.Unlock()
	for i := range migrations {
		if migrations[i].component == component && migrations[i].version == version {
			if migrations[i].sqliteApply != nil {
				panic("store: RegisterSQLiteApply " + component + "/" + strconv.Itoa(version) + " already bound")
			}
			migrations[i].sqliteApply = apply
			return
		}
	}
	panic("store: RegisterSQLiteApply " + component + "/" + strconv.Itoa(version) + " is not registered")
}

// sqlFor returns the migration text to run on dialect d.
func (m migration) sqlFor(d Dialect) string {
	if s, ok := m.overrides[d]; ok {
		return s
	}
	if d == Postgres {
		return TranslateDDL(m.sql)
	}
	return m.sql
}

// Options selects and configures the database backend.
type Options struct {
	// DataDir holds ether.db for SQLite.
	DataDir string
	// Driver is "sqlite" (default) or "postgres".
	Driver string
	// URL is the PostgreSQL connection URL (postgres://...). A postgres://
	// or postgresql:// URL implies Driver "postgres".
	URL string
	// Logger receives open-time warnings; nil uses slog.Default().
	Logger *slog.Logger
	// MigrationGate, when set, decides on PostgreSQL whether this node may
	// apply its pending migrations now (see MigrationGate).
	MigrationGate MigrationGate
	// SnapshotFiles are stored with a SQLite pre-migrate snapshot (the
	// settings and credential key matching the database), if one is taken.
	SnapshotFiles []SnapshotFile
}

// Querier is the read side of *sql.DB and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// MigrationGate is consulted while the cluster-wide migration lock is held
// and before any of pending ("component/version", in apply order) is
// applied. Returning an error wrapping ErrMigrationDeferred makes
// OpenWithRetry wait and try again; any other error fails the open. q
// reads inside the migration transaction.
type MigrationGate func(ctx context.Context, q Querier, pending []string) error

// ErrMigrationDeferred means this node must not migrate the shared schema
// yet (another live node runs a binary that does not know the pending
// migrations and the owner asked to wait for it).
var ErrMigrationDeferred = errors.New("store: schema migration deferred")

// HasDependency reports whether component declared a RegisterAfter dependency
// that is present. Absent dependencies are ignored, matching Migrate.
func HasDependency(component, dependency string) bool {
	regMu.Lock()
	defer regMu.Unlock()
	return dependencies[component][dependency]
}

// ComponentOrder is the migration component sequence Migrate will apply.
func ComponentOrder() []string {
	var order []string
	seen := map[string]bool{}
	for _, m := range registered() {
		if seen[m.component] {
			continue
		}
		seen[m.component] = true
		order = append(order, m.component)
	}
	return order
}

// KnownMigrations is the highest migration version this binary registers
// for each component.
func KnownMigrations() map[string]int {
	out := map[string]int{}
	for _, m := range registered() {
		if m.version > out[m.component] {
			out[m.component] = m.version
		}
	}
	return out
}

// UnknownApplied lists migrations ("component/version") recorded in db that
// this binary does not register: the database was migrated by a newer
// version of Ether.
func UnknownApplied(ctx context.Context, db *sql.DB) ([]string, error) {
	known := map[string]bool{}
	for _, m := range registered() {
		known[m.component+"/"+strconv.Itoa(m.version)] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT component, version FROM schema_migrations ORDER BY component, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		if k := c + "/" + strconv.Itoa(v); !known[k] {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

// Dialect is the backend these options select.
func (o Options) Dialect() Dialect {
	if isPostgresURL(o.URL) {
		return Postgres
	}
	return ParseDialect(o.Driver)
}

// OpenWith opens and migrates the database the options select and reports
// its dialect.
func OpenWith(ctx context.Context, o Options) (*sql.DB, Dialect, error) {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	switch d := o.Dialect(); d {
	case Postgres:
		if o.URL == "" {
			return nil, d, errors.New("store: postgres requires a database URL")
		}
		db, err := openPostgres(ctx, o.URL, log, o.MigrationGate)
		return db, d, err
	default:
		if o.DataDir == "" {
			return nil, d, errors.New("store: sqlite requires a data directory")
		}
		db, err := openSQLite(ctx, filepath.Join(o.DataDir, "ether.db"), o.SnapshotFiles)
		return db, d, err
	}
}

// Open opens (creating if needed) ether.db in dataDir and migrates it.
func Open(ctx context.Context, dataDir string) (*sql.DB, error) {
	return openSQLite(ctx, filepath.Join(dataDir, "ether.db"), nil)
}

// OpenPath opens a database file; ":memory:" works for tests (single conn).
// Like OpenMemory, ":memory:" is a fresh PostgreSQL schema instead when
// TestPostgresEnv is set.
func OpenPath(ctx context.Context, path string) (*sql.DB, error) {
	if path == ":memory:" {
		if u := os.Getenv(TestPostgresEnv); u != "" {
			return OpenTestPostgres(ctx, u)
		}
	}
	return openSQLite(ctx, path, nil)
}

func openSQLite(ctx context.Context, path string, files []SnapshotFile) (*sql.DB, error) {
	// journal_size_limit caps the WAL left on disk after a checkpoint; without
	// it one large transaction leaves a WAL file that never shrinks.
	dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-65536)&_pragma=temp_store(MEMORY)&_pragma=mmap_size(268435456)" +
		fmt.Sprintf("&_pragma=journal_size_limit(%d)", journalSizeLimit)
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	} else if err := protectDBFiles(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite-cached", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else if ok, err := snapshotBeforeMigrate(ctx, db, path, registered(), files); err != nil {
		// Best effort: a full disk or read-only directory must not stop
		// the server from starting.
		slog.Warn("store: could not snapshot the database before migrating", "path", path+PreMigrateSuffix, "err", err)
	} else if ok {
		slog.Info("store: snapshot taken before applying database migrations", "path", path+PreMigrateSuffix)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		finishPreMigrate(path)
		warnSchemaAhead(ctx, db, slog.Default(), path+PreMigrateSuffix)
	}
	// Recommended at open by the SQLite docs: analyse tables whose stats are
	// missing or stale, bounded by the 0x10000 analysis limit so it is cheap.
	_, _ = db.ExecContext(ctx, `PRAGMA optimize=0x10002`)
	if path != ":memory:" {
		// Opening SQLite re-reads the whole schema. The default idle pool
		// drops every connection past the second, so a play paid that cost
		// again. Keep the connections a server actually uses.
		db.SetMaxIdleConns(8)
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// protectDBFiles keeps a database file and its SQLite sidecars owner-only,
// since ether.db holds tokens and credentials. SQLite creates -wal, -shm
// and -journal with the database file's mode, so creating the file 0600
// first covers new sidecars; existing files lose group and other access.
// Files owned by another user are left alone. On Windows the data
// directory's ACL protects them instead.
func protectDBFiles(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if m := fi.Mode().Perm(); m&0o077 != 0 {
			if err := os.Chmod(p, m&^0o077); err != nil && !errors.Is(err, fs.ErrPermission) {
				return err
			}
		}
	}
	return nil
}

// registered returns components in stable dependency order, versions ascending.
func registered() []migration {
	regMu.Lock()
	ms := append([]migration(nil), migrations...)
	deps := make(map[string][]string, len(dependencies))
	for component, requirements := range dependencies {
		for requirement := range requirements {
			deps[component] = append(deps[component], requirement)
		}
		sort.Strings(deps[component])
	}
	regMu.Unlock()
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].component != ms[j].component {
			return ms[i].component < ms[j].component
		}
		return ms[i].version < ms[j].version
	})
	groups := make(map[string][]migration)
	var components []string
	for _, m := range ms {
		if _, ok := groups[m.component]; !ok {
			components = append(components, m.component)
		}
		groups[m.component] = append(groups[m.component], m)
	}
	done := make(map[string]bool, len(groups))
	ordered := make([]migration, 0, len(ms))
	for len(done) < len(components) {
		chosen := ""
		for _, component := range components {
			if done[component] {
				continue
			}
			ready := true
			for _, requirement := range deps[component] {
				if _, present := groups[requirement]; present && !done[requirement] {
					ready = false
					break
				}
			}
			if ready {
				chosen = component
				break
			}
		}
		if chosen == "" {
			panic("store: cyclic migration dependency")
		}
		done[chosen] = true
		ordered = append(ordered, groups[chosen]...)
	}
	return ordered
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
		component TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY (component, version))`

// Migrate applies every registered migration db has not yet recorded.
func Migrate(ctx context.Context, db *sql.DB) error {
	if DialectOf(db) == Postgres {
		return migratePostgres(ctx, db, nil)
	}
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return err
	}
	for _, m := range registered() {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE component=? AND version=?`,
			m.component, m.version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// sqliteApply is SQLite-only. A nil callback runs sqlFor(SQLite), the
		// same statement as before this hook existed. PostgreSQL goes through
		// migratePostgres and never reaches this branch.
		var applyErr error
		if m.sqliteApply != nil {
			applyErr = m.sqliteApply(ctx, tx)
		} else {
			_, applyErr = tx.ExecContext(ctx, m.sqlFor(SQLite))
		}
		if applyErr != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s/%d: %w", m.component, m.version, applyErr)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(component, version) VALUES (?, ?)`,
			m.component, m.version); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// AppliedMigrations returns the recorded (component -> highest version).
func AppliedMigrations(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT component, MAX(version) FROM schema_migrations GROUP BY component`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			return nil, err
		}
		out[c] = v
	}
	return out, rows.Err()
}

// RetryOptions configure OpenWithRetry. Zero durations take the defaults
// in parentheses.
type RetryOptions struct {
	MinBackoff time.Duration // first wait (1s), doubling
	MaxBackoff time.Duration // longest wait (30s)
	// OnRetry is called before every wait with the failed attempt's error
	// (attempt counts from 1).
	OnRetry func(attempt int, wait time.Duration, err error)
}

// OpenWithRetry is OpenWith that keeps retrying a PostgreSQL database that
// cannot be reached (refused, timed out, shutting down), with backoff,
// until it opens or ctx ends. It gives up at once on errors retrying cannot
// fix: an invalid URL, failed authentication, a database that does not
// exist, an unsupported server or a failing migration. SQLite is opened
// once, as by OpenWith.
func OpenWithRetry(ctx context.Context, o Options, r RetryOptions) (*sql.DB, Dialect, error) {
	if r.MinBackoff <= 0 {
		r.MinBackoff = time.Second
	}
	if r.MaxBackoff < r.MinBackoff {
		r.MaxBackoff = max(30*time.Second, r.MinBackoff)
	}
	wait := r.MinBackoff
	for attempt := 1; ; attempt++ {
		db, d, err := OpenWith(ctx, o)
		retry := IsUnavailable(err) || errors.Is(err, ErrMigrationDeferred)
		if err == nil || d != Postgres || IsPermanent(err) || !retry {
			return db, d, err
		}
		if ctx.Err() != nil {
			return nil, d, err
		}
		if r.OnRetry != nil {
			r.OnRetry(attempt, wait, err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, d, fmt.Errorf("%w (gave up waiting for the database: %v)", err, ctx.Err())
		case <-t.C:
		}
		wait = min(wait*2, r.MaxBackoff)
	}
}
