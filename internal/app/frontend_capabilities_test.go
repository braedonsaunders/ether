package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

func TestSurfaceCapabilitiesApplyQuirksAndOwnerRules(t *testing.T) {
	a := setupTestApp(t, true)
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Quality = config.Quality{Profiles: []core.QualityProfile{{Name: "standard", MaxHeight: 2160}, {Name: "jelly", MaxHeight: 720}, {Name: "remote", MaxHeight: 480}}, Default: "standard", Remote: "remote", Rules: []config.QualityRule{{Protocol: "jellyfin", Profile: "jelly"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	provider := surfaceCapabilities{app: a, surface: "jellyfin"}
	request := frontend.CapabilityRequest{Viewer: &core.Viewer{ID: 1}, Device: caps.DeviceInfo{Protocol: "plex", Product: "Jellyfin Apple TV", VideoCodecs: []string{"h264"}, DolbyVision: true}, Profile: caps.DeviceProfile{DirectPlayProfiles: []caps.DirectPlayProfile{{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac"}}}}
	if _, err := provider.Build(context.Background(), request); err == nil {
		t.Fatal("missing verified peer accepted")
	}
	lan := frontend.WithClientIP(context.Background(), netip.MustParseAddr("192.168.1.2"))
	remote := frontend.WithClientIP(context.Background(), netip.MustParseAddr("198.51.100.7"))
	request.Device.Remote = true
	got, err := provider.Build(lan, request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Client.Protocol != "jellyfin" || !got.Client.SupportsVideo("hevc") || !slices.Contains(got.Client.DVProfiles, 5) || !slices.Contains(got.Client.SubtitleCodecs, "srt") || got.Quality.Name != "jelly" || got.Quality.MaxHeight != 720 {
		t.Fatalf("%+v", got)
	}
	request.Device.SubtitleCodecs = []string{}
	request.Device.DVProfiles = []int{8}
	got, err = provider.Build(lan, request)
	if err != nil || got.Client.SubtitleCodecs == nil || len(got.Client.SubtitleCodecs) != 0 || !slices.Equal(got.Client.DVProfiles, []int{8}) {
		t.Fatalf("explicit profile lost: %+v %v", got, err)
	}
	other := provider
	other.surface = "emby"
	got, err = other.Build(lan, request)
	if err != nil || got.Quality.Name != "standard" {
		t.Fatal(got.Quality, err)
	}
	request.Device.Remote = false
	got, err = other.Build(remote, request)
	if err != nil || !got.Client.Remote || got.Quality.Name != "remote" {
		t.Fatal(got.Quality, err)
	}
	request.Device.Product = "Jellyfin Chrome"
	if _, err := provider.Build(lan, request); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	a.playbackPolicyHandler().ServeHTTP(w, httptest.NewRequest("GET", "/policy", nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var payload compatCatalog
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !catalogHas(payload.Apps, "jellyfin", "Streamyfin") || !catalogHas(payload.Apps, "emby", "Emby for TV") || !catalogHas(payload.Apps, "silo", "Silo Android") || !catalogHas(payload.Apps, "dlna", "DLNA renderer") || !catalogHas(payload.Apps, "webdav", "Files and media players") {
		t.Fatalf("builtin apps missing: %+v", payload.Apps)
	}
	appleTVApp := false
	for _, app := range payload.Apps {
		if app.Name == "Apple TV" && app.Protocol == "plex" {
			appleTVApp = true
		}
	}
	if appleTVApp {
		t.Fatal("Apple TV is a device, not a Plex app")
	}
	var apple *compatDevice
	for i := range payload.Devices {
		if payload.Devices[i].Name == "Apple TV" {
			apple = &payload.Devices[i]
		}
	}
	if apple == nil || !slices.Equal(apple.DolbyVisionProfiles, []int{5, 8}) {
		t.Fatalf("Apple TV device: %+v", apple)
	}
	found := map[string]bool{}
	webFamily := false
	for _, row := range payload.Observed {
		if row.Name == "Apple TV" {
			found[row.Protocol] = true
		}
		if row.Protocol == "jellyfin" && row.Name == "Jellyfin Chrome" && !row.LastSeen.IsZero() {
			webFamily = true
		}
	}
	if !found["jellyfin"] || !found["emby"] {
		t.Fatal("reported client families absent", found)
	}
	if !webFamily {
		t.Fatal("non-Plex browser labelled as Plex Web")
	}
}

func TestSwiftfinWithoutProfileUsesAppleCodecs(t *testing.T) {
	a := setupTestApp(t, true)
	provider := surfaceCapabilities{app: a, surface: "jellyfin"}
	lan := frontend.WithClientIP(context.Background(), netip.MustParseAddr("192.168.1.2"))
	got, err := provider.Build(lan, frontend.CapabilityRequest{Viewer: &core.Viewer{ID: 1}, Device: caps.DeviceInfo{Product: "Swiftfin tvOS"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Client.AudioCodecs, "mp3") || !slices.Contains(got.Client.AudioCodecs, "ac3") || got.Client.MaxHeight != 2160 {
		t.Fatalf("%+v", got.Client)
	}
	limited, err := provider.Build(lan, frontend.CapabilityRequest{Viewer: &core.Viewer{ID: 1}, Device: caps.DeviceInfo{Product: "Swiftfin tvOS"}, Profile: caps.DeviceProfile{DirectPlayProfiles: []caps.DirectPlayProfile{{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(limited.Client.AudioCodecs, "ac3") || !slices.Contains(limited.Client.AudioCodecs, "aac") {
		t.Fatalf("profile audio overwritten: %v", limited.Client.AudioCodecs)
	}
}

func catalogHas(apps []compatApp, protocol, name string) bool {
	for _, app := range apps {
		if app.Protocol == protocol && app.Name == name {
			return true
		}
	}
	return false
}
