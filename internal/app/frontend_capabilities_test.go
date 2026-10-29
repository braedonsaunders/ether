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
	var payload struct {
		Policies []protocolClientPolicy `json:"policies"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	found := map[frontend.Protocol]bool{}
	webFamily := false
	for _, row := range payload.Policies {
		if row.Family == "Apple TV" {
			found[row.Protocol] = true
		}
		if row.Protocol == "jellyfin" && row.Family == "Jellyfin Chrome" {
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
