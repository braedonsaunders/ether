package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An install still on the v0.3.1 quality defaults (1080p at home and away,
// Apple TV rule, 8 Mbps remote cap) moves to the uncapped defaults on open
// and saves them; an owner's own choices are kept.
func TestOpenUpgradesShippedQualityDefaults(t *testing.T) {
	old := `{"quality":{"default":"1080p","remote":"1080p","rules":[{"product":"Apple TV","profile":"4K"}],` +
		`"profiles":[{"name":"4K","maxHeight":2160,"minHeight":1080},{"name":"1080p","maxHeight":1080,"minHeight":720}]},` +
		`"transcode":{"remoteMaxBitrateKbps":8000}}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Get()
	q := cfg.Quality
	if q.Default != "4K" || q.Remote != "" || len(q.Rules) != 0 || q.Profiles[0].MinHeight != 720 || cfg.Transcode.RemoteMaxBitrateKbps != 0 {
		t.Fatalf("not upgraded: default=%q remote=%q rules=%v 4K min=%d remote kbps=%d", q.Default, q.Remote, q.Rules, q.Profiles[0].MinHeight, cfg.Transcode.RemoteMaxBitrateKbps)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil || !strings.Contains(string(saved), `"remote": ""`) && !strings.Contains(string(saved), `"remote":""`) {
		t.Fatalf("upgrade not saved: %v", err)
	}

	for name, owned := range map[string]string{
		"remote profile": `{"quality":{"default":"1080p","remote":"720p","rules":[{"product":"Apple TV","profile":"4K"}],"profiles":[{"name":"4K","maxHeight":2160,"minHeight":1080},{"name":"720p","maxHeight":720}]}}`,
		"extra rule":     `{"quality":{"default":"1080p","remote":"1080p","rules":[{"product":"Apple TV","profile":"4K"},{"platform":"Roku","profile":"4K"}],"profiles":[{"name":"4K","maxHeight":2160,"minHeight":1080},{"name":"1080p","maxHeight":1080}]}}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(owned), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if q := s.Get().Quality; q.Default != "1080p" || q.Remote == "" {
			t.Errorf("%s: owner's choice changed: default=%q remote=%q", name, q.Default, q.Remote)
		}
	}
}
