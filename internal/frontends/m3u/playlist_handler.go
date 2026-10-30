package m3u

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// playlist publishes stable surface-authenticated channel URLs. Each request to
// such a URL must revalidate the grant and current policy before starting a
// broker-scoped stream, so clients can safely cache the playlist.
func (i *instance) playlist(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.SurfaceURL == nil {
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
	var entries []playlistEntry
	for _, channel := range channels {
		if channel.ID == "" {
			continue
		}
		channelURL := *u
		channelURL.Path = strings.TrimRight(u.Path, "/") + "/" + credential + "/channel"
		channelURL.RawPath = ""
		channelURL.RawQuery = url.Values{"id": []string{channel.ID}}.Encode()
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
