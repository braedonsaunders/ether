package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestPlexTVFixtureEntitlement(t *testing.T) {
	server := httptest.NewServer(plextvFixture())
	defer server.Close()
	c, err := plextv.New(plextv.Options{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	account, err := c.User(context.Background(), "parity-owner")
	if err != nil || !account.Subscription.Active || account.ID != 1 {
		t.Fatalf("owner=%+v err=%v", account, err)
	}
	if _, err = c.Features(context.Background(), "parity-owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.User(context.Background(), "wrong"); err == nil {
		t.Fatal("accepted unknown fixture token")
	}
}

func TestPlexTVFixtureRecordsUnknownEndpoints(t *testing.T) {
	tracker := &fixtureTracker{}
	server := httptest.NewServer(trackedPlexTVFixture(tracker))
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Plex-Token", "parity-owner")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || len(tracker.failures()) != 1 {
		t.Fatalf("status=%d missing=%v", resp.StatusCode, tracker.failures())
	}
}
