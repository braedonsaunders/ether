// Package notinterested stores viewer-specific title exclusions. It does not
// affect ranking or browsing until a caller explicitly applies the preference.
package notinterested

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("notinterested", 1, `CREATE TABLE notinterested_titles (viewer_id INTEGER NOT NULL, title_key TEXT NOT NULL, added_at INTEGER NOT NULL, handle TEXT NOT NULL UNIQUE, PRIMARY KEY(viewer_id,title_key)); CREATE INDEX notinterested_titles_order ON notinterested_titles(viewer_id,added_at,title_key);`)
}

var ErrForbidden = errors.New("notinterested: viewer or title unavailable")

// Options supplies the shared database and current authorization checks.
// Changed is called once after each successful change and never for a no-op.
type Options struct {
	DB      *sql.DB
	Active  func(context.Context, int64) (*core.User, error)
	Allowed func(context.Context, *core.User, core.RatingKey) bool
	Changed func(context.Context, int64, core.RatingKey)
	Now     func() time.Time
}

type Service struct {
	o   Options
	gen atomic.Uint64
}

// Generation changes when a title is hidden or restored.
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

type Entry struct {
	Key     core.RatingKey
	AddedAt time.Time
}

func New(o Options) (*Service, error) {
	if o.DB == nil || o.Active == nil || o.Allowed == nil || o.Changed == nil {
		return nil, errors.New("notinterested: incomplete dependencies")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}, nil
}

func valid(key core.RatingKey) bool {
	if key <= 0 || !key.Valid() {
		return false
	}
	switch key.Kind() {
	case core.KindMovie, core.KindShow, core.KindSeason, core.KindEpisode, core.KindLocal:
		return true
	}
	return false
}
func (s *Service) viewer(ctx context.Context, v *core.User) (*core.User, error) {
	if v == nil || v.ID <= 0 {
		return nil, ErrForbidden
	}
	active, err := s.o.Active(ctx, v.ID)
	if err != nil || active == nil || active.ID != v.ID {
		return nil, ErrForbidden
	}
	return active, nil
}
func (s *Service) authorize(ctx context.Context, v *core.User, key core.RatingKey) error {
	if !valid(key) {
		return ErrForbidden
	}
	active, err := s.viewer(ctx, v)
	if err != nil || !s.o.Allowed(ctx, active, key) {
		return ErrForbidden
	}
	return nil
}

// Set records or undoes an exclusion. Both operations require current access,
// so knowledge of an old key cannot be used to probe an inaccessible title.
func (s *Service) Set(ctx context.Context, v *core.User, key core.RatingKey, excluded bool) (bool, error) {
	if err := s.authorize(ctx, v, key); err != nil {
		return false, err
	}
	var result sql.Result
	var err error
	if excluded {
		var token [32]byte
		if _, err = rand.Read(token[:]); err != nil {
			return false, err
		}
		result, err = s.o.DB.ExecContext(ctx, `INSERT INTO notinterested_titles(viewer_id,title_key,added_at,handle) VALUES(?,?,?,?) ON CONFLICT(viewer_id,title_key) DO NOTHING`, v.ID, key.String(), s.o.Now().UnixNano(), hex.EncodeToString(token[:]))
	} else {
		result, err = s.o.DB.ExecContext(ctx, `DELETE FROM notinterested_titles WHERE viewer_id=? AND title_key=?`, v.ID, key.String())
	}
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	s.gen.Add(1)
	s.o.Changed(ctx, v.ID, key)
	return true, nil
}

func (s *Service) Has(ctx context.Context, v *core.User, key core.RatingKey) (bool, error) {
	if err := s.authorize(ctx, v, key); err != nil {
		return false, err
	}
	var n int
	err := s.o.DB.QueryRowContext(ctx, `SELECT 1 FROM notinterested_titles WHERE viewer_id=? AND title_key=?`, v.ID, key.String()).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// HasMany reads membership for keys the caller already authorized. It checks
// the viewer once and issues one query per chunk.
func (s *Service) HasMany(ctx context.Context, v *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := make(map[core.RatingKey]bool, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	active, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		if !valid(key) {
			return nil, ErrForbidden
		}
	}
	for start := 0; start < len(keys); start += 400 {
		chunk := keys[start:min(start+400, len(keys))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, active.ID)
		query := `SELECT title_key FROM notinterested_titles WHERE viewer_id=? AND title_key IN (`
		for i, key := range chunk {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, key.String())
		}
		query += ")"
		rows, err := s.o.DB.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, err
			}
			key, err := core.ParseRatingKey(raw)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[key] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// List filters before applying limit; hidden titles never appear in the undo UI.
// A nonpositive limit returns all permitted entries.
func (s *Service) List(ctx context.Context, v *core.User, limit int) ([]Entry, error) {
	active, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT title_key,added_at FROM notinterested_titles WHERE viewer_id=? ORDER BY added_at DESC,title_key ASC`, active.ID)
	if err != nil {
		return nil, err
	}
	type row struct {
		key string
		at  int64
	}
	var candidates []row
	for rows.Next() {
		var candidate row
		if err := rows.Scan(&candidate.key, &candidate.at); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, candidate := range candidates {
		key, err := core.ParseRatingKey(candidate.key)
		if err != nil || !valid(key) || !s.o.Allowed(ctx, active, key) {
			continue
		}
		entries = append(entries, Entry{Key: key, AddedAt: time.Unix(0, candidate.at).UTC()})
		if limit > 0 && len(entries) >= limit {
			break
		}
	}
	return entries, nil
}

// UndoEntry exposes no title key or metadata when the title is no longer
// visible. Handles are random, viewer-bound, and useful only for removal.
type UndoEntry struct {
	Handle string
	Key    core.RatingKey // zero means no current permission to disclose the title
}

// ListUndo returns opaque handles for all saved exclusions; ordering by handle
// does not disclose when an inaccessible item was added.
func (s *Service) ListUndo(ctx context.Context, v *core.User) ([]UndoEntry, error) {
	active, err := s.viewer(ctx, v)
	if err != nil {
		return nil, err
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT title_key,handle FROM notinterested_titles WHERE viewer_id=? ORDER BY handle`, active.ID)
	if err != nil {
		return nil, err
	}
	type row struct{ key, handle string }
	var candidates []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.key, &r.handle); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]UndoEntry, 0, len(candidates))
	for _, candidate := range candidates {
		entry := UndoEntry{Handle: candidate.handle}
		key, e := core.ParseRatingKey(candidate.key)
		if e == nil && valid(key) && s.o.Allowed(ctx, active, key) {
			entry.Key = key
		}
		result = append(result, entry)
	}
	return result, nil
}

// Undo removes only the current viewer's entry for a handle. For a now-hidden
// item Changed receives key zero so a notification cannot disclose its identity.
func (s *Service) Undo(ctx context.Context, v *core.User, handle string) (bool, error) {
	active, err := s.viewer(ctx, v)
	if err != nil {
		return false, err
	}
	if len(handle) != 64 {
		return false, ErrForbidden
	}
	if _, err := hex.DecodeString(handle); err != nil {
		return false, ErrForbidden
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT title_key FROM notinterested_titles WHERE viewer_id=? AND handle=?`, active.ID, handle).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM notinterested_titles WHERE viewer_id=? AND handle=?`, active.ID, handle)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	key, e := core.ParseRatingKey(raw)
	if e != nil || !valid(key) || !s.o.Allowed(ctx, active, key) {
		key = 0
	}
	s.gen.Add(1)
	s.o.Changed(ctx, active.ID, key)
	return true, nil
}
