package mediabrowser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

func TestSourceDerivedDeviceProfiles(t *testing.T) {
	files, err := filepath.Glob("../profiles/jellyfin/deviceprofiles/*.json")
	if err != nil || len(files) == 0 {
		t.Fatal("no source-derived profiles", err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var profile caps.DeviceProfile
			// Explicit tags preserve the metadata wire spelling.
			var meta struct {
				Client        string          `json:"client"`
				Source        string          `json:"source"`
				Commit        string          `json:"source_commit"`
				Configuration string          `json:"configuration"`
				Profile       json.RawMessage `json:"device_profile"`
			}
			if err = json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.Client == "" || meta.Source == "" || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(meta.Commit) || meta.Configuration == "" || !strings.Contains(meta.Source, meta.Commit) {
				t.Fatal("missing provenance")
			}
			if err = json.Unmarshal(meta.Profile, &profile); err != nil {
				t.Fatal(err)
			}
			if string(meta.Profile) == "null" || len(meta.Profile) == 0 {
				t.Fatal("missing device profile")
			}
		})
	}
}
