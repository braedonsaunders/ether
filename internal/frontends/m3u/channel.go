package m3u

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// channel is stable in cached playlists. Each tune verifies the surface grant
// and channel policy before issuing a short-lived broker-owned media capability.
func (i *instance) channel(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.URL.Query().Get("id")
	channels, err := i.host.LiveTV.Channels(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	found := false
	for _, c := range channels {
		if id != "" && c.ID == id {
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if i.host.LazyStreams == nil || i.host.ClientIP == nil || i.host.Capabilities == nil || i.host.SurfaceURL == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	sum := sha256.Sum256([]byte(credential))
	device := frontend.Device{ID: "m3u-" + hex.EncodeToString(sum[:16]), Client: "M3U", Product: "M3U"}
	ctx := frontend.WithClientIP(r.Context(), ip)
	profile, err := i.host.Capabilities.Build(ctx, frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: "m3u", ClientID: device.ID, Product: device.Product}, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	base, err := i.host.SurfaceURL(ctx, r)
	if err != nil || !playlistURL(base) {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	u, err := url.Parse(base)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	path, err := i.host.LazyStreams.Issue(ctx, credential, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "m3u", Channel: id, Caps: profile.Client})
	if err != nil || !validChannelCapability(path) {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if i.host.ClientSeen != nil {
		i.host.ClientSeen(device.ID)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func validChannelCapability(path string) bool {
	rest, ok := strings.CutPrefix(path, "/sessions/lazy/")
	if !ok {
		return false
	}
	token, ok := strings.CutSuffix(rest, "/live/index.m3u8")
	if !ok || token == "" || len(token) > 128 {
		return false
	}
	for _, r := range token {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (i *instance) serveLazy(w http.ResponseWriter, r *http.Request) {
	if i.host.LazyStreams == nil {
		http.NotFound(w, r)
		return
	}
	i.host.LazyStreams.ServeHTTP(w, r)
}
