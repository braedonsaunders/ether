package auth

import (
	"context"
	"database/sql"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
)

// StoredIdentity looks up an import destination without granting access or
// creating an authentication service. Inactive viewers retain their history.
func StoredIdentity(ctx context.Context, db *sql.DB, id int64) (*core.User, error) {
	var u core.User
	err := db.QueryRowContext(ctx, `SELECT id,COALESCE(plex_id,0),username FROM auth_users WHERE id=?`, id).Scan(&u.ID, &u.PlexID, &u.Username)
	if errors.Is(err, sql.ErrNoRows) {
		err = core.ErrNotFound
	}
	return &u, err
}
