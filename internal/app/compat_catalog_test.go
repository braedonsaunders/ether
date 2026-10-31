package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// profileApps maps a conform profile's client field to the catalog app name.
// A new profile file fails this test until it is mapped.
var profileApps = map[string]string{
	"silo-appletv":                          "Silo for Apple TV",
	"Silo Apple":                            "Silo Apple",
	"Silo Android":                          "Silo Android",
	"Swiftfin":                              "Swiftfin",
	"Streamyfin":                            "Streamyfin",
	"Jellyfin Android TV":                   "Jellyfin Android TV",
	"JellyCon":                              "JellyCon",
	"Infuse (documented; capture required)": "Infuse",
	"infuse-jellyfin":                       "Infuse",
	"Findroid":                              "Findroid",
	"Emby.SDK v2 reference client":          "Emby SDK",
}

func TestBuiltinAppsCoverConformProfilesAndCertification(t *testing.T) {
	root := filepath.Join("..", "conform", "profiles")
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".json") || strings.Contains(path, "deviceprofiles") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var head struct {
			Client string `json:"client"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return err
		}
		if head.Client == "" {
			t.Fatalf("%s has no client", path)
		}
		if _, ok := profileApps[head.Client]; !ok {
			t.Errorf("unmapped conform client %q in %s", head.Client, path)
		}
		seen[head.Client] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	apps := builtinApps()
	names := map[string]bool{}
	for _, app := range apps {
		names[app.Name] = true
		if app.Protocol == "plex" && app.Name == "Apple TV" {
			t.Fatal("device labelled as a Plex app")
		}
	}
	for client, name := range profileApps {
		if !seen[client] {
			t.Errorf("mapping %q has no profile file", client)
		}
		if !names[name] {
			t.Errorf("catalog missing %q for profile client %q", name, client)
		}
	}
	for _, want := range []string{"Jellyfin for Roku", "Emby Web", "Emby for Android TV", "Emby for TV", "Stremio", "DLNA renderer", "Files and media players"} {
		if !names[want] {
			t.Errorf("catalog missing %s", want)
		}
	}
	var streamyfin, embyTV compatApp
	for _, app := range apps {
		if app.Name == "Streamyfin" {
			streamyfin = app
		}
		if app.Name == "Emby for TV" {
			embyTV = app
		}
	}
	if !slices.Equal(streamyfin.VideoCodecs, []string{"h264", "hevc", "vp8", "vp9", "av1"}) || streamyfin.Note != "no-dv5" {
		t.Fatalf("streamyfin: %+v", streamyfin)
	}
	if embyTV.Certification != "unsupported" {
		t.Fatalf("emby tv: %+v", embyTV)
	}
	for _, device := range builtinDevices() {
		if device.Name == "Apple TV" && !slices.Equal(device.DolbyVisionProfiles, []int{5, 8}) {
			t.Fatalf("apple tv device: %+v", device)
		}
	}
}
