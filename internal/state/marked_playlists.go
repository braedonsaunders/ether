package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("state", 3, `CREATE TABLE state_playlist_markers (user_id INTEGER NOT NULL, marker TEXT NOT NULL, playlist_id INTEGER NOT NULL UNIQUE, PRIMARY KEY(user_id,marker), FOREIGN KEY(playlist_id) REFERENCES state_playlists(id) ON DELETE CASCADE);`)
}
func (v playlistsView) MarkedPlaylist(ctx context.Context, user int64, marker string) (*core.Playlist, error) {
	var id int64
	err := v.s.db.QueryRowContext(ctx, `SELECT m.playlist_id FROM state_playlist_markers m JOIN state_playlists p ON p.id=m.playlist_id AND p.user_id=m.user_id WHERE m.user_id=? AND m.marker=?`, user, marker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v.s.loadPlaylist(ctx, user, id)
}
func (v playlistsView) EnsureMarkedPlaylist(ctx context.Context, user int64, marker, title string) (*core.Playlist, error) {
	if user <= 0 || marker == "" || title == "" {
		return nil, errors.New("invalid marked playlist")
	}
	tx, err := v.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	key, err := v.EnsureMarkedPlaylistTx(ctx, tx, user, marker, title)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return v.s.GetPlaylist(ctx, user, key)
}

func (v playlistsView) EnsureMarkedPlaylistTx(ctx context.Context, tx *sql.Tx, user int64, marker, title string) (core.RatingKey, error) {
	if tx == nil || user <= 0 || marker == "" || title == "" {
		return 0, errors.New("invalid marked playlist")
	}
	if err := store.LockKey(ctx, store.DialectOf(v.s.db), tx, fmt.Sprintf("playlist-marker:%d:%s", user, marker)); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT m.playlist_id FROM state_playlist_markers m JOIN state_playlists p ON p.id=m.playlist_id AND p.user_id=m.user_id WHERE m.user_id=? AND m.marker=?`, user, marker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Remove an orphan marker left by databases without foreign-key enforcement.
		if _, err = tx.ExecContext(ctx, `DELETE FROM state_playlist_markers WHERE user_id=? AND marker=?`, user, marker); err != nil {
			return 0, err
		}
		if err = tx.QueryRowContext(ctx, `INSERT INTO state_playlists(user_id,title,smart,updated_at) VALUES(?,?,0,?) RETURNING id`, user, title, v.s.now().UTC().UnixNano()).Scan(&id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO state_playlist_markers(user_id,marker,playlist_id) VALUES(?,?,?)`, user, marker, id); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	return core.PlaylistKey(id), nil
}

var _ core.MarkedPlaylists = playlistsView{}

func (v playlistsView) DeleteMarkedPlaylistTx(ctx context.Context, tx *sql.Tx, user int64, marker string) error {
	if tx == nil || user <= 0 || marker == "" {
		return errors.New("invalid marked playlist")
	}
	if err := store.LockKey(ctx, store.DialectOf(v.s.db), tx, fmt.Sprintf("playlist-marker:%d:%s", user, marker)); err != nil {
		return err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT playlist_id FROM state_playlist_markers WHERE user_id=? AND marker=?`, user, marker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM state_playlist_markers WHERE user_id=? AND marker=?`, user, marker); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM state_playlist_items WHERE playlist_id=?`, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM state_playlists WHERE id=? AND user_id=?`, id, user)
	return err
}

func (v playlistsView) MarkedPlaylistKeyTx(ctx context.Context, tx *sql.Tx, user int64, marker string) (core.RatingKey, error) {
	if tx == nil || user <= 0 || marker == "" {
		return 0, errors.New("invalid marked playlist")
	}
	if err := store.LockKey(ctx, store.DialectOf(v.s.db), tx, fmt.Sprintf("playlist-marker:%d:%s", user, marker)); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT m.playlist_id FROM state_playlist_markers m JOIN state_playlists p ON p.id=m.playlist_id AND p.user_id=m.user_id WHERE m.user_id=? AND m.marker=?`, user, marker).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, core.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return core.PlaylistKey(id), nil
}
