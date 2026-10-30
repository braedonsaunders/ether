package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A saved list must reload exactly: decoding a file's array over the
// default slice must not let an element inherit default fields the file
// omits (omitempty zero values).
func TestOpenDoesNotLeakDefaultSliceElements(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *Config) error {
		c.Quality.Rules = []QualityRule{{Platform: "iOS", Profile: "720p"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rules := s2.Get().Quality.Rules
	if len(rules) != 1 || rules[0] != (QualityRule{Platform: "iOS", Profile: "720p"}) {
		t.Fatalf("reloaded rules = %+v, want only the iOS rule", rules)
	}
}

// Sections and keys the file lacks keep their defaults; a null slice
// clears it, as before.
func TestOpenKeepsDefaultsForMissingKeys(t *testing.T) {
	dir := t.TempDir()
	doc := `{"server":{"port":1234,"machineIdentifier":"m"},"quality":{"rules":null},"cache":{"budgetBytes":123456789012}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	d := Defaults(dir)
	if c.Server.Port != 1234 || c.Server.MachineIdentifier != "m" || c.Server.FriendlyName != d.Server.FriendlyName ||
		c.Server.MoviesLibraryName != d.Server.MoviesLibraryName || c.Server.TVShowsLibraryName != d.Server.TVShowsLibraryName {
		t.Fatalf("server = %+v", c.Server)
	}
	if c.Quality.Rules != nil || len(c.Quality.Profiles) != len(d.Quality.Profiles) || c.Quality.Default != d.Quality.Default {
		t.Fatalf("quality = %+v", c.Quality)
	}
	if c.Cache.BudgetBytes != 123456789012 || c.Cache.Dir != d.Cache.Dir {
		t.Fatalf("cache = %+v", c.Cache)
	}
	if c.Catalog.Language != d.Catalog.Language || c.Transcode.MaxTranscodes != d.Transcode.MaxTranscodes {
		t.Fatalf("missing sections lost their defaults: %+v %+v", c.Catalog, c.Transcode)
	}
}

func TestReadOpensSealedSettingsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(c *Config) error { c.Plex.Token = "secret-token"; return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	before, _ := os.ReadFile(path)
	c, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Plex.Token != "secret-token" {
		t.Fatalf("token = %q", c.Plex.Token)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("Read rewrote config.json")
	}
	if _, err := Read(t.TempDir()); err == nil {
		t.Fatal("missing config read without error")
	}
}

// Like json.Unmarshal, Open rejects data after the config document rather
// than loading the first value and later rewriting the file without it.
func TestOpenRejectsTrailingData(t *testing.T) {
	for _, doc := range []string{`{"server":{"port":1234}} {"server":{"port":5678}}`, `{"server":{"port":1234}} x`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Errorf("Open(%q) succeeded", doc)
		}
	}
}
