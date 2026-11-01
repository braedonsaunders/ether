package main

import (
	"context"
	"fmt"
	"net/http"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// collidingClientSessions keeps two viewers playing concurrently with the
// same client identifier, then proves that stopping one cannot stop or expose
// the other viewer's session.
func collidingClientSessions(ctx context.Context, client *http.Client, base string, headers http.Header, vars map[string]map[string]string) ([]capture, error) {
	const profile = "web"
	ids := map[string]string{"owner": "parity-collision-owner", "adult": "parity-collision-adult"}
	var captures []capture
	fetch := func(viewer, label, path, method string, want int) (protocol.Response, error) {
		h := headers.Clone()
		h.Set("X-Plex-Client-Identifier", "parity-colliding-client")
		h.Set("X-Plex-Session-Identifier", ids[viewer])
		h.Set("Accept", "application/json")
		if label == "part" {
			h.Set("Range", "bytes=0-4095")
		}
		req := protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{profile + "-json": h}}
		r, err := strictFetch(ctx, client, base, "parity-"+viewer, req, profile, vars[viewer], true)
		if err != nil || r.Status != want || (want == http.StatusPartialContent && len(r.Body) != 4096) {
			return r, fmt.Errorf("collision %s %s: status=%d want=%d bytes=%d: %v", viewer, label, r.Status, want, len(r.Body), err)
		}
		captures = append(captures, capture{viewer, profile, "playback:collision:" + label, true, r})
		return r, nil
	}
	part := fmt.Sprintf("/library/parts/{{movieKey}}/%d/file.mp4", fixtureNow.Unix())
	timeline := "/:/timeline?ratingKey={{movieKey}}&duration=5400000&time=1200000&state="
	for _, viewer := range []string{"owner", "adult"} {
		if _, err := fetch(viewer, "part", part, http.MethodGet, http.StatusPartialContent); err != nil {
			return nil, err
		}
		if _, err := fetch(viewer, "playing", timeline+"playing", http.MethodPost, http.StatusOK); err != nil {
			return nil, err
		}
	}
	check := func(viewer, label string, owner, adult bool) error {
		r, err := fetch(viewer, label, "/status/sessions", http.MethodGet, http.StatusOK)
		if err != nil {
			return err
		}
		for v, want := range map[string]bool{"owner": owner, "adult": adult} {
			present, err := sessionListed(r.Body, ids[v])
			if err != nil || present != want {
				return fmt.Errorf("collision %s %s sees %s=%v, want %v: %v", viewer, label, v, present, want, err)
			}
		}
		return nil
	}
	if err := check("owner", "both-active", true, true); err != nil {
		return nil, err
	}
	if err := check("adult", "adult-isolated", false, true); err != nil {
		return nil, err
	}
	if _, err := fetch("owner", "owner-stopped", timeline+"stopped", http.MethodPost, http.StatusOK); err != nil {
		return nil, err
	}
	if err := check("owner", "adult-survives", false, true); err != nil {
		return nil, err
	}
	if _, err := fetch("adult", "adult-stopped", timeline+"stopped", http.MethodPost, http.StatusOK); err != nil {
		return nil, err
	}
	if err := check("owner", "both-stopped", false, false); err != nil {
		return nil, err
	}
	return captures, nil
}
