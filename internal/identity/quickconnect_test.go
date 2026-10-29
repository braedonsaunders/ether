package identity_test

import (
	"context"
	"errors"
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
	if err = q.Approve(ctx, code, 1); !errors.Is(err, core.ErrUnauthorized) {
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
	if err = q.Approve(ctx, code, 1); !errors.Is(err, core.ErrUnauthorized) {
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
