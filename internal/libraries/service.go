package libraries

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// List returns every library in owner order: sort, then id (§1.3).
func (s *Service) listDB(ctx context.Context) ([]core.Library, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+libraryCols+` FROM libraries ORDER BY sort, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

// snapshotTTL bounds how long a rename on another cluster node can take to
// reach this node; local writes invalidate the snapshot at once.
const snapshotTTL = 30 * time.Second

type snapshot struct {
	gen  uint64
	at   time.Time
	libs []core.Library
}

// List returns every library in display order. Clients list libraries on
// every section request, so reads come from an in-memory snapshot of the
// (one row per library) table, refreshed on local writes and every
// snapshotTTL.
func (s *Service) List(ctx context.Context) ([]core.Library, error) {
	libs, err := s.cached(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.Library, len(libs))
	for i, lib := range libs {
		out[i] = cloneLibrary(lib)
	}
	return out, nil
}

// Get returns one library by id, or core.ErrNotFound.
func (s *Service) Get(ctx context.Context, id core.LibraryID) (core.Library, error) {
	libs, err := s.cached(ctx)
	if err != nil {
		return core.Library{}, err
	}
	for _, lib := range libs {
		if lib.ID == id {
			return cloneLibrary(lib), nil
		}
	}
	return core.Library{}, core.ErrNotFound
}

func (s *Service) cached(ctx context.Context) ([]core.Library, error) {
	gen := s.gen.Load()
	if snap := s.snap.Load(); snap != nil && snap.gen == gen && s.now().Sub(snap.at) < snapshotTTL {
		return snap.libs, nil
	}
	libs, err := s.listDB(ctx)
	if err != nil {
		return nil, err
	}
	s.snap.Store(&snapshot{gen: gen, at: s.now(), libs: libs})
	return libs, nil
}

func cloneRule(r core.LibraryExclusion) core.LibraryExclusion {
	if len(r.Rules) > 0 {
		conds := make([]core.CollectionRule, len(r.Rules))
		for i, c := range r.Rules {
			if len(c.Value) > 0 {
				v := make(json.RawMessage, len(c.Value))
				copy(v, c.Value)
				c.Value = v
			}
			conds[i] = c
		}
		r.Rules = conds
	}
	return r
}

func cloneLibrary(lib core.Library) core.Library {
	lib.Sources = append([]string(nil), lib.Sources...)
	if len(lib.Rules) > 0 {
		rules := make([]core.LibraryExclusion, len(lib.Rules))
		for i, r := range lib.Rules {
			rules[i] = cloneRule(r)
		}
		lib.Rules = rules
	}
	return lib
}

// ByKey returns one library by its immutable slug, or core.ErrNotFound.
func (s *Service) ByKey(ctx context.Context, key string) (core.Library, error) {
	libs, err := s.cached(ctx)
	if err != nil {
		return core.Library{}, err
	}
	for _, lib := range libs {
		if lib.Key == key {
			return cloneLibrary(lib), nil
		}
	}
	return core.Library{}, core.ErrNotFound
}

// Create saves a library with sources and visible visibility rules.
func (s *Service) Create(ctx context.Context, name string, mediaType core.MediaType, rules []core.LibraryExclusion, fallback string, p core.Presentations) (core.Library, error) {
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if name == "" || slug == "" {
		return core.Library{}, fmt.Errorf("%w: library needs a name with letters or digits", ErrInvalid)
	}
	if mediaType != core.TypeMovie && mediaType != core.TypeShow {
		return core.Library{}, fmt.Errorf("%w: library type must be movie or show", ErrInvalid)
	}
	if _, err := libraryfilter.CompilePolicy(rules, fallback); err != nil {
		return core.Library{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Library{}, err
	}
	defer tx.Rollback()
	id, err := nextID(ctx, tx)
	if err != nil {
		return core.Library{}, err
	}
	key, err := freeKey(ctx, tx, slug)
	if err != nil {
		return core.Library{}, err
	}
	lib, err := s.insert(ctx, tx, core.Library{
		ID: id, Key: key, Name: name, Source: core.LibraryCatalog,
		MediaType: mediaType, Rules: rules, VisibilityDefault: fallback, Present: p, Sort: int(id),
	})
	if err != nil {
		return core.Library{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.Library{}, err
	}
	s.gen.Add(1)
	return lib, nil
}

// CreateCollection saves an owner collection-library (§1.5): a named
// library that shows exactly the titles of one owner collection (an
// MDBList list, a Trakt list, a smart rule, a hand-made list…). The
// collection id is stored as given; whether it still names a collection
// is checked when the library is browsed, so a deleted collection leaves
// the library empty rather than failing the read. mediaType pins
// movie|show like a filter library. The caller passes the zero
// Presentations for the console-only default (§15.3).
func (s *Service) CreateCollection(ctx context.Context, name string, collectionID int64, mediaType core.MediaType, p core.Presentations) (core.Library, error) {
	name = strings.TrimSpace(name)
	slug := slugify(name)
	if name == "" || slug == "" {
		return core.Library{}, fmt.Errorf("%w: collection library needs a name with letters or digits", ErrInvalid)
	}
	if collectionID <= 0 {
		return core.Library{}, fmt.Errorf("%w: collection library needs a collection", ErrInvalid)
	}
	if mediaType != core.TypeMovie && mediaType != core.TypeShow {
		return core.Library{}, fmt.Errorf("%w: collection library type must be movie or show", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Library{}, err
	}
	defer tx.Rollback()
	id, err := nextID(ctx, tx)
	if err != nil {
		return core.Library{}, err
	}
	key, err := freeKey(ctx, tx, slug)
	if err != nil {
		return core.Library{}, err
	}
	lib, err := s.insert(ctx, tx, core.Library{
		ID: id, Key: key, Name: name, Source: core.LibraryCollection,
		MediaType: mediaType, CollectionID: collectionID, Present: p, Sort: int(id),
	})
	if err != nil {
		return core.Library{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.Library{}, err
	}
	s.gen.Add(1)
	return lib, nil
}

// CollectionMember reports whether key is a shared member of the owner
// collection collectionID: one indexed read of collections_members by
// (spec_id, key). A deleted collection has no members and reports false,
// never an error, so a collection library simply shows nothing.
func (s *Service) CollectionMember(ctx context.Context, collectionID int64, key core.RatingKey) (bool, error) {
	if collectionID <= 0 || key == 0 {
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM collections_members WHERE spec_id=? AND key=? AND shared=1 LIMIT 1`,
		collectionID, int64(key)).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpsertPluginLibrary returns the library for one plugin media kind,
// creating it on first install (§2.5). An existing library is
// returned untouched: the stored name and presentation are
// owner-owned after install and a later manifest never overwrites
// them.
func (s *Service) UpsertPluginLibrary(ctx context.Context, pluginID, kindKey string, decl PluginLibraryDecl) (core.Library, error) {
	pluginID, kindKey = strings.TrimSpace(pluginID), strings.TrimSpace(kindKey)
	name := strings.TrimSpace(decl.Name)
	if pluginID == "" || kindKey == "" || name == "" || slugify(pluginID) == "" || slugify(kindKey) == "" {
		return core.Library{}, fmt.Errorf("%w: plugin library needs plugin, kind and name", ErrInvalid)
	}
	existing, err := s.libraryForPluginKind(ctx, pluginID, kindKey)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return core.Library{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.Library{}, err
	}
	defer tx.Rollback()
	id, err := nextID(ctx, tx)
	if err != nil {
		return core.Library{}, err
	}
	key, err := freeKey(ctx, tx, "pk-"+slugify(pluginID)+"-"+slugify(kindKey))
	if err != nil {
		return core.Library{}, err
	}
	lib, err := s.insert(ctx, tx, core.Library{
		ID: id, Key: key, Name: name, Source: core.LibraryPlugin,
		PluginID: pluginID, KindKey: kindKey, Present: decl.Present, Sort: int(id),
	})
	if err != nil {
		// A concurrent install won the (plugin, kind) unique index:
		// return its row rather than failing the install.
		if again, gerr := s.libraryForPluginKind(ctx, pluginID, kindKey); gerr == nil {
			return again, nil
		}
		return core.Library{}, err
	}
	if err := tx.Commit(); err != nil {
		if again, gerr := s.libraryForPluginKind(ctx, pluginID, kindKey); gerr == nil {
			return again, nil
		}
		return core.Library{}, err
	}
	s.gen.Add(1)
	return lib, nil
}

func (s *Service) libraryForPluginKind(ctx context.Context, pluginID, kindKey string) (core.Library, error) {
	lib, err := scanLibrary(s.db.QueryRowContext(ctx,
		`SELECT `+libraryCols+` FROM libraries WHERE source = 'plugin' AND plugin_id = ? AND kind_key = ?`, pluginID, kindKey))
	if errors.Is(err, sql.ErrNoRows) {
		return core.Library{}, core.ErrNotFound
	}
	return lib, err
}

// Rename changes a library's display name. The key never changes.
func (s *Service) Rename(ctx context.Context, id core.LibraryID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: library name must not be empty", ErrInvalid)
	}
	return s.setColumn(ctx, id, "name", name)
}

// SetSort sets a library's owner order in every client.
func (s *Service) SetSort(ctx context.Context, id core.LibraryID, sort int) error {
	return s.setColumn(ctx, id, "sort", sort)
}

// SetHidden hides or shows a library in all clients; it stays in the
// console either way.
func (s *Service) SetHidden(ctx context.Context, id core.LibraryID, hidden bool) error {
	v := 0
	if hidden {
		v = 1
	}
	return s.setColumn(ctx, id, "hidden", v)
}

// SetPresent sets how protocols expose a library (§6.2).
func (s *Service) SetPresent(ctx context.Context, id core.LibraryID, p core.Presentations) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.setColumn(ctx, id, "present", string(b))
}

// SetQualityProfile sets the quality profile name titles in this library
// rank with. An empty name means "Default" (today's Movies/Shows
// home/away defaults). The name is stored verbatim; whether it names a
// configured profile is checked at resolution time, so a profile rename
// or a guide sync never leaves a library pointing at a missing profile.
func (s *Service) SetQualityProfile(ctx context.Context, id core.LibraryID, name string) error {
	return s.setColumn(ctx, id, "quality_profile", strings.TrimSpace(name))
}

// SetRules validates and stores TMDB visibility rules and default visibility
// on a built-in or filter library. Rules on a single-type library are pinned
// to that library's media type.
func (s *Service) SetRules(ctx context.Context, id core.LibraryID, rules []core.LibraryExclusion, visibilityDefault string) error {
	lib, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	pinnedType := ""
	if lib.Source == core.LibraryPlugin {
		pinnedType = "mixed"
	}
	switch {
	case lib.ID == core.LibraryMovies || lib.MediaType == core.TypeMovie:
		pinnedType = string(core.TypeMovie)
	case lib.ID == core.LibraryShows || lib.MediaType == core.TypeShow:
		pinnedType = string(core.TypeShow)
	}
	normalized := make([]core.LibraryExclusion, len(rules))
	for i, r := range rules {
		cp := cloneRule(r)
		if pinnedType != "" {
			cp.MediaType = pinnedType
		} else if cp.MediaType == "" {
			cp.MediaType = "mixed"
		}
		normalized[i] = cp
	}
	compile := libraryfilter.CompilePolicy
	if lib.Source == core.LibraryPlugin {
		compile = libraryfilter.CompilePluginPolicy
	}
	if _, err := compile(normalized, visibilityDefault); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	vis := ""
	if visibilityDefault == "hide" {
		vis = "hide"
	}
	rulesJSON := "[]"
	if len(normalized) > 0 {
		b, err := json.Marshal(normalized)
		if err != nil {
			return err
		}
		rulesJSON = string(b)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE libraries SET rules = ?, visibility_default = ?, updated_at = ? WHERE id = ?`,
		rulesJSON, vis, s.now().Unix(), int64(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	s.gen.Add(1)
	return nil
}

// Delete removes a filter or plugin library. Built-ins are refused:
// Movies and TV Shows exist forever. Plugin libraries are deleted
// only through plugin removal (§2.5), which calls this after purging
// the plugin's items.
func (s *Service) Delete(ctx context.Context, id core.LibraryID) error {
	lib, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if lib.Source == core.LibraryBuiltin {
		return ErrBuiltinLibrary
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, int64(id)); err != nil {
		return err
	}
	s.gen.Add(1)
	return nil
}
