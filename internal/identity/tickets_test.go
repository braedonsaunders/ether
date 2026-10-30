package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
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

func TestTicketServiceUsesSharedHeaderAndClock(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 123000000)
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
	service, err := identity.NewTicketService(db, "silo", grants, clock)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.Issue(ctx, credential, device, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	var a, b int64
	if err = db.QueryRow(`SELECT h.expires FROM capability_headers h JOIN capability_tickets t ON t.namespace=h.namespace AND t.digest=h.digest WHERE h.namespace='ticket' AND h.digest=?`, hash).Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT expires FROM capability_headers WHERE namespace='ticket' AND digest=?`, hash).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if a != now.Add(30*time.Second).UnixMilli() || a != b {
		t.Fatalf("legacy=%d header=%d now=%d", a, b, now.UnixMilli())
	}
	ticket, err := service.Consume(ctx, token, "events-ws")
	if err != nil || ticket.Viewer == nil || ticket.Device.ID != "tv" {
		t.Fatalf("consume: %v", err)
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, hash).Scan(&n); err != nil || n != 0 {
		t.Fatalf("header count=%d err=%v", n, err)
	}
}

// Maintenance must use the same injected clock as the issuing service.
func TestTicketServiceMaintenanceUsesIssuingClock(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 123000000)
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
	service, err := identity.NewTicketService(db, "silo", grants, clock)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.Issue(ctx, credential, device, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(digest[:])
	now = now.Add(30*time.Second - time.Millisecond)
	if _, err := capability.PruneExpired(ctx, db, clock()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM capability_tickets WHERE namespace='ticket' AND digest=?`, hash).Scan(&n); err != nil || n != 1 {
		t.Fatalf("before expiry count=%d err=%v", n, err)
	}
	now = now.Add(time.Millisecond)
	if _, err := capability.PruneExpired(ctx, db, clock()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM capability_tickets WHERE namespace='ticket' AND digest=?`, hash).Scan(&n); err != nil || n != 0 {
		t.Fatalf("at expiry count=%d err=%v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, hash).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired header count=%d err=%v", n, err)
	}
}
