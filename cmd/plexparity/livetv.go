package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

var liveSessionKey = regexp.MustCompile(`/livetv/sessions/([0-9a-fA-F-]{36})`)

// liveTVCapture tunes the actual seeded virtual channel, follows its returned
// session and subscription identifiers, and compares the resulting media.
func liveTVCapture(ctx context.Context, client *http.Client, base, viewer, name string, profile http.Header) ([]capture, error) {
	h := profile.Clone()
	h.Set("Accept", "application/json")
	h.Set("X-Plex-Client-Identifier", "parity-"+viewer+"-"+name+"-livetv")
	fetch := func(method, path string) (protocol.Response, error) {
		return strictFetch(ctx, client, base, "parity-"+viewer, protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{name: h}}, name, nil, false)
	}
	tune, err := fetch(http.MethodPost, "/livetv/dvrs/1/channels/fixture-channel/tune")
	if err != nil {
		return nil, err
	}
	if tune.Status != http.StatusOK {
		if (viewer == "teen" || viewer == "child") && (tune.Status == http.StatusNotFound || tune.Status == http.StatusForbidden) {
			return []capture{{viewer, name, "livetv:tune", true, tune}}, nil
		}
		return nil, fmt.Errorf("live tune: status=%d body=%s", tune.Status, tune.Body)
	}
	match := liveSessionKey.FindSubmatch(tune.Body)
	if len(match) != 2 {
		return nil, fmt.Errorf("live tune has no session key: %s", tune.Body)
	}
	id := string(match[1])
	var envelope struct {
		MediaContainer struct {
			Subscriptions []struct {
				Key string `json:"key"`
			} `json:"MediaSubscription"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(tune.Body, &envelope); err != nil || len(envelope.MediaContainer.Subscriptions) != 1 || envelope.MediaContainer.Subscriptions[0].Key == "" {
		return nil, fmt.Errorf("live tune has no subscription: %s (%v)", tune.Body, err)
	}
	subscription := envelope.MediaContainer.Subscriptions[0].Key
	activity := tune.Headers.Get("X-Plex-Activity")
	if activity == "" {
		return nil, fmt.Errorf("live tune has no activity")
	}
	// These are per-session random identities, not item, route, or media data.
	// Preserve the ordinary web activity prefix and the exact original lengths.
	stable := func(r protocol.Response, binary bool) protocol.Response {
		replace := func(s string) string {
			if activity != id && !strings.Contains(activity, id) {
				s = strings.ReplaceAll(s, activity, strings.Repeat("A", len(activity)))
			}
			return strings.ReplaceAll(s, id, strings.Repeat("S", len(id)))
		}
		if !binary {
			r.Body = []byte(replace(string(r.Body)))
		}
		r.Headers = r.Headers.Clone()
		for k, values := range r.Headers {
			for i, v := range values {
				values[i] = replace(v)
			}
			r.Headers[k] = values
		}
		return r
	}
	out := []capture{{viewer, name, "livetv:tune", true, stable(tune, false)}}
	root := "/livetv/sessions/" + id
	for _, item := range []struct{ label, path string }{
		{"session", root}, {"subscription", "/media/subscriptions/" + url.PathEscape(subscription)}, {"operation", "/media/grabbers/operations/" + id},
	} {
		r, e := fetch(http.MethodGet, item.path)
		if e != nil || r.Status != http.StatusOK {
			return nil, fmt.Errorf("live %s: status=%d error=%v body=%s", item.label, r.Status, e, r.Body)
		}
		out = append(out, capture{viewer, name, "livetv:" + item.label, true, stable(r, false)})
	}
	playlist, e := fetch(http.MethodGet, root+"/0/index.m3u8")
	if e != nil || playlist.Status != http.StatusOK || !strings.HasPrefix(string(playlist.Body), "#EXTM3U") {
		return nil, fmt.Errorf("live playlist: status=%d error=%v body=%s", playlist.Status, e, playlist.Body)
	}
	out = append(out, capture{viewer, name, "livetv:playlist", false, stable(playlist, false)})
	var segment string
	for _, line := range strings.Split(string(playlist.Body), "\n") {
		if strings.HasPrefix(line, "program/") {
			segment = line
			break
		}
	}
	if segment == "" {
		return nil, fmt.Errorf("live playlist has no programme segment: %s", playlist.Body)
	}
	r, e := fetch(http.MethodGet, root+"/0/"+segment)
	if e != nil || r.Status != http.StatusOK || len(r.Body) == 0 {
		return nil, fmt.Errorf("live programme segment: status=%d error=%v", r.Status, e)
	}
	out = append(out, capture{viewer, name, "livetv:program-segment", false, stable(r, true)})
	stopped, e := fetch(http.MethodDelete, root)
	if e != nil || stopped.Status != http.StatusOK {
		return nil, fmt.Errorf("live stop: status=%d error=%v", stopped.Status, e)
	}
	out = append(out, capture{viewer, name, "livetv:stop", true, stable(stopped, false)})
	return out, nil
}
