package trakt

import (
	"errors"
	"fmt"
	"testing"
)

// watchlist recognizes the unlinked state through NotConnected without
// importing this package.
func TestErrNotConnectedIsRecognizableWithoutImport(t *testing.T) {
	err := fmt.Errorf("sync viewer: %w", ErrNotConnected)
	var unlinked interface{ NotConnected() bool }
	if !errors.As(err, &unlinked) || !unlinked.NotConnected() {
		t.Fatal("wrapped ErrNotConnected does not expose NotConnected")
	}
	if !errors.Is(err, ErrNotConnected) {
		t.Fatal("errors.Is no longer matches ErrNotConnected")
	}
	if errors.As(ErrRevoked, &unlinked) {
		t.Fatal("other errors must not look unlinked")
	}
}
