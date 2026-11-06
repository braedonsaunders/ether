package config

import (
	"encoding/json"
	"testing"
)

func TestPlexServersSwitchUpgradeAndRoundTrip(t *testing.T) {
	for _, test := range []struct {
		raw     string
		enabled bool
	}{
		{`{}`, true},
		{`{"plexServers":{"enabled":false}}`, false},
		{`{"plexServers":{"enabled":true}}`, true},
	} {
		cfg, _, err := decodeOverDefaults(Defaults(t.TempDir()), []byte(test.raw))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PlexServers.Enabled != test.enabled {
			t.Fatalf("%s: enabled=%v", test.raw, cfg.PlexServers.Enabled)
		}
		encoded, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		round, _, err := decodeOverDefaults(Defaults(t.TempDir()), encoded)
		if err != nil || round.PlexServers != cfg.PlexServers {
			t.Fatalf("round trip: %v", err)
		}
	}
}
