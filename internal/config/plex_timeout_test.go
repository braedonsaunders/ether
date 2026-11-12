package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPlexFileCheckTimeout(t *testing.T) {
	for _, tc := range []struct {
		json string
		want time.Duration
	}{
		{`{}`, 8 * time.Second},
		{`{"fileCheckTimeout":"0s"}`, 8 * time.Second},
		{`{"fileCheckTimeout":"-1s"}`, 8 * time.Second},
		{`{"fileCheckTimeout":"15s"}`, 15 * time.Second},
	} {
		var p Plex
		if err := json.Unmarshal([]byte(tc.json), &p); err != nil {
			t.Fatal(err)
		}
		if got := p.FileCheckTimeoutOrDefault(); got != tc.want {
			t.Errorf("%s: timeout = %v, want %v", tc.json, got, tc.want)
		}
	}
	if got := Defaults(t.TempDir()).Plex.FileCheckTimeoutOrDefault(); got != 8*time.Second {
		t.Fatalf("new install timeout = %v", got)
	}
}
