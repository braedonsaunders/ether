package core

import (
	"context"
	"database/sql"
)

// MarkedPlaylists provides stable, per-viewer playlist identities independent of names.
type MarkedPlaylists interface {
	MarkedPlaylist(context.Context, int64, string) (*Playlist, error)
	EnsureMarkedPlaylist(context.Context, int64, string, string) (*Playlist, error)
}

// TransactionalMarkedPlaylists joins marked-playlist creation to an existing shared database transaction.
type TransactionalMarkedPlaylists interface {
	EnsureMarkedPlaylistTx(context.Context, *sql.Tx, int64, string, string) (RatingKey, error)
	DeleteMarkedPlaylistTx(context.Context, *sql.Tx, int64, string) error
	MarkedPlaylistKeyTx(context.Context, *sql.Tx, int64, string) (RatingKey, error)
}
