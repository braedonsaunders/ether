package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func TestSurfaceCapabilitiesApplyQuirksAndOwnerRules(t *testing.T) {
	a := setupTestApp(t, true)
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Quality = config.Quality{Profiles: []core.QualityProfile{{Name: "standard", MaxHeight: 2160}, {Name: "jelly", MaxHeight: 720}, {Name: "remote", MaxHeight: 480}}, Default: "standard", DefaultAway: "remote", Rules: []config.QualityRule{{ID: "r_jf", Devices: []config.DeviceMatch{{App: "jellyfin:Swiftfin"}}, Profile: "jelly"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	provider := surfaceCapabilities{app: a, surface: "jellyfin"}
	request := frontend.CapabilityRequest{Viewer: &core.Viewer{ID: 1}, Device: caps.DeviceInfo{Protocol: "plex", Product: "Swiftfin", Platform: "tvOS", VideoCodecs: []string{"h264"}, DolbyVision: true}, Profile: caps.DeviceProfile{DirectPlayProfiles: []caps.DirectPlayProfile{{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac"}}}}
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
	request.Device.Platform = ""
	if _, err := provider.Build(lan, request); err != nil {
		t.Fatal(err)
	}
	a.familyWrites.Wait()
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
		if payload.Devices[i].Name == "Apple TV 4K" {
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
	if !slices.Contains(got.Client.AudioCodecs, "mp3") || !slices.Contains(got.Client.AudioCodecs, "ac3") || got.Client.MaxHeight != 2160 || slices.Contains(got.Client.VideoCodecs, "av1") {
		t.Fatalf("%+v", got.Client)
	}
	infuse, err := provider.Build(lan, frontend.CapabilityRequest{
		Viewer: &core.Viewer{ID: 1},
		Device: caps.DeviceInfo{Product: "Infuse-Direct", DeviceName: "AppleTV", Version: "8.5.9"},
		Profile: caps.DeviceProfile{
			TranscodingProfiles: []caps.TranscodingProfile{{Container: "mp4", Type: "Video", VideoCodec: "hevc,h264,av1", AudioCodec: "eac3,ac3,aac", Protocol: "hls"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(infuse.Client.VideoCodecs, "av1") || !infuse.Client.SupportsVideo("hevc") || !infuse.Client.SupportsVideo("h264") || !slices.Equal(infuse.Client.DVProfiles, []int{5, 8}) {
		t.Fatalf("Infuse-Direct on AppleTV should not direct-play AV1 without explicit DirectPlayProfile: %+v", infuse.Client)
	}
	advertised, err := provider.Build(lan, frontend.CapabilityRequest{
		Viewer:  &core.Viewer{ID: 1},
		Device:  caps.DeviceInfo{Product: "Infuse-Direct", DeviceName: "AppleTV", Version: "8.5.9"},
		Profile: caps.DeviceProfile{DirectPlayProfiles: []caps.DirectPlayProfile{{Container: "mkv,mp4", VideoCodec: "h264,hevc,av1", AudioCodec: "eac3,ac3,aac"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !advertised.Client.SupportsVideo("av1") {
		t.Fatalf("explicitly advertised AV1 lost: %+v", advertised.Client)
	}
	limited, err := provider.Build(lan, frontend.CapabilityRequest{Viewer: &core.Viewer{ID: 1}, Device: caps.DeviceInfo{Product: "Swiftfin tvOS"}, Profile: caps.DeviceProfile{DirectPlayProfiles: []caps.DirectPlayProfile{{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(limited.Client.AudioCodecs, "ac3") || !slices.Contains(limited.Client.AudioCodecs, "aac") {
		t.Fatalf("profile audio overwritten: %v", limited.Client.AudioCodecs)
	}
}

func TestSurfaceCapabilitiesAlwaysEncodeVideoWithoutForce(t *testing.T) {
	a := setupTestApp(t, true)
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Clients.Overrides = []core.ClientCapabilityOverride{
			{Product: "Swiftfin", ProductExact: true, AlwaysEncodeVideo: true},
			{Product: "MysteryTVApp", ProductExact: true, AlwaysEncodeVideo: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lan := frontend.WithClientIP(context.Background(), netip.MustParseAddr("192.168.1.2"))
	provider := surfaceCapabilities{app: a, surface: "jellyfin"}

	// Known app with declared DeviceProfile gets AlwaysEncodeVideo without Force.
	swiftfin, err := provider.Build(lan, frontend.CapabilityRequest{
		Viewer:  &core.Viewer{ID: 1},
		Device:  caps.DeviceInfo{Product: "Swiftfin", Platform: "tvOS"},
		Profile: caps.DeviceProfile{DirectPlayProfiles: []caps.DirectPlayProfile{{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"}}},
	})
	if err != nil || !swiftfin.Client.AlwaysEncodeVideo {
		t.Fatalf("known app AlwaysEncodeVideo missing: %+v %v", swiftfin.Client, err)
	}

	// Unknown/generic app also gets AlwaysEncodeVideo despite matching the generic preset.
	custom, err := provider.Build(lan, frontend.CapabilityRequest{
		Viewer: &core.Viewer{ID: 1},
		Device: caps.DeviceInfo{Product: "MysteryTVApp"},
	})
	if err != nil || !custom.Client.AlwaysEncodeVideo {
		t.Fatalf("generic app AlwaysEncodeVideo missing: %+v %v", custom.Client, err)
	}

	// Non-matching app remains unaffected.
	other, err := provider.Build(lan, frontend.CapabilityRequest{
		Viewer: &core.Viewer{ID: 1},
		Device: caps.DeviceInfo{Product: "VLC"},
	})
	if err != nil || other.Client.AlwaysEncodeVideo {
		t.Fatalf("non-matching app should not get AlwaysEncodeVideo: %+v %v", other.Client, err)
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

func TestSurfaceCapabilitiesOverrideDeviceMatchingAcrossProtocols(t *testing.T) {
	a := setupTestApp(t, true)
	on := true
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Clients.Overrides = []core.ClientCapabilityOverride{
			{Product: "Swiftfin", Device: "Bedroom Apple TV", Force: true, MaxHeight: 1080, DolbyVision: &on, DolbyVisionProfiles: []int{5, 7, 8}},
			{Product: "Silo", Platform: "tvOS", Device: "Living Room", Force: true, MaxHeight: 1080, HDR10: &on},
			{Product: "Vela", Platform: "iOS", Device: "Den iPad", Force: true, MaxHeight: 720},
			{Product: "Stremio", Platform: "Desktop", Force: true, MaxHeight: 1080},
			{Product: "CustomDual", Device: "Friendly Label", Force: true, MaxHeight: 720},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lan := frontend.WithClientIP(context.Background(), netip.MustParseAddr("192.168.1.2"))
	for _, tc := range []struct {
		surface core.Protocol
		dev     caps.DeviceInfo
		wantH   int
	}{
		{"jellyfin", caps.DeviceInfo{Product: "Swiftfin", DeviceName: "Bedroom Apple TV"}, 1080},
		{"emby", caps.DeviceInfo{Product: "Swiftfin", DeviceName: "Bedroom Apple TV"}, 1080},
		{"silo", caps.DeviceInfo{Product: "Silo", Platform: "tvOS", DeviceName: "Living Room"}, 1080},
		{"vela", caps.DeviceInfo{Product: "Vela", Platform: "iOS", DeviceName: "Den iPad"}, 720},
		{"stremio", caps.DeviceInfo{Product: "Stremio", Platform: "Desktop"}, 1080},
		{"jellyfin", caps.DeviceInfo{Product: "CustomDual", Device: "Hardware Model", DeviceName: "Friendly Label"}, 720},
	} {
		got, err := (surfaceCapabilities{app: a, surface: tc.surface}).Build(lan, frontend.CapabilityRequest{
			Viewer: &core.Viewer{ID: 1},
			Device: tc.dev,
		})
		if err != nil {
			t.Fatalf("%s %+v: %v", tc.surface, tc.dev, err)
		}
		if got.Client.MaxHeight != tc.wantH {
			t.Fatalf("%s %+v: MaxHeight = %d, want %d", tc.surface, tc.dev, got.Client.MaxHeight, tc.wantH)
		}
	}
}

func TestSourceIPDecidesLocalityAcrossAllFrontendsAndUploadBudget(t *testing.T) {
	restore := clientnet.OverrideHostPrefixes([]netip.Prefix{
		netip.MustParsePrefix("198.18.10.0/24"),
		netip.MustParsePrefix("2001:db8:abcd:1::/64"),
	})
	t.Cleanup(restore)

	a := setupTestApp(t, true)
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Server.TrustedProxyCIDRs = []string{"172.16.50.5"}
		c.Quality = config.Quality{
			Profiles:    []core.QualityProfile{{Name: "home", MaxHeight: 2160}, {Name: "away", MaxHeight: 720}},
			Default:     "home",
			DefaultAway: "away",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	surfaces := []core.Protocol{"jellyfin", "emby", "silo", "vela", "stremio", "xtream"}
	cases := []struct {
		name        string
		peer        string
		xff         string
		claimRemote bool
		wantRemote  bool
		wantProfile string
	}{
		{"private-ipv4-behind-proxy-claims-remote", "172.16.50.5:41000", "192.168.3.10", true, false, "home"},
		{"host-ipv4-subnet-claims-remote", "172.16.50.5:41001", "198.18.10.42", true, false, "home"},
		{"host-ipv6-slash64-claims-remote", "172.16.50.5:41002", "2001:db8:abcd:1::42", true, false, "home"},
		{"cgnat-claims-remote", "172.16.50.5:41003", "100.64.1.5", true, false, "home"},
		{"public-ipv4-claims-local", "172.16.50.5:41004", "203.0.113.10", false, true, "away"},
		{"public-ipv6-outside-host-claims-local", "172.16.50.5:41005", "2001:db8:ffff:1::42", false, true, "away"},
	}

	for _, surface := range surfaces {
		provider := surfaceCapabilities{app: a, surface: surface}
		for _, tc := range cases {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			ip, ok := a.frontendClientIP(r)
			if !ok {
				t.Fatalf("%s/%s: failed to resolve client IP", surface, tc.name)
			}
			got, err := provider.Build(frontend.WithClientIP(context.Background(), ip), frontend.CapabilityRequest{
				Viewer: &core.Viewer{ID: 1},
				Device: caps.DeviceInfo{Product: "Test Client", Remote: tc.claimRemote},
			})
			if err != nil {
				t.Fatalf("%s/%s: %v", surface, tc.name, err)
			}
			if got.Client.Remote != tc.wantRemote || got.Quality.Name != tc.wantProfile {
				t.Fatalf("%s/%s: remote=%v profile=%q, want remote=%v profile=%q", surface, tc.name, got.Client.Remote, got.Quality.Name, tc.wantRemote, tc.wantProfile)
			}
		}
	}

	// Upload budget remote metering must exclude RFC1918, CGNAT, host IPv4
	// subnets, and host IPv6 /64 prefixes, and meter only public remote IPs.
	a.buildUploadBudget()
	if a.uploadBudget == nil {
		t.Fatal("upload budget not initialized")
	}
	for _, localAddr := range []string{
		"192.168.3.10:51000",
		"100.64.1.5:51000",
		"198.18.10.42:51000",
		"[2001:db8:abcd:1::42]:51000",
	} {
		a.observeTranscodeRemoteDelivery(transcode.RemoteDeliveryEvent{
			SessionID:        "local-" + localAddr,
			RemoteAddr:       localAddr,
			Bytes:            4 << 20,
			Duration:         5 * time.Second,
			SegDuration:      4 * time.Second,
			MediaBitrateKbps: 8000,
			TCPBacklogBytes:  128 << 10,
		})
	}
	if snap := a.uploadBudget.Status(); snap.MeasuredSamples != 0 {
		t.Fatalf("local delivery fed upload budget estimator: samples=%d", snap.MeasuredSamples)
	}
	a.observeTranscodeRemoteDelivery(transcode.RemoteDeliveryEvent{
		SessionID:        "remote-public",
		RemoteAddr:       "203.0.113.10:51000",
		Bytes:            4 << 20,
		Duration:         5 * time.Second,
		SegDuration:      4 * time.Second,
		MediaBitrateKbps: 8000,
		TCPBacklogBytes:  128 << 10,
	})
	if snap := a.uploadBudget.Status(); snap.MeasuredSamples != 1 {
		t.Fatalf("public remote delivery did not feed upload budget estimator: samples=%d", snap.MeasuredSamples)
	}
}
