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

func TestGrantReferenceRevalidatesSurfaceMembershipAndExpiry(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	g, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token, err := g.Issue(ctx, 1, nil, "stremio", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	ref, viewer, err := g.GrantReference(ctx, "stremio", token)
	if err != nil || viewer.ID != 1 || ref == token || len(ref) != 64 {
		t.Fatalf("reference: %q %+v %v", ref, viewer, err)
	}
	if err = g.ValidateGrantReference(ctx, "stremio", ref, 1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		surface, ref string
		viewer       int64
	}{{"webdav", ref, 1}, {"stremio", ref, 2}, {"stremio", token, 1}, {"stremio", "malformed", 1}} {
		if err = g.ValidateGrantReference(ctx, tc.surface, tc.ref, tc.viewer); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("accepted wrong scope: %+v %v", tc, err)
		}
	}
	now = now.Add(59 * time.Minute)
	if err = g.ValidateGrantReference(ctx, "stremio", ref, 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err = g.ValidateGrantReference(ctx, "stremio", ref, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("validation extended expiry: %v", err)
	}
	token, err = g.Issue(ctx, 1, nil, "stremio", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err = g.GrantReference(ctx, "stremio", token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = g.ValidateGrantReference(ctx, "stremio", ref, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive viewer: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = g.RevokeGrant(ctx, 1, ref); err != nil {
		t.Fatal(err)
	}
	if err = g.ValidateGrantReference(ctx, "stremio", ref, 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked grant: %v", err)
	}
}

type plexAuth struct{ calls int }

func (p *plexAuth) Authenticate(_ context.Context, token string) (*core.User, error) {
	p.calls++
	if token == "plex-token" {
		return &core.User{ID: 1}, nil
	}
	return nil, core.ErrUnauthorized
}
func TestResolverSeparatesPlexAndGrants(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	plex := &plexAuth{}
	resolver, err := identity.NewResolver(plex, grants)
	if err != nil {
		t.Fatal(err)
	}
	token, err := grants.Issue(ctx, 1, nil, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(ctx, "plex", token); !errors.Is(err, core.ErrUnauthorized) || plex.calls != 0 {
		t.Fatalf("grant reached Plex validator: %v calls %d", err, plex.calls)
	}
	if _, err = resolver.Resolve(ctx, "jellyfin", "plex-token"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("Plex token accepted on Jellyfin: %v", err)
	}
	if _, err = resolver.Resolve(ctx, "plex", "plex-token"); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(ctx, "jellyfin", token); err != nil {
		t.Fatal(err)
	}
}

type staleOwner struct{}

func (staleOwner) ActiveUser(_ context.Context, id int64) (*core.User, error) {
	return &core.User{ID: id, IsOwner: true}, nil
}
func TestGrantOwnerRoleUsesCurrentDatabase(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,is_owner,created_at,updated_at) VALUES(1,1,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, staleOwner{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := grants.Issue(ctx, 1, nil, "owner", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	user, err := grants.Resolve(ctx, "owner", token)
	if err != nil {
		t.Fatal(err)
	}
	if user.IsOwner {
		t.Fatal("stale owner role accepted")
	}
}
