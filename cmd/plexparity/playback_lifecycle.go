package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// sessionListed checks the actual tracked playback ID; a 200 response alone
// does not prove that timeline events were bound to the intended session.
func sessionListed(body []byte, id string) (bool, error) {
	var data struct {
		MediaContainer struct {
			Metadata []struct {
				Session struct {
					ID string `json:"id"`
				} `json:"Session"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return false, err
	}
	for _, item := range data.MediaContainer.Metadata {
		if item.Session.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// seekAndReconnect reopens the actual media playlist, then restarts the same
// playback session at a nonzero offset and consumes a real segment.
func seekAndReconnect(ctx context.Context, client *http.Client, base, viewer, profile, mode, startPath, playlistPath string, headers http.Header, vars map[string]string) ([]capture, string, error) {
	h := headers.Clone()
	h.Set("Accept", "application/vnd.apple.mpegurl")
	var captures []capture
	fetch := func(label, path string) (protocol.Response, error) {
		req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{profile: h}}
		r, err := strictFetch(ctx, client, base, "parity-"+viewer, req, profile, vars, false)
		if err != nil || r.Status != http.StatusOK || len(r.Body) == 0 {
			return r, fmt.Errorf("%s %s %s %s: status=%d bytes=%d: %v", viewer, profile, mode, label, r.Status, len(r.Body), err)
		}
		captures = append(captures, capture{viewer, profile, "playback:" + label + ":" + mode, false, r})
		return r, nil
	}
	reconnect, err := fetch("reconnect-playlist", playlistPath)
	if err != nil || !bytes.HasPrefix(reconnect.Body, []byte("#EXTM3U")) {
		return nil, "", fmt.Errorf("reconnect playlist: %v", err)
	}
	master, err := fetch("seek-master", startPath+"&offset=3")
	if err != nil || !bytes.HasPrefix(master.Body, []byte("#EXTM3U")) {
		return nil, "", fmt.Errorf("seek master: %v", err)
	}
	firstURI := func(body []byte) string {
		for _, line := range strings.Split(string(body), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				return line
			}
		}
		return ""
	}
	uri := firstURI(master.Body)
	if uri == "" || strings.HasPrefix(uri, "/") || strings.Contains(uri, "://") {
		return nil, "", fmt.Errorf("invalid seek playlist URI %q", uri)
	}
	seekPlaylist := "/video/:/transcode/universal/" + uri
	media, err := fetch("seek-playlist", seekPlaylist)
	if err != nil || !bytes.HasPrefix(media.Body, []byte("#EXTM3U")) {
		return nil, "", fmt.Errorf("seek media playlist: %v", err)
	}
	segment := firstURI(media.Body)
	if segment == "" {
		return nil, "", fmt.Errorf("seek playlist has no media segment")
	}
	parent, err := url.Parse(seekPlaylist)
	if err != nil {
		return nil, "", err
	}
	child, err := url.Parse(segment)
	if err != nil {
		return nil, "", err
	}
	resolved := parent.ResolveReference(child)
	if resolved.IsAbs() || resolved.Host != "" {
		return nil, "", fmt.Errorf("external seek segment URL %q", segment)
	}
	if _, err = fetch("segment", resolved.String()); err != nil {
		return nil, "", err
	}
	return captures, resolved.String(), nil
}

// timelineLifecycle uses the same Plex client and playback session that
// started the encoder. It verifies the tracked session before and after stop,
// and requires the actual encoder resource to disappear after timeline stop.
func timelineLifecycle(ctx context.Context, client *http.Client, base, viewer, profile, mode, session, gonePath string, headers http.Header, vars map[string]string) ([]capture, error) {
	h := headers.Clone()
	h.Set("X-Plex-Session-Identifier", session)
	h.Set("Accept", "application/json")
	var captures []capture
	fetch := func(label, path string, want int) (protocol.Response, error) {
		req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{profile + "-json": h}}
		r, err := strictFetch(ctx, client, base, "parity-"+viewer, req, profile, vars, true)
		if err != nil || r.Status != want {
			return r, fmt.Errorf("%s %s %s %s: status=%d want=%d: %v", viewer, profile, mode, label, r.Status, want, err)
		}
		captures = append(captures, capture{viewer, profile, "playback:lifecycle:" + mode + ":" + label, true, r})
		return r, nil
	}
	baseTimeline := "/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&duration=5400000"
	if _, err := fetch("progress", baseTimeline+"&state=playing&time=1200000", http.StatusOK); err != nil {
		return nil, err
	}
	active, err := fetch("sessions-active", "/status/sessions", http.StatusOK)
	if err != nil {
		return nil, err
	}
	if ok, err := sessionListed(active.Body, session); err != nil || !ok {
		return nil, fmt.Errorf("%s %s %s active playback %q absent: %v", viewer, profile, mode, session, err)
	}
	if _, err = fetch("stopped", baseTimeline+"&state=stopped&time=1300000", http.StatusOK); err != nil {
		return nil, err
	}
	stopped, err := fetch("sessions-stopped", "/status/sessions", http.StatusOK)
	if err != nil {
		return nil, err
	}
	if ok, err := sessionListed(stopped.Body, session); err != nil || ok {
		return nil, fmt.Errorf("%s %s %s stopped playback %q remains: %v", viewer, profile, mode, session, err)
	}
	// gonePath is discovered from the live HLS playlist or a known DASH part,
	// not an invented session/resource. Keep its token out of the report label.
	if _, err = fetch("encoder-gone", gonePath, http.StatusNotFound); err != nil {
		return nil, err
	}
	return captures, nil
}
