package newznab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestCapsCacheFollowsIdentityAndExpires(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, personaCaps)
	}))
	defer srv.Close()
	clock := &fakeClock{t: time.Unix(0, 0)}
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	c := New(cfg, nil, Options{Identity: servarrPolicy(), Version: "1.0", Now: clock.now})
	ctx := context.Background()

	if _, err := c.CapsFor(ctx, mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("caps requests = %d", requests.Load())
	}
	clock.add(6 * 24 * time.Hour)
	if _, err := c.CapsFor(ctx, mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("caps refetched inside the 7-day window: %d", requests.Load())
	}
	clock.add(2 * 24 * time.Hour)
	if _, err := c.CapsFor(ctx, mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("caps not refetched after 7 days: %d", requests.Load())
	}

	// Another resolved identity for the same media is a different cache entry.
	c.SetIdentityPolicy(config.ClientIdentity{SearchTV: IdentityProwlarr, GrabTV: config.IdentitySame, GrabMovies: config.IdentitySame})
	if _, err := c.CapsFor(ctx, mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("identity change reused cached caps: %d", requests.Load())
	}

	// An indexer search override is a new resolved identity for the same
	// media, so capabilities are fetched for it.
	changed := c.Config()
	changed.SearchIdentity = IdentityNZBHydra2
	c.UpdateConfig(changed)
	if _, err := c.CapsFor(ctx, mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 {
		t.Fatalf("override change reused cached caps: %d", requests.Load())
	}
}

func TestCapsCacheKeepsBothMediaIdentities(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, personaCaps)
	}))
	defer srv.Close()
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	c := New(cfg, nil, Options{Identity: servarrPolicy(), Version: "1.0"})
	ctx := context.Background()
	for range 3 {
		if _, err := c.CapsFor(ctx, mediaTV); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CapsFor(ctx, mediaMovie); err != nil {
			t.Fatal(err)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("caps requests = %d, want one per media identity", got)
	}
}

func TestCapsCachePersistsPerIdentity(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, personaCaps)
	}))
	defer srv.Close()
	db := store.OpenTest(t)
	clock := &fakeClock{t: time.Unix(0, 0)}
	cfg := config.Indexer{ID: "ix", Name: "Fake", URL: srv.URL, APIKey: "k", Enabled: true}
	opts := Options{Identity: servarrPolicy(), Version: "1.0", Now: clock.now}
	first := New(cfg, db, opts)
	if _, err := first.CapsFor(context.Background(), mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}

	restarted := New(cfg, db, opts)
	if _, err := restarted.CapsFor(context.Background(), mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("restart refetched caps inside the window: %d", requests.Load())
	}

	clock.add(8 * 24 * time.Hour)
	expired := New(cfg, db, opts)
	if _, err := expired.CapsFor(context.Background(), mediaTV); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("expired caps not refetched: %d", requests.Load())
	}
}
