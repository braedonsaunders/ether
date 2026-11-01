package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/store"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestFrontendClientsCountDistinctAndSurface(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Unix(2_000_000_000, 0)
	clients := newFrontendClients(db, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now })
	clients.observe("plex", "device-one")
	clients.observe("plex", "device-one")
	clients.observe("plex", "device-two")
	clients.observe("silo", "device-one")
	clients.Close()
	count, known := clients.count(context.Background(), "plex")
	if !known || count != 2 {
		t.Fatal(count, known)
	}
	count, known = clients.count(context.Background(), "silo")
	if !known || count != 1 {
		t.Fatal(count, known)
	}
	var raw string
	if err := db.QueryRow(`SELECT device_hash FROM frontend_clients_seen LIMIT 1`).Scan(&raw); err != nil || raw == "device-one" || len(raw) != 64 {
		t.Fatal(raw, err)
	}
	now = now.Add(25 * time.Hour)
	count, known = clients.count(context.Background(), "plex")
	if !known || count != 0 {
		t.Fatal(count, known)
	}
}
func TestFrontendClientWithoutIdentifierIsUnknown(t *testing.T) {
	db := store.OpenTest(t)
	clients := newFrontendClients(db, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	clients.observe("plex", "")
	clients.Close()
	_, known := clients.count(context.Background(), "plex")
	if known {
		t.Fatal("missing device identifier reported exact")
	}
	if _, known := clients.count(context.Background(), "silo"); !known {
		t.Fatal("missing Plex identifier affected Silo")
	}
}
