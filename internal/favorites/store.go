// Package favorites owns per-viewer favorite identities.
package favorites

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("favorites", 1, `
CREATE TABLE favorites_items (viewer_id INTEGER NOT NULL, kind TEXT NOT NULL, subject_id TEXT NOT NULL, added_at INTEGER NOT NULL, sort_order INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(viewer_id,kind,subject_id));
CREATE INDEX favorites_order ON favorites_items(viewer_id,kind,sort_order,added_at);
CREATE TABLE favorites_mirror (viewer_id INTEGER PRIMARY KEY, disabled INTEGER NOT NULL, updated_at INTEGER NOT NULL DEFAULT 0);
`)
}

type Kind string

const (
	KindTitle      Kind = "title"
	KindPerson     Kind = "person"
	KindCollection Kind = "collection"
	KindChannel    Kind = "channel"
)

type Subject struct {
	Kind Kind
	ID   string
}
type Entry struct {
	Subject Subject
	AddedAt time.Time
}

// Options requires Marked to use the same DB as DB, so playlist creation and
// favorite mutations share one transaction. Callbacks must recheck current access.
type Options struct {
	DB            *sql.DB
	Active        func(context.Context, int64) (*core.User, error)
	Allowed       func(context.Context, *core.User, Subject) bool
	AllowedTitles func(context.Context, *core.User, []core.RatingKey) (bool, error)
	Changed       func(context.Context, int64, Subject)
	Now           func() time.Time
	Marked        core.TransactionalMarkedPlaylists
}
type Service struct {
	o   Options
	gen atomic.Uint64
}

// Generation changes when a favorite is added or removed.
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

var ErrForbidden = errors.New("favorites: viewer or subject unavailable")

func New(o Options) (*Service, error) {
	if o.DB == nil || o.Active == nil || o.Allowed == nil || o.Changed == nil || o.Marked == nil {
		return nil, errors.New("favorites: incomplete dependencies")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}, nil
}
func valid(s Subject) bool {
	if s.Kind != KindTitle && s.Kind != KindPerson && s.Kind != KindCollection && s.Kind != KindChannel {
		return false
	}
	if s.ID == "" || len(s.ID) > 512 || s.ID != strings.TrimSpace(s.ID) || strings.ContainsAny(s.ID, "\x00\r\n") || strings.Contains(s.ID, "://") {
		return false
	}
	if s.Kind == KindTitle || s.Kind == KindCollection {
		key, err := core.ParseRatingKey(s.ID)
		if err != nil {
			return false
		}
		if s.Kind == KindCollection {
			return key.Kind() == core.KindCollection || key.Kind() == core.KindSmart
		}
		return key.Kind() == core.KindMovie || key.Kind() == core.KindShow || key.Kind() == core.KindSeason || key.Kind() == core.KindEpisode
	}
	return true
}
func (s *Service) authorize(ctx context.Context, viewer *core.User, subject Subject) error {
	if viewer == nil || viewer.ID <= 0 || !valid(subject) {
		return ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID || !s.o.Allowed(ctx, active, subject) {
		return ErrForbidden
	}
	return nil
}

// Set changes one favorite; repeated identical writes emit no event.
func (s *Service) Set(ctx context.Context, viewer *core.User, subject Subject, on bool) (bool, error) {
	changed, err := s.SetMany(ctx, viewer, []Subject{subject}, on)
	return len(changed) != 0, err
}

// SetMany validates the entire request before changing any favorite and commits
// all changes together. Repeated subjects and no-op writes emit no event.
func (s *Service) SetMany(ctx context.Context, viewer *core.User, subjects []Subject, on bool) ([]Subject, error) {
	return s.setMany(ctx, viewer, subjects, on, 0)
}

func (s *Service) setMany(ctx context.Context, viewer *core.User, subjects []Subject, on bool, expected core.RatingKey) ([]Subject, error) {
	seen := make(map[Subject]bool, len(subjects))
	unique := make([]Subject, 0, len(subjects))
	for _, subject := range subjects {
		if seen[subject] {
			continue
		}
		if err := s.authorize(ctx, viewer, subject); err != nil {
			return nil, err
		}
		seen[subject] = true
		unique = append(unique, subject)
	}
	if len(unique) == 0 {
		return nil, nil
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.o.DB), tx, fmt.Sprintf("favorites:viewer:%d", viewer.ID)); err != nil {
		return nil, err
	}
	if expected != 0 {
		key, err := s.o.Marked.MarkedPlaylistKeyTx(ctx, tx, viewer.ID, "favorites")
		if err != nil {
			return nil, err
		}
		if key != expected {
			return nil, core.ErrNotFound
		}
	}
	var disabled int
	err = tx.QueryRowContext(ctx, `SELECT disabled FROM favorites_mirror WHERE viewer_id=?`, viewer.ID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if on && disabled == 0 {
		for _, subject := range unique {
			if subject.Kind == KindTitle {
				if _, err := s.o.Marked.EnsureMarkedPlaylistTx(ctx, tx, viewer.ID, "favorites", "Favorites"); err != nil {
					return nil, err
				}
				break
			}
		}
	}
	changed := make([]Subject, 0, len(unique))
	for _, subject := range unique {
		var result sql.Result
		if on {
			result, err = tx.ExecContext(ctx, `INSERT INTO favorites_items(viewer_id,kind,subject_id,added_at,sort_order) VALUES(?,?,?,?,COALESCE((SELECT MIN(sort_order)-1 FROM favorites_items WHERE viewer_id=? AND kind=?),0)) ON CONFLICT(viewer_id,kind,subject_id) DO NOTHING`, viewer.ID, string(subject.Kind), subject.ID, s.o.Now().UnixNano(), viewer.ID, string(subject.Kind))
		} else {
			result, err = tx.ExecContext(ctx, `DELETE FROM favorites_items WHERE viewer_id=? AND kind=? AND subject_id=?`, viewer.ID, string(subject.Kind), subject.ID)
		}
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			changed = append(changed, subject)
		}
	}
	if len(changed) > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO favorites_mirror(viewer_id,disabled,updated_at) VALUES(?,0,?) ON CONFLICT(viewer_id) DO UPDATE SET updated_at=CASE WHEN excluded.updated_at>favorites_mirror.updated_at THEN excluded.updated_at ELSE favorites_mirror.updated_at+1 END`, viewer.ID, s.o.Now().UnixNano()); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		s.gen.Add(1)
	}
	for _, subject := range changed {
		s.o.Changed(ctx, viewer.ID, subject)
	}
	return changed, nil
}
func (s *Service) Has(ctx context.Context, viewer *core.User, subject Subject) (bool, error) {
	if err := s.authorize(ctx, viewer, subject); err != nil {
		return false, err
	}
	var n int
	err := s.o.DB.QueryRowContext(ctx, `SELECT 1 FROM favorites_items WHERE viewer_id=? AND kind=? AND subject_id=?`, viewer.ID, string(subject.Kind), subject.ID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// HasTitles checks title membership in bounded batches, revalidating every subject.
func (s *Service) HasTitles(ctx context.Context, viewer *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
	out := make(map[core.RatingKey]bool, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	if viewer == nil || viewer.ID <= 0 {
		return nil, ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return nil, ErrForbidden
	}
	for _, key := range keys {
		if !valid(Subject{Kind: KindTitle, ID: key.String()}) {
			return nil, ErrForbidden
		}
	}
	if s.o.AllowedTitles != nil {
		allowed, err := s.o.AllowedTitles(ctx, active, keys)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, ErrForbidden
		}
	} else {
		for _, key := range keys {
			if !s.o.Allowed(ctx, active, Subject{Kind: KindTitle, ID: key.String()}) {
				return nil, ErrForbidden
			}
		}
	}
	for start := 0; start < len(keys); start += 400 {
		chunk := keys[start:min(start+400, len(keys))]
		args := make([]any, 0, len(chunk)+2)
		args = append(args, viewer.ID, string(KindTitle))
		marks := make([]string, len(chunk))
		for i, key := range chunk {
			marks[i] = "?"
			args = append(args, key.String())
		}
		rows, err := s.o.DB.QueryContext(ctx, `SELECT subject_id FROM favorites_items WHERE viewer_id=? AND kind=? AND subject_id IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			key, err := core.ParseRatingKey(id)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[key] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// List returns permitted favorites newest first, without exposing hidden entries.
func (s *Service) List(ctx context.Context, viewer *core.User, kind Kind, limit int) ([]Entry, error) {
	if viewer == nil || viewer.ID <= 0 || (kind != KindTitle && kind != KindPerson && kind != KindCollection && kind != KindChannel) {
		return nil, ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return nil, ErrForbidden
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT subject_id,added_at FROM favorites_items WHERE viewer_id=? AND kind=? ORDER BY sort_order ASC,added_at DESC,subject_id ASC`, viewer.ID, string(kind))
	if err != nil {
		return nil, err
	}
	var candidates []Entry
	for rows.Next() {
		var id string
		var at int64
		if err = rows.Scan(&id, &at); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, Entry{Subject: Subject{Kind: kind, ID: id}, AddedAt: time.Unix(0, at).UTC()})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(candidates))
	for _, entry := range candidates {
		if s.o.Allowed(ctx, active, entry.Subject) {
			out = append(out, entry)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// removeTitleAt protects the playlist index from concurrent favorite changes.
// Policy callbacks run outside the transaction; a changed database snapshot
// returns a conflict rather than deleting a different entry.
func (s *Service) removeTitleAt(ctx context.Context, viewer *core.User, expected core.RatingKey, index int) error {
	if viewer == nil || viewer.ID <= 0 || index < 0 {
		return ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return ErrForbidden
	}
	read := func(query func(context.Context, string, ...any) (*sql.Rows, error)) ([]string, error) {
		rows, err := query(ctx, `SELECT subject_id FROM favorites_items WHERE viewer_id=? AND kind=? ORDER BY sort_order ASC,added_at DESC,subject_id ASC`, viewer.ID, string(KindTitle))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
	{
		before, err := read(s.o.DB.QueryContext)
		if err != nil {
			return err
		}
		var visible []string
		for _, id := range before {
			if s.o.Allowed(ctx, active, Subject{Kind: KindTitle, ID: id}) {
				visible = append(visible, id)
			}
		}
		if index >= len(visible) {
			return core.ErrNotFound
		}
		subject := Subject{Kind: KindTitle, ID: visible[index]}
		tx, err := s.o.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err = store.LockKey(ctx, store.DialectOf(s.o.DB), tx, fmt.Sprintf("favorites:viewer:%d", viewer.ID)); err != nil {
			tx.Rollback()
			return err
		}
		key, err := s.o.Marked.MarkedPlaylistKeyTx(ctx, tx, viewer.ID, "favorites")
		if err != nil {
			tx.Rollback()
			return err
		}
		if key != expected {
			tx.Rollback()
			return core.ErrNotFound
		}
		after, err := read(tx.QueryContext)
		if err != nil {
			tx.Rollback()
			return err
		}
		if !slices.Equal(before, after) {
			tx.Rollback()
			return errors.New("favorites: playlist changed concurrently")
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM favorites_items WHERE viewer_id=? AND kind=? AND subject_id=?`, viewer.ID, string(KindTitle), subject.ID)
		if err != nil {
			tx.Rollback()
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			tx.Rollback()
			return err
		}
		if n > 0 {
			if _, err = tx.ExecContext(ctx, `INSERT INTO favorites_mirror(viewer_id,disabled,updated_at) VALUES(?,0,?) ON CONFLICT(viewer_id) DO UPDATE SET updated_at=CASE WHEN excluded.updated_at>favorites_mirror.updated_at THEN excluded.updated_at ELSE favorites_mirror.updated_at+1 END`, viewer.ID, s.o.Now().UnixNano()); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if n > 0 {
			s.gen.Add(1)
			s.o.Changed(ctx, viewer.ID, subject)
		}
		return nil
	}
}

// SetMirror enables or disables the Plex projection without changing favorites.
// MirrorEnabled reports whether the viewer's Plex favorites mirror is enabled.
// Viewers without an explicit setting default to enabled.
func (s *Service) MirrorEnabled(ctx context.Context, viewer *core.User) (bool, error) {
	if viewer == nil || viewer.ID <= 0 {
		return false, ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return false, ErrForbidden
	}
	var disabled int
	err = s.o.DB.QueryRowContext(ctx, `SELECT disabled FROM favorites_mirror WHERE viewer_id=?`, viewer.ID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return disabled == 0, nil
}

func (s *Service) SetMirror(ctx context.Context, viewer *core.User, enabled bool) error {
	return s.setMirror(ctx, viewer, enabled, 0)
}

func (s *Service) setMirror(ctx context.Context, viewer *core.User, enabled bool, expected core.RatingKey) error {
	if viewer == nil || viewer.ID <= 0 {
		return ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return ErrForbidden
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.o.DB), tx, fmt.Sprintf("favorites:viewer:%d", viewer.ID)); err != nil {
		return err
	}
	if expected != 0 {
		key, err := s.o.Marked.MarkedPlaylistKeyTx(ctx, tx, viewer.ID, "favorites")
		if err != nil {
			return err
		}
		if key != expected {
			return core.ErrNotFound
		}
	}
	disabled := 1
	if enabled {
		disabled = 0
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO favorites_mirror(viewer_id,disabled) VALUES(?,?) ON CONFLICT(viewer_id) DO UPDATE SET disabled=excluded.disabled`, viewer.ID, disabled); err != nil {
		return err
	}
	if enabled {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM favorites_items WHERE viewer_id=? AND kind=?`, viewer.ID, string(KindTitle)).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err = s.o.Marked.EnsureMarkedPlaylistTx(ctx, tx, viewer.ID, "favorites", "Favorites"); err != nil {
				return err
			}
		}
	} else {
		err = s.o.Marked.DeleteMarkedPlaylistTx(ctx, tx, viewer.ID, "favorites")
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return err
		}
	}
	return tx.Commit()
}

// MoveTitle preserves hidden entries while changing the ordering of visible titles.
func (s *Service) MoveTitle(ctx context.Context, viewer *core.User, expected core.RatingKey, from, to int) error {
	if viewer == nil || viewer.ID <= 0 || from < 0 || to < 0 {
		return ErrForbidden
	}
	active, err := s.o.Active(ctx, viewer.ID)
	if err != nil || active == nil || active.ID != viewer.ID {
		return ErrForbidden
	}
	read := func(query func(context.Context, string, ...any) (*sql.Rows, error)) ([]string, error) {
		rows, err := query(ctx, `SELECT subject_id FROM favorites_items WHERE viewer_id=? AND kind=? ORDER BY sort_order ASC,added_at DESC,subject_id ASC`, viewer.ID, string(KindTitle))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
	before, err := read(s.o.DB.QueryContext)
	if err != nil {
		return err
	}
	var positions []int
	for i, id := range before {
		if s.o.Allowed(ctx, active, Subject{Kind: KindTitle, ID: id}) {
			positions = append(positions, i)
		}
	}
	if from >= len(positions) || to >= len(positions) {
		return core.ErrNotFound
	}
	ordered := append([]string(nil), before...)
	visible := make([]string, len(positions))
	for i, p := range positions {
		visible[i] = before[p]
	}
	item := visible[from]
	visible = append(visible[:from], visible[from+1:]...)
	visible = append(visible, "")
	copy(visible[to+1:], visible[to:])
	visible[to] = item
	for i, p := range positions {
		ordered[p] = visible[i]
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.o.DB), tx, fmt.Sprintf("favorites:viewer:%d", viewer.ID)); err != nil {
		return err
	}
	key, err := s.o.Marked.MarkedPlaylistKeyTx(ctx, tx, viewer.ID, "favorites")
	if err != nil {
		return err
	}
	if key != expected {
		return core.ErrNotFound
	}
	after, err := read(tx.QueryContext)
	if err != nil {
		return err
	}
	if !slices.Equal(before, after) {
		return errors.New("favorites: playlist changed concurrently")
	}
	if from == to {
		return tx.Commit()
	}
	for i, id := range ordered {
		if _, err = tx.ExecContext(ctx, `UPDATE favorites_items SET sort_order=? WHERE viewer_id=? AND kind=? AND subject_id=?`, i, viewer.ID, string(KindTitle), id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO favorites_mirror(viewer_id,disabled,updated_at) VALUES(?,0,?) ON CONFLICT(viewer_id) DO UPDATE SET updated_at=CASE WHEN excluded.updated_at>favorites_mirror.updated_at THEN excluded.updated_at ELSE favorites_mirror.updated_at+1 END`, viewer.ID, s.o.Now().UnixNano()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if from != to {
		lo, hi := min(from, to), max(from, to)
		for _, id := range visible[lo : hi+1] {
			s.o.Changed(ctx, viewer.ID, Subject{Kind: KindTitle, ID: id})
		}
	}
	return nil
}

// UpdatedAt reports the latest title favorite or order change.
func (s *Service) UpdatedAt(ctx context.Context, viewerID int64) (time.Time, error) {
	var at int64
	err := s.o.DB.QueryRowContext(ctx, `SELECT updated_at FROM favorites_mirror WHERE viewer_id=?`, viewerID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if at == 0 {
		return time.Time{}, nil
	}
	return time.Unix(0, at).UTC(), nil
}
