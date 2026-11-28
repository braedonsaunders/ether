package newznab

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/vault"
)

func TestNZBStoreFailureExpires(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	c := New(config.Indexer{ID: "ix", Name: "IX", Enabled: true}, nil, Options{
		Now: func() time.Time { return now },
	})
	rel := core.Release{ID: "rel-1", Name: "Example.2026.1080p"}

	c.nzbFlightMu.Lock()
	c.nzbStoreFailures = map[string]time.Time{"rel-1": now.Add(5 * time.Minute)}
	c.nzbFlightMu.Unlock()

	if err := c.nzbRefusal(context.Background(), rel); !errors.Is(err, vault.ErrPayloadMissing) {
		t.Fatalf("during TTL: err = %v, want vault.ErrPayloadMissing", err)
	}
	now = now.Add(6 * time.Minute)
	if err := c.nzbRefusal(context.Background(), rel); err != nil {
		t.Fatalf("after TTL: err = %v, want nil", err)
	}
}
