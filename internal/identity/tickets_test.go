package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestTicketCrossNodeScopeAndExpiry(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	clock := func() time.Time { return now }
	g, err := identity.NewGrants(db, members{db}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	d := core.Device{ID: "tv"}
	access, err := g.Issue(ctx, 1, nil, "silo", d)
	if err != nil {
		t.Fatal(err)
	}
	node1, _ := identity.NewTicketService(db, "silo", g, clock)
	node2, _ := identity.NewTicketService(db, "silo", g, clock)
	other, _ := identity.NewTicketService(db, "emby", g, clock)
	ticket, err := node1.Issue(ctx, access, d, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Consume(ctx, ticket, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("surface: %v", err)
	}
	if _, err = node2.Consume(ctx, ticket, "other"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("purpose: %v", err)
	}
	got, err := node2.Consume(ctx, ticket, "events-ws")
	if err != nil || got.Viewer.ID != 1 || got.Device.ID != "tv" {
		t.Fatalf("consume: %+v %v", got, err)
	}
	if ok, err := node2.Active(ctx, got); err != nil || !ok {
		t.Fatalf("active %v %v", ok, err)
	}
	if _, err = node1.Consume(ctx, ticket, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("replay %v", err)
	}

	ticket, err = node1.Issue(ctx, access, d, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	for j := range results {
		wg.Add(1)
		go func(j int) { defer wg.Done(); _, results[j] = node2.Consume(ctx, ticket, "events-ws") }(j)
	}
	wg.Wait()
	success := 0
	for _, e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, core.ErrUnauthorized) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("concurrent use: %v", results)
	}
	ticket, err = node1.Issue(ctx, access, d, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if _, err = node2.Consume(ctx, ticket, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expiry: %v", err)
	}
	ticket, err = node1.Issue(ctx, access, d, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if ok, err := node2.Active(ctx, got); err != nil || ok {
		t.Fatalf("inactive live ticket %v %v", ok, err)
	}
	if _, err = node2.Consume(ctx, ticket, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
}

// A ticket written by an older node remains consumable after codec extraction.
func TestTicketConsumesLegacyHash(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	clock := func() time.Time { return now }
	grants, err := identity.NewGrants(db, members{db}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	credential, err := grants.Issue(ctx, 1, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	grantHash := sha256.Sum256([]byte(credential))
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	digest := sha256.Sum256([]byte(token))
	if _, err = db.Exec(`INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES(?,?,?,?,?,?,?)`, hex.EncodeToString(digest[:]), "silo", "events-ws", 1, hex.EncodeToString(grantHash[:]), `{"ID":"tv"}`, now.Add(30*time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewTicketService(db, "silo", grants, clock)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := service.Consume(ctx, token, "events-ws")
	if err != nil || ticket.Viewer == nil || ticket.Viewer.ID != 1 || ticket.Device.ID != "tv" {
		t.Fatalf("legacy ticket consume failed: %v", err)
	}
}
