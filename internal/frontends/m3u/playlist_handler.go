package m3u

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// playlist publishes channel-scoped media capabilities without exposing the login grant.
func (i *instance) playlist(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.SurfaceURL == nil || i.host.LazyStreams == nil || i.host.ClientIP == nil || i.host.Capabilities == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	base, err := i.host.SurfaceURL(r.Context(), r)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	u, err := url.Parse(base)
	if err != nil || !playlistURL(base) || u.RawQuery != "" || u.Fragment != "" {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	channels, err := i.host.LiveTV.Channels(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	sum := sha256.Sum256([]byte(credential))
	device := frontend.Device{ID: "m3u-" + hex.EncodeToString(sum[:16]), Client: "M3U", Product: "M3U"}
	if i.host.ClientSeen != nil {
		i.host.ClientSeen(device.ID)
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := frontend.WithClientIP(r.Context(), ip)
	profile, err := i.host.Capabilities.Build(ctx, frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: "m3u", ClientID: device.ID, Product: device.Product}, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	lineups, ok := i.host.LazyStreams.(frontend.LineupStreams)
	if !ok {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	path, err := lineups.IssueLineup(ctx, credential, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "m3u", Caps: profile.Client})
	if err != nil || !validLineupCapability(path) {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var entries []playlistEntry
	for _, channel := range channels {
		if channel.ID == "" {
			continue
		}
		channelURL := *u
		channelURL.Path = strings.TrimRight(u.Path, "/") + path + base64.RawURLEncoding.EncodeToString([]byte(channel.ID)) + "/live/index.m3u8"
		channelURL.RawPath = ""
		channelURL.RawQuery = ""
		entries = append(entries, playlistEntry{ID: channel.ID, Name: channel.Name, StreamURL: channelURL.String(), LogoURL: i.imageURL(r, credential, device, 0, channel.ID, channel.Images)})
	}
	var output bytes.Buffer
	if err := writePlaylist(&output, entries); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	_, _ = w.Write(output.Bytes())
}
