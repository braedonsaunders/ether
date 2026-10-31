package perfharness

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
)

// Credentials minted into the throwaway copy. Every viewer signs in with
// Password through the real protocol sign-in routes; Plex and the owner
// console use a minted Plex token; Stremio uses a minted surface grant.
const Password = "etherperf-password"

type Viewer struct {
	ID         int64
	Username   string
	Owner      bool
	Restricted bool
	PlexToken  string
	Stremio    string
}

type activeViewers struct{ db *sql.DB }

func (a activeViewers) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	var u core.User
	var owner int
	err := a.db.QueryRowContext(ctx, `SELECT id, username, is_owner FROM auth_users WHERE id=? AND active=1`, id).Scan(&u.ID, &u.Username, &owner)
	if err != nil {
		return nil, core.ErrUnauthorized
	}
	u.IsOwner = owner == 1
	return &u, nil
}

// MintCredentials gives up to max active viewers of a throwaway database a
// known password, a Plex token and a Stremio grant. The owner comes first.
// Never run it against a real server's database.
func MintCredentials(ctx context.Context, db *sql.DB, max int) ([]Viewer, error) {
	rows, err := db.QueryContext(ctx, `SELECT u.id, u.is_owner, u.restricted, COALESCE((SELECT subject FROM identity_links l WHERE l.viewer_id=u.id AND l.provider='local' LIMIT 1), u.username)
		FROM auth_users u WHERE u.active=1 AND u.owner_enabled=1 ORDER BY u.is_owner DESC, u.id LIMIT ?`, max)
	if err != nil {
		return nil, err
	}
	var viewers []Viewer
	for rows.Next() {
		var v Viewer
		var owner, restricted int
		if err := rows.Scan(&v.ID, &owner, &restricted, &v.Username); err != nil {
			rows.Close()
			return nil, err
		}
		v.Owner, v.Restricted = owner == 1, restricted == 1
		viewers = append(viewers, v)
	}
	rows.Close()
	if len(viewers) == 0 {
		return nil, errors.New("no active viewers in the copied database")
	}
	local, err := identity.NewLocal(db)
	if err != nil {
		return nil, err
	}
	grants, err := identity.NewGrants(db, activeViewers{db}, 30*24*time.Hour, nil)
	if err != nil {
		return nil, err
	}
	for i := range viewers {
		v := &viewers[i]
		if err := local.UpsertPassword(ctx, v.ID, v.Username, Password); err != nil {
			return nil, fmt.Errorf("password for %s: %w", v.Username, err)
		}
		var raw [20]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		v.PlexToken = "perf" + hex.EncodeToString(raw[:])
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash, user_id, source, created_at) VALUES(?,?,?,?)`,
			auth.HashToken(v.PlexToken), v.ID, "etherperf", time.Now().Unix()); err != nil {
			return nil, fmt.Errorf("plex token for %s: %w", v.Username, err)
		}
		v.Stremio, err = grants.Issue(ctx, v.ID, nil, "stremio", core.Device{ID: "etherperf-stremio-" + v.Username, Client: "stremio", Product: "stremio"})
		if err != nil {
			return nil, fmt.Errorf("stremio grant for %s: %w", v.Username, err)
		}
	}
	return viewers, nil
}
