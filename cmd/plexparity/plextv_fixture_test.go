package main

import (
	"context"
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
