package identity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestQuickConnect(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,99,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	q, err := identity.NewQuickConnect(db, time.Minute, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	code, secret, err := q.Initiate(ctx, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 {
		t.Fatal(code)
	}
	details, err := q.DetailsSurface(ctx, "jellyfin", secret)
	if err != nil || !details.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("expiry: %+v %v", details, err)
	}
	if _, err = q.Poll(ctx, secret); !errors.Is(err, identity.ErrQuickConnectPending) {
		t.Fatalf("pending: %v", err)
	}
	if err = q.Approve(ctx, code, 1); err != nil {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, code, 1); !errors.Is(err, identity.ErrQuickConnectUsed) {
		t.Fatalf("duplicate approval: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, e := q.Poll(ctx, secret)
			if e == nil {
				results <- token
			}
		}()
	}
	wg.Wait()
	close(results)
	var tokens []string
	for token := range results {
		tokens = append(tokens, token)
	}
	if len(tokens) != 1 {
		t.Fatalf("successful redemptions: %d", len(tokens))
	}
	if err = q.Approve(ctx, code, 1); !errors.Is(err, identity.ErrQuickConnectUsed) {
		t.Fatalf("consumed approval: %v", err)
	}
	if err = q.Approve(ctx, "ZZZZZZ", 1); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("unknown code: %v", err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = grants.Resolve(ctx, "jellyfin", tokens[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = grants.Resolve(ctx, "plex", tokens[0]); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cross surface: %v", err)
	}
	code, secret, err = q.Initiate(ctx, "emby", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err = q.Approve(ctx, code, 1); !errors.Is(err, identity.ErrQuickConnectExpired) {
		t.Fatalf("expired approval: %v", err)
	}
	if _, err = q.Poll(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired poll: %v", err)
	}
	code, secret, err = q.Initiate(ctx, "emby", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, code, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Poll(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
}

func TestQuickConnectReplayIsDeviceBound(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,99,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0)
	q, err := identity.NewQuickConnect(db, time.Minute, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	phone := core.Device{ID: "apple-tv", Client: "Swiftfin"}
	code, secret, err := q.Initiate(ctx, "jellyfin", phone)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, code, 1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	first, err := q.PollSurfaceDevice(ctx, "jellyfin", secret, phone)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("redeem took %s", elapsed)
	}
	again, err := q.PollSurfaceDevice(ctx, "jellyfin", secret, phone)
	if err != nil || again != first {
		t.Fatalf("replay %q %v", again, err)
	}
	if _, err = q.PollSurfaceDevice(ctx, "jellyfin", secret, core.Device{ID: "other", Client: "Swiftfin"}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("other device: %v", err)
	}
	if _, err = q.PollSurfaceDevice(ctx, "jellyfin", secret, core.Device{ID: "apple-tv", Client: "Infuse"}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("other client: %v", err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(first))
	if err = grants.RevokeGrant(ctx, 1, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err = q.PollSurfaceDevice(ctx, "jellyfin", secret, phone); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked replay: %v", err)
	}
	code, secret, err = q.Initiate(ctx, "jellyfin", phone)
	if err != nil || q.Approve(ctx, code, 1) != nil {
		t.Fatal(err)
	}
	if _, err = q.PollSurfaceDevice(ctx, "jellyfin", secret, phone); err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second)
	if _, err = q.PollSurfaceDevice(ctx, "jellyfin", secret, phone); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired replay: %v", err)
	}
}

func TestQuickConnectRedeemStaysFastOnALargeDatabase(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,99,0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE quick_bulk(n INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 256<<10)
	for i := 0; i < 32; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO quick_bulk(n,payload) VALUES(?,?)`, i, payload); err != nil {
			t.Fatal(err)
		}
	}
	q, err := identity.NewQuickConnect(db, time.Minute, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, secret, err := q.Initiate(ctx, "jellyfin", core.Device{ID: "apple-tv", Client: "Swiftfin"})
	if err != nil || q.Approve(ctx, code, 1) != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err = q.PollSurfaceDevice(ctx, "jellyfin", secret, core.Device{ID: "apple-tv", Client: "Swiftfin"}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("large-db redeem took %s", elapsed)
	}
}

func TestQuickConnectApprovalNamesTheDevice(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,99,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0)
	q, err := identity.NewQuickConnect(db, time.Minute, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := q.Initiate(ctx, "jellyfin", core.Device{DeviceName: "Living Room", Product: "Jellyfin", Client: "Apple TV"})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, " "+strings.ToLower(code[:3])+"-"+code[3:]+" ", 1); err != nil {
		t.Fatal(err)
	}
	target, err := q.ApprovedTarget(ctx, code, 1)
	if err != nil || target.DeviceName != "Living Room" || target.Surface != "jellyfin" || target.Product != "Jellyfin" {
		t.Fatalf("target: %+v %v", target, err)
	}
}
