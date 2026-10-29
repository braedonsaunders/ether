package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestResolveDeviceIsSurfaceBoundAndReadOnly(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,is_owner,created_at,updated_at) VALUES(1,1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0).UTC()
	grants, err := identity.NewGrants(db, members{db}, 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "apple-device"}
	token, err := grants.Issue(ctx, 1, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	var beforeUsed, beforeExpiry int64
	if err = db.QueryRowContext(ctx, `SELECT last_used,expires FROM identity_grants WHERE viewer_id=1`).Scan(&beforeUsed, &beforeExpiry); err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * 24 * time.Hour)
	viewer, expiry, err := grants.ResolveDevice(ctx, "silo", token, device)
	if err != nil || viewer == nil || viewer.ID != 1 || !viewer.IsOwner || !expiry.Equal(time.UnixMilli(beforeExpiry)) {
		t.Fatalf("viewer=%+v expiry=%s err=%v", viewer, expiry, err)
	}
	for _, tc := range []struct {
		surface string
		device  core.Device
	}{{"emby", device}, {"silo", core.Device{ID: "other"}}, {"silo", core.Device{}}} {
		if _, _, err = grants.ResolveDevice(ctx, tc.surface, token, tc.device); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("wrong binding %+v: %v", tc, err)
		}
	}
	var afterUsed, afterExpiry int64
	if err = db.QueryRowContext(ctx, `SELECT last_used,expires FROM identity_grants WHERE viewer_id=1`).Scan(&afterUsed, &afterExpiry); err != nil {
		t.Fatal(err)
	}
	if afterUsed != beforeUsed || afterExpiry != beforeExpiry {
		t.Fatalf("read changed grant: %d/%d -> %d/%d", beforeUsed, beforeExpiry, afterUsed, afterExpiry)
	}
	var observations int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_viewer_times WHERE viewer_id=1`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("device validation wrote activity: %d %v", observations, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = grants.ResolveDevice(ctx, "silo", token, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	now = time.UnixMilli(beforeExpiry)
	if _, _, err = grants.ResolveDevice(ctx, "silo", token, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired: %v", err)
	}
	now = now.Add(-time.Hour)
	if err = grants.RevokeDevice(ctx, 1, device.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = grants.ResolveDevice(ctx, "silo", token, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked: %v", err)
	}
}
