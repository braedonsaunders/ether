// Package libraries owns the one Library model (spec §1): the two
// built-in libraries (1 = Movies, 2 = TV Shows), plugin-kind libraries
// and owner filter-libraries, in one component-owned table. Frontends,
// the console and app wiring consume it through Service; nothing in a
// request path special-cases a library beyond a dispatch on its Source.
package libraries

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("libraries", 1, `
CREATE TABLE libraries (
	id         INTEGER PRIMARY KEY,
	key        TEXT    NOT NULL UNIQUE,
	name       TEXT    NOT NULL,
	source     TEXT    NOT NULL,
	media_type TEXT    NOT NULL DEFAULT '',
	plugin_id  TEXT    NOT NULL DEFAULT '',
	kind_key   TEXT    NOT NULL DEFAULT '',
	filter     TEXT    NOT NULL DEFAULT '',
	present    TEXT    NOT NULL DEFAULT '{}',
	sort       INTEGER NOT NULL DEFAULT 0,
	hidden     INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX libraries_plugin_kind ON libraries(plugin_id, kind_key) WHERE source = 'plugin';
CREATE TABLE libraries_seq (id INTEGER PRIMARY KEY, next INTEGER NOT NULL);
CREATE TABLE libraries_state (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
`)
	store.RegisterDialect("libraries", 1, store.Postgres, `
CREATE TABLE libraries (
	id         BIGINT PRIMARY KEY,
	key        TEXT    NOT NULL UNIQUE,
	name       TEXT    NOT NULL,
	source     TEXT    NOT NULL,
	media_type TEXT    NOT NULL DEFAULT '',
	plugin_id  TEXT    NOT NULL DEFAULT '',
	kind_key   TEXT    NOT NULL DEFAULT '',
	filter     TEXT    NOT NULL DEFAULT '',
	present    TEXT    NOT NULL DEFAULT '{}',
	sort       BIGINT  NOT NULL DEFAULT 0,
	hidden     INTEGER NOT NULL DEFAULT 0,
	created_at BIGINT  NOT NULL,
	updated_at BIGINT  NOT NULL
);
CREATE UNIQUE INDEX libraries_plugin_kind ON libraries(plugin_id, kind_key) WHERE source = 'plugin';
CREATE TABLE libraries_seq (id BIGINT PRIMARY KEY, next BIGINT NOT NULL);
CREATE TABLE libraries_state (k TEXT PRIMARY KEY, v TEXT NOT NULL DEFAULT '');
`)
	store.Register("libraries", 2, `
ALTER TABLE libraries ADD COLUMN rules TEXT NOT NULL DEFAULT '[]';
ALTER TABLE libraries ADD COLUMN visibility_default TEXT NOT NULL DEFAULT '';
`)
	store.RegisterDialect("libraries", 2, store.Postgres, `
ALTER TABLE libraries ADD COLUMN rules TEXT NOT NULL DEFAULT '[]';
ALTER TABLE libraries ADD COLUMN visibility_default TEXT NOT NULL DEFAULT '';
`)
	store.Register("libraries", 3, `
ALTER TABLE libraries ADD COLUMN quality_profile TEXT NOT NULL DEFAULT '';
`)
	store.RegisterDialect("libraries", 3, store.Postgres, `
ALTER TABLE libraries ADD COLUMN quality_profile TEXT NOT NULL DEFAULT '';
`)
}

var (
	// ErrBuiltinLibrary refuses a mutation the two built-ins never take
	// (deletion). Renaming, sorting and hiding them is fine.
	ErrBuiltinLibrary = errors.New("libraries: built-in library cannot be deleted")
	// ErrInvalid is a rejected declaration: empty name, an unslugifiable
	// name, or a plugin library without both ids.
	ErrInvalid = errors.New("libraries: invalid library")
)

// PluginLibraryDecl is what a plugin kind asks for at install (§2.5):
// the default display name and presentation. Owner edits made after
// install win: an upsert of an existing (plugin, kind) never
// overwrites the stored name or presentation.
type PluginLibraryDecl struct {
	Name    string
	Present core.Presentations
}

// Options configures the Service.
type Options struct {
	Logger *slog.Logger
	Now    func() time.Time
}

// Service is the libraries store. List/Get are plain indexed reads of
// a table that holds one row per library, never per item, so no call
// scales with library size.
type Service struct {
	db   *sql.DB
	log  *slog.Logger
	now  func() time.Time
	gen  atomic.Uint64
	snap atomic.Pointer[snapshot]
}

// New seeds the two built-in libraries idempotently (§1.3) and returns
// the service. db must already be migrated (store.Migrate), which is
// how every component table is opened.
func New(ctx context.Context, db *sql.DB, o Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("libraries: db required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Service{db: db, log: o.Logger.With("component", "libraries"), now: o.Now}
	if err := s.seedBuiltins(ctx); err != nil {
		return nil, fmt.Errorf("libraries: seed builtins: %w", err)
	}
	return s, nil
}

// Generation changes on every library mutation; it feeds
// frontend.PageFreshness (§6.4).
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

func (s *Service) seedBuiltins(ctx context.Context) error {
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, b := range []struct {
		id        core.LibraryID
		key, name string
		mediaType core.MediaType
		sort      int
	}{
		{core.LibraryMovies, "movies", "Movies", core.TypeMovie, 1},
		{core.LibraryShows, "shows", "TV Shows", core.TypeShow, 2},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO libraries(id, key, name, source, media_type, sort, created_at, updated_at)
VALUES(?, ?, ?, 'builtin', ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
			int64(b.id), b.key, b.name, string(b.mediaType), b.sort, now, now); err != nil {
			return err
		}
	}
	// The next free id, past whatever already exists; INSERT of a
	// fresh row only, so a later open never rewinds the counter. The
	// WHERE keeps SQLite from reading ON CONFLICT as a join's ON.
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries_seq(id, next)
SELECT 1, COALESCE(MAX(id), 2) + 1 FROM libraries WHERE 1=1 ON CONFLICT(id) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

// nextID allocates a never-reused library id inside tx (§1.3: a
// libraries_seq row in the same component, no AUTOINCREMENT
// divergence between dialects).
func nextID(ctx context.Context, tx *sql.Tx) (core.LibraryID, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE libraries_seq SET next = next + 1 WHERE id = 1`); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT next FROM libraries_seq WHERE id = 1`).Scan(&next); err != nil {
		return 0, err
	}
	return core.LibraryID(next - 1), nil
}
