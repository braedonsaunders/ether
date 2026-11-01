package mediabrowser

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestEmbyUserUnsupportedPolicyOnCapabilityPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps frontend.ViewerCapabilitiesFunc
	}{
		{name: "absent"},
		{name: "error", caps: func(context.Context, *frontend.Viewer, frontend.Device, netip.Addr) (frontend.ViewerCapabilities, error) {
			return frontend.ViewerCapabilities{}, errors.New("unavailable")
		}},
		{name: "success", caps: func(context.Context, *frontend.Viewer, frontend.Device, netip.Addr) (frontend.ViewerCapabilities, error) {
			yes := true
			return frontend.ViewerCapabilities{CanPlay: &yes}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &instance{protocol: "emby", host: frontend.Host{ViewerCapabilities: tc.caps}}
			policy := i.userDTO(httptest.NewRequest("GET", "/", nil), &frontend.Viewer{ID: 1})["Policy"].(map[string]any)
			for _, key := range []string{"AllowCameraUpload", "AllowSharingPersonalItems", "EnableSubtitleDownloading"} {
				if policy[key] != false {
					t.Fatalf("%s: %#v", key, policy[key])
				}
			}
			if tc.name == "success" && policy["EnableMediaPlayback"] != true {
				t.Fatal("lost authoritative capability")
			}
			if policy["IntroSkipMode"] != nil {
				t.Fatal("policy absorbed configuration")
			}
			cfg := i.userDTO(httptest.NewRequest("GET", "/", nil), &frontend.Viewer{ID: 1, Username: "Ada"})
			if cfg["Prefix"] != "A" || cfg["Configuration"].(map[string]any)["IntroSkipMode"] != "ShowButton" || cfg["Policy"].(map[string]any)["LockedOutDate"] != 0 {
				t.Fatal(cfg)
			}
		})
	}
}
