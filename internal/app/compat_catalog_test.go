package app

import (
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
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
	for _, want := range []string{
		"Plezy",
		"Swiftfin",
		"Infuse",
		"Streamyfin",
		"Findroid",
		"Kodi",
		"VLC",
		"Nuvio",
		"Plex Web",
		"Plex for Apple TV",
		"Plex for iOS",
		"Plex for Android phones",
		"Plex for Android TV",
		"Plex HTPC",
		"Plex for Windows",
		"Plex for Mac",
		"Plex for Linux",
		"Plex for Roku",
		"Plex for Kodi",
		"Jellyfin for Roku",
		"Emby Web",
		"Emby for Android TV",
		"Emby for TV",
		"Stremio",
		"DLNA renderer",
		"Files and media players",
	} {
		if !names[want] {
			t.Errorf("catalog missing %s", want)
		}
	}
	var streamyfin, embyTV, plezy, vlc, swiftfin compatApp
	for _, app := range apps {
		switch app.Name {
		case "Streamyfin":
			streamyfin = app
		case "Emby for TV":
			embyTV = app
		case "Plezy":
			plezy = app
		case "VLC":
			vlc = app
		case "Swiftfin":
			swiftfin = app
		}
	}
	if !slices.Equal(streamyfin.VideoCodecs, []string{"h264", "hevc", "vp8", "vp9", "av1"}) || streamyfin.Note != "no-dv5" {
		t.Fatalf("streamyfin: %+v", streamyfin)
	}
	if embyTV.Certification != "unsupported" {
		t.Fatalf("emby tv: %+v", embyTV)
	}
	if plezy.MaxHeight != 2160 || !plezy.HDR10 || !slices.Contains(plezy.VideoCodecs, "hevc") || !slices.Equal(plezy.DolbyVisionProfiles, []int{5, 7, 8}) {
		t.Fatalf("plezy built-in caps missing: %+v", plezy)
	}
	if vlc.MaxHeight != 2160 || !vlc.HDR10 || !slices.Contains(vlc.VideoCodecs, "av1") {
		t.Fatalf("vlc built-in caps missing: %+v", vlc)
	}
	if swiftfin.MaxHeight != 2160 || !swiftfin.HDR10 || !swiftfin.DolbyVision || !slices.Equal(swiftfin.DolbyVisionProfiles, []int{5, 8}) {
		t.Fatalf("swiftfin built-in caps missing: %+v", swiftfin)
	}
	for _, rule := range clientreg.ProductRules() {
		matched := false
		for _, app := range apps {
			for _, product := range app.products {
				if rule.MatchesProduct(product) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			t.Errorf("registry product match %+v has no Apps catalogue row", rule)
		}
	}
	for _, device := range builtinDevices(nil) {
		if device.Name == "Apple TV 4K" && !slices.Equal(device.DolbyVisionProfiles, []int{5, 8}) {
			t.Fatalf("apple tv device: %+v", device)
		}
	}
}

// Every catalogue row must exercise a real registry matcher, including owner
// overrides. This prevents display labels silently becoming generic clients.
func TestBuiltinDevicesMatchRuntimeRegistry(t *testing.T) {
	off := false
	overrides := []core.ClientCapabilityOverride{{ID: "shield", VideoCodecs: []string{"h264"}, MaxHeight: 720, HDR10: &off, DolbyVision: &off, DolbyVisionProfiles: []int{8}, SubtitleCodecs: []string{"srt"}}}
	for _, edits := range [][]core.ClientCapabilityOverride{nil, overrides} {
		rows := builtinDevices(edits)
		for i, device := range clientreg.Devices() {
			row := rows[i]
			id := device.Identity
			play := transcode.PlaybackDefaultsFor(id.Product, id.Platform, id.Device, id.DeviceName, id.Model, id.Version, edits)
			base := transcode.PlaybackDefaultsFor(id.Product, id.Platform, id.Device, id.DeviceName, id.Model, id.Version, nil)
			policy := transcode.ResolvePolicy(core.ClientCaps{Product: id.Product, Platform: id.Platform, Device: id.Device, Model: id.Model, Version: id.Version}, edits)
			if play.ID == "generic" {
				t.Errorf("%s unexpectedly uses generic playback", row.Name)
			}
			if row.Name != device.Name || row.MaxHeight != play.MaxHeight || row.HDR10 != play.HDR10 || row.DolbyVision != play.DolbyVision || !slices.Equal(row.VideoCodecs, play.Video) || !slices.Equal(row.DolbyVisionProfiles, policy.DolbyVisionProfiles) || !slices.Equal(row.SubtitleCodecs, policy.SubtitleCodecs) || row.Notes != policy.Notes || row.BuiltinMaxHeight != base.MaxHeight || row.BuiltinHDR10 != base.HDR10 || row.BuiltinDolbyVision != base.DolbyVision {
				t.Errorf("%s differs from runtime registry: %+v", device.Name, row)
			}
			if edits == nil && row.Name == "NVIDIA Shield" && (row.ID != "shield" || row.MaxHeight != 2160 || !row.HDR10 || slices.Contains(row.VideoCodecs, "av1") || !slices.Equal(row.DolbyVisionProfiles, []int{5, 8})) {
				t.Errorf("incorrect Shield baseline: %+v", row)
			}
		}
	}
}
