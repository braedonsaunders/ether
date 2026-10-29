//go:build plexparity

package app

import (
	"testing"
	"time"
)

func TestParityClock(t *testing.T) {
	got := parityClock()()
	want := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("clock = %v, want %v", got, want)
	}
	t.Setenv("ETHER_PARITY_TMDB_URL", "http://127.0.0.1:1234/3")
	if len(parityTMDBOptions()) == 0 || len(parityCatalogueOptions()) == 0 || len(parityCollectionOptions()) == 0 {
		t.Fatal("fixture dependencies not installed")
	}
}
