package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

type members struct{ db *sql.DB }

func (m members) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	var active int
	err := m.db.QueryRowContext(ctx, `SELECT active FROM auth_users WHERE id=?`, id).Scan(&active)
	if err != nil || active != 1 {
		return nil, core.ErrUnauthorized
	}
	return &core.User{ID: id}, nil
}

func TestGrantsSurfaceMembershipAndRevocation(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	g, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	surfaces := []string{"plex", "jellyfin", "emby", "silo", "stremio", "webdav", "dlna", "owner", "viewer-portal", "future"}
	tokens := make([]string, len(surfaces))
	for i, s := range surfaces {
		tokens[i], err = g.Issue(ctx, 1, nil, s, core.Device{ID: "device"})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, s := range surfaces {
		for j, other := range surfaces {
			_, err = g.Resolve(ctx, other, tokens[i])
			if i == j && err != nil {
				t.Fatalf("%s own: %v", s, err)
			}
			if i != j && !errors.Is(err, core.ErrUnauthorized) {
				t.Fatalf("%s accepted on %s: %v", s, other, err)
			}
		}
	}
	if _, err = g.Resolve(ctx, "jellyfin", "plex.tv-token"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("Plex credential accepted: %v", err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM identity_grants WHERE hash=?`, tokens[0]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stored plaintext: %d %v", count, err)
	}
	now = now.Add(59 * time.Minute)
	if _, err = g.Resolve(ctx, "plex", tokens[0]); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err = g.Resolve(ctx, "plex", tokens[0]); err != nil {
		t.Fatalf("sliding expiration: %v", err)
	}
	if _, err = g.Resolve(ctx, "jellyfin", tokens[1]); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired: %v", err)
	}
	_, err = db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.Resolve(ctx, "plex", tokens[0]); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
	_, err = db.ExecContext(ctx, `UPDATE auth_users SET active=1 WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.RevokeDevice(ctx, 1, "device"); err != nil {
		t.Fatal(err)
	}
	if _, err = g.Resolve(ctx, "plex", tokens[0]); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked: %v", err)
	}
}
