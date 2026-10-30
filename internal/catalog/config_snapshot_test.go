package catalog

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestCatalogMatchesInstalledPolicyConfig(t *testing.T) {
	initial := config.Catalog{MinVotes: 42}
	c, err := New(store.OpenTest(t), nil, initial, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MatchesConfig(initial) {
		t.Fatal("initial config mismatch")
	}
	invalid := initial
	invalid.Exclusions = []core.LibraryExclusion{{Name: "invalid", MediaType: "invalid"}}
	c.UpdateConfig(invalid)
	if c.MatchesConfig(invalid) || !c.MatchesConfig(initial) {
		t.Fatal("rejected config was reported installed")
	}
	next := initial
	next.GateMoviesByHomeRelease = true
	c.UpdateConfig(next)
	if !c.MatchesConfig(next) || c.MatchesConfig(initial) {
		t.Fatal("installed replacement not observed")
	}
}
