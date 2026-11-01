package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A viewer kept only for imported history has no server access, so the
// favorites and not-interested stores refuse them. Their taste must still
// build: the console's viewer pages showed "favorites: viewer or subject
// unavailable" instead.
func TestInactiveViewerHasNoFavoriteOrHiddenSignals(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, Options{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	historyOnly := &core.User{ID: 4242, Username: "imported-only"}
	if got, err := a.favoriteSignals(ctx, historyOnly); err != nil || len(got) != 0 {
		t.Fatalf("favoriteSignals = %v, %v; want none and no error", got, err)
	}
	if got, err := a.notInterestedSignals(ctx, historyOnly); err != nil || len(got) != 0 {
		t.Fatalf("notInterestedSignals = %v, %v; want none and no error", got, err)
	}
	if got, _, err := a.notInterestedSnapshot(ctx, historyOnly); err != nil || len(got) != 0 {
		t.Fatalf("notInterestedSnapshot = %v, %v; want none and no error", got, err)
	}
}
