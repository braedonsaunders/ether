package config

import "testing"

func TestProtectedValuesFollowsSecretSemantics(t *testing.T) {
	var cfg Config
	cfg.Server.FriendlyName = "LivingRoomName"
	cfg.Usenet.Providers = []UsenetProvider{{
		Username: "usenet-user-seed", Password: "usenet-pass-seed", Host: "news.example",
	}}
	cfg.Posters.URLTemplate = "https://img.example/{type}/{tmdb_id}?k=template-seed"
	cfg.Plex.Token = "plex-token-seed"
	cfg.Database.URL = "postgres://dbuser:dbpass-seed@db.example/ether"
	cfg.OpenSubtitles.Username = "os-user-seed"
	cfg.OpenSubtitles.Password = "os-pass-seed"
	cfg.Indexers = []Indexer{{
		APIKey: "ix-key-seed", URL: "https://idx.example",
		SecretSettings: map[string]string{"passkey": "passkey-seed"},
	}}
	got := ProtectedValues(cfg)
	has := func(seed string) bool {
		for _, v := range got {
			if v == seed {
				return true
			}
		}
		return false
	}
	seeds := []struct{ field, value string }{
		{"usenet username", "usenet-user-seed"},
		{"usenet password", "usenet-pass-seed"},
		{"posters url template", "https://img.example/{type}/{tmdb_id}?k=template-seed"},
		{"plex token", "plex-token-seed"},
		{"database url", "postgres://dbuser:dbpass-seed@db.example/ether"},
		{"opensubtitles username", "os-user-seed"},
		{"opensubtitles password", "os-pass-seed"},
		{"indexer api key", "ix-key-seed"},
		{"indexer secret setting", "passkey-seed"},
	}
	for _, seed := range seeds {
		if !has(seed.value) {
			t.Errorf("protected field missing: %s", seed.field)
		}
	}
	if has("LivingRoomName") || has("news.example") || has("https://idx.example") {
		t.Fatal("a non-secret setting was collected")
	}
}
