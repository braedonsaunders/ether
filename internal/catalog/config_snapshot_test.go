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
	evidence, ok := c.CaptureConfigEvidence(initial)
	if !ok || !evidence.Current() {
		t.Fatal("initial evidence missing")
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
	c.UpdateConfig(initial)
	if evidence.Current() {
		t.Fatal("config A-B-A revived old evidence")
	}
	if fresh, ok := c.CaptureConfigEvidence(initial); !ok || !fresh.Current() {
		t.Fatal("fresh evidence missing")
	}
}

func TestCatalogConfigDoesNotAliasCallerRules(t *testing.T) {
	cfg := config.Catalog{Exclusions: []core.LibraryExclusion{{Name: "country", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{{Field: "country", Op: "is", Value: []byte(`"US"`)}}}}}
	expected := cloneCatalogConfig(cfg)
	c, err := New(store.OpenTest(t), nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Exclusions[0].Rules[0].Value[1] = 'X'
	if !c.MatchesConfig(expected) {
		t.Fatal("constructor retained caller-owned rule bytes")
	}
	c.UpdateConfig(expected)
	evidence, ok := c.CaptureConfigEvidence(expected)
	if !ok {
		t.Fatal("updated evidence missing")
	}
	expected.Exclusions[0].Rules[0].Value[1] = 'Y'
	if !evidence.Current() || c.config().Exclusions[0].Rules[0].Value[1] != 'U' {
		t.Fatal("update retained caller-owned rule bytes")
	}
}
