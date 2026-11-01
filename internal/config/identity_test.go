package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientIdentityDefaults(t *testing.T) {
	cfg := Defaults(t.TempDir())
	id := cfg.ClientIdentity
	if id.SearchTV != "" || id.SearchMovies != "" {
		t.Fatalf("search defaults = %+v, want ether (empty)", id)
	}
	if id.GrabTV != IdentitySame || id.GrabMovies != IdentitySame {
		t.Fatalf("grab defaults = %+v, want same as search", id)
	}
	if id.UserAgent != "" || len(id.Headers) != 0 {
		t.Fatalf("custom defaults = %+v, want empty", id)
	}
}

func TestClientIdentityReloadsFromExistingConfig(t *testing.T) {
	dir := t.TempDir()
	doc := `{"server":{"port":1234,"machineIdentifier":"m"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id := s.Get().ClientIdentity; id.GrabTV != IdentitySame || id.GrabMovies != IdentitySame {
		t.Fatalf("grab defaults lost on reload: %+v", id)
	}
}

func TestClientIdentityRoundTripAndSealedHeaders(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := ClientIdentity{
		SearchTV:     "sonarr",
		SearchMovies: "radarr",
		GrabTV:       IdentitySame,
		GrabMovies:   IdentitySame,
		UserAgent:    "Custom/1.0",
		Headers:      map[string]string{"Authorization": "Bearer header-secret"},
	}
	ix := Indexer{
		ID: "a", Name: "A", URL: "https://ix.example", Enabled: true,
		SearchIdentity: "prowlarr",
		GrabIdentity:   "custom",
		UserAgent:      "Indexer/2.0",
		Headers:        map[string]string{"X-Api-Key": "indexer-header-secret"},
	}
	if err := s.Update(func(c *Config) error {
		c.ClientIdentity = want
		c.Indexers = []Indexer{ix}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"header-secret", "indexer-header-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("header value %q stored in plaintext", secret)
		}
	}
	reloaded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Get()
	if got.ClientIdentity.SearchTV != want.SearchTV || got.ClientIdentity.UserAgent != want.UserAgent ||
		got.ClientIdentity.Headers["Authorization"] != want.Headers["Authorization"] {
		t.Fatalf("client identity = %+v, want %+v", got.ClientIdentity, want)
	}
	gotIx := got.Indexers[0]
	if gotIx.SearchIdentity != ix.SearchIdentity || gotIx.Headers["X-Api-Key"] != ix.Headers["X-Api-Key"] {
		t.Fatalf("indexer identity = %+v, want %+v", gotIx, ix)
	}
}
