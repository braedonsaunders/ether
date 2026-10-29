package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func NewPreferences(db *sql.DB, surface frontend.Protocol) *Preferences {
	return &Preferences{db: db, surface: surface}
}

func init() {
	store.Register("frontend_preferences", 1, `CREATE TABLE frontend_preferences (viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, surface TEXT NOT NULL, client TEXT NOT NULL, pref_key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(viewer_id,surface,client,pref_key));`)
}

const maxPreferenceBytes = 64 * 1024
const maxPreferencesPerViewer = 256

type Preferences struct {
	db      *sql.DB
	surface frontend.Protocol
}

func preferenceKeyOK(s string) bool {
	return s != "" && len(s) <= 128 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func (s Preferences) valid(viewer *core.Viewer, client, key string) error {
	if s.db == nil || s.surface == "" || viewer == nil || viewer.ID <= 0 {
		return core.ErrUnauthorized
	}
	if !preferenceKeyOK(client) || !preferenceKeyOK(key) {
		return frontend.ErrInvalidPreferences
	}
	return nil
}
func (s Preferences) Get(ctx context.Context, viewer *core.Viewer, client, key string) (json.RawMessage, error) {
	if err := s.valid(viewer, client, key); err != nil {
		return nil, err
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT p.value FROM frontend_preferences p JOIN auth_users u ON u.id=p.viewer_id WHERE p.viewer_id=? AND p.surface=? AND p.client=? AND p.pref_key=? AND u.active=1`, viewer.ID, string(s.surface), client, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, frontend.ErrPreferencesNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
func (s Preferences) Put(ctx context.Context, viewer *core.Viewer, client, key string, raw json.RawMessage) error {
	if err := s.valid(viewer, client, key); err != nil {
		return err
	}
	if len(raw) > maxPreferenceBytes {
		return frontend.ErrPreferencesLimit
	}
	if !json.Valid(raw) {
		return frontend.ErrInvalidPreferences
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serializes quota checks for a viewer on SQLite and PostgreSQL.
	lock, err := tx.ExecContext(ctx, `UPDATE auth_users SET id=id WHERE id=? AND active=1`, viewer.ID)
	if err != nil {
		return err
	}
	n, err := lock.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrUnauthorized
	}
	var exists, count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_preferences WHERE viewer_id=? AND surface=? AND client=? AND pref_key=?`, viewer.ID, string(s.surface), client, key).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_preferences WHERE viewer_id=?`, viewer.ID).Scan(&count); err != nil {
			return err
		}
		if count >= maxPreferencesPerViewer {
			return frontend.ErrPreferencesLimit
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO frontend_preferences(viewer_id,surface,client,pref_key,value) VALUES(?,?,?,?,?) ON CONFLICT(viewer_id,surface,client,pref_key) DO UPDATE SET value=excluded.value`, viewer.ID, string(s.surface), client, key, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Preferences) Delete(ctx context.Context, viewer *core.Viewer, client, key string) error {
	if err := s.valid(viewer, client, key); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM frontend_preferences WHERE viewer_id=? AND surface=? AND client=? AND pref_key=? AND EXISTS (SELECT 1 FROM auth_users WHERE id=? AND active=1)`, viewer.ID, string(s.surface), client, key, viewer.ID)
	return err
}

var _ frontend.Preferences = Preferences{}
