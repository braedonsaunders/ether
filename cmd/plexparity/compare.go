package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

type capture struct {
	viewer, client, path string
	json                 bool
	response             protocol.Response
}
type transcript struct {
	ids      map[string]string
	captures []capture
}

// captureCorpus retains literal response bytes. There are no normalizations.
func captureCorpus(ctx context.Context, base string) (transcript, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	ids := protocol.Discover(ctx, client, base, "parity-owner", "web", nil)
	if len(ids.Warnings) > 0 {
		return transcript{}, fmt.Errorf("discovery incomplete: %s", strings.Join(ids.Warnings, "; "))
	}
	profiles := protocol.Clients()
	web := profiles["web"]
	for _, p := range []struct{ name, product, platform, device string }{{"androidtv", "Plex for Android", "Android", "Android TV"}, {"desktop", "Plex for Windows", "Windows", "Windows"}} {
		h := web.Headers.Clone()
		h.Set("X-Plex-Product", p.product)
		h.Set("X-Plex-Platform", p.platform)
		h.Set("X-Plex-Device", p.device)
		profiles[p.name] = protocol.Client{Name: p.product, Headers: h}
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	result := transcript{ids: ids.Values}
	viewerIDs := map[string]map[string]string{}
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		h := profiles["web"].Headers.Clone()
		h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
		h.Set("Accept", "application/xml")
		q := protocol.Request{Method: http.MethodGet, Path: "/playlists", Headers: map[string]http.Header{"web": h}}
		resp, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, q, "web", nil, false)
		if err != nil || resp.Status != http.StatusOK {
			return result, fmt.Errorf("%s playlist discovery: status=%d error=%v", viewer, resp.Status, err)
		}
		var listing struct {
			Playlists []struct {
				Key string `xml:"ratingKey,attr"`
			} `xml:"Playlist"`
		}
		if err = xml.Unmarshal(resp.Body, &listing); err != nil || len(listing.Playlists) == 0 || listing.Playlists[0].Key == "" {
			return result, fmt.Errorf("%s missing playlist fixture: %v", viewer, err)
		}
		vars := map[string]string{}
		for k, v := range ids.Values {
			vars[k] = v
		}
		vars["playlistKey"] = listing.Playlists[0].Key
		viewerIDs[viewer] = vars
	}
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			for _, jsonMode := range []bool{false, true} {
				for _, probe := range protocol.Corpus() {
					probe.Path = strings.ReplaceAll(probe.Path, "query=burbs", "query=fixture")
					h := profiles[name].Headers.Clone()
					h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
					if jsonMode {
						h.Set("Accept", "application/json")
					} else {
						h.Set("Accept", "application/xml")
					}
					probe.Headers = map[string]http.Header{name: h, name + "-json": h}
					response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, probe, name, viewerIDs[viewer], jsonMode)
					if err != nil {
						return result, fmt.Errorf("%s %s: %w", viewer, probe.Path, err)
					}
					if len(response.Body) >= 16<<20 {
						return result, fmt.Errorf("response at corpus size limit: %s %s", viewer, probe.Path)
					}
					result.captures = append(result.captures, capture{viewer, name, probe.Path, jsonMode, response})
				}
			}
		}
	}
	// Cover further browse routes after the original probes, preserving their
	// approved fixture signatures. Missing owner data is an invalid fixture.
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			for _, jsonMode := range []bool{false, true} {
				for _, path := range browsingPaths() {
					h := profiles[name].Headers.Clone()
					h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
					if jsonMode {
						h.Set("Accept", "application/json")
					} else {
						h.Set("Accept", "application/xml")
					}
					probe := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{name: h, name + "-json": h}}
					resp, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, probe, name, viewerIDs[viewer], jsonMode)
					if err != nil {
						return result, fmt.Errorf("browse %s %s: %w", viewer, path, err)
					}
					if viewer == "owner" && strings.HasPrefix(path, "/tv.plex.providers.epg.xmltv:1/grid?") && !bytes.Contains(resp.Body, []byte("Fixture Movie")) {
						return result, fmt.Errorf("live TV grid missing fixture programs: %s", resp.Body)
					}
					if viewer == "owner" && (resp.Status < 200 || resp.Status >= 300) {
						return result, fmt.Errorf("browse fixture %s returned %d", path, resp.Status)
					}
					if len(resp.Body) >= 16<<20 {
						return result, fmt.Errorf("response at corpus size limit: %s %s", viewer, path)
					}
					result.captures = append(result.captures, capture{viewer, name, path, jsonMode, resp})
				}
			}
		}
	}
	// Mutations are replayed only after the read-only probes, keeping the
	// initial response signatures independent of mutation ordering.
	sequence := []string{
		"/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&state=playing&time=1200000&duration=5400000",
		"/hubs/continueWatching", "/hubs",
		"/:/scrobble?identifier=com.plexapp.plugins.library&key={{episodeKey}}",
		"/library/onDeck", "/hubs",
		"/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&state=stopped&time=1300000&duration=5400000",
		"/hubs/continueWatching", "/hubs",
	}
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			for _, path := range sequence {
				if viewer == "teen" {
					path = strings.ReplaceAll(path, "{{episodeKey}}", "{{movieKey}}")
				}
				h := profiles[name].Headers.Clone()
				h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
				h.Set("Accept", "application/json")
				probe := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{name: h, name + "-json": h}}
				response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, probe, name, viewerIDs[viewer], true)
				if err != nil {
					return result, fmt.Errorf("mutation %s %s: %w", viewer, path, err)
				}
				if response.Status < 200 || response.Status >= 300 {
					return result, fmt.Errorf("mutation %s %s returned %d", viewer, path, response.Status)
				}
				if len(response.Body) >= 16<<20 {
					return result, fmt.Errorf("response at corpus size limit: %s %s", viewer, path)
				}
				result.captures = append(result.captures, capture{viewer, name, "sequence:" + path, true, response})
			}
		}
	}
	// Each start uses its own client/session and is explicitly stopped before
	// the next mode. Successful HLS manifests are required, not error pages.
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			id := "parity-" + viewer + "-" + name
			h := profiles[name].Headers.Clone()
			h.Set("X-Plex-Client-Identifier", id)
			h.Set("X-Plex-Session-Identifier", id)
			h.Set("Accept", "application/xml")
			directSession := id + "-direct"
			directDecisionPath := "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&protocol=http&session=" + directSession + "&directPlay=1&directStream=1"
			directDecisionReq := protocol.Request{Method: http.MethodGet, Path: directDecisionPath, Headers: map[string]http.Header{name: h}}
			directDecision, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, directDecisionReq, name, viewerIDs[viewer], false)
			if err != nil || directDecision.Status != http.StatusOK {
				return result, fmt.Errorf("%s %s direct decision status %d: %v", viewer, name, directDecision.Status, err)
			}
			if err = requireDecision(directDecision.Body, "direct"); err != nil {
				return result, fmt.Errorf("%s %s: %w: %s", viewer, name, err, directDecision.Body)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:" + directDecisionPath, false, directDecision})
			h.Set("Range", "bytes=0-4095")
			directPath := fmt.Sprintf("/library/parts/{{movieKey}}/%d/file.mp4", fixtureNow.Unix())
			directReq := protocol.Request{Method: http.MethodGet, Path: directPath, Headers: map[string]http.Header{name: h}}
			direct, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, directReq, name, viewerIDs[viewer], false)
			if err != nil || direct.Status != http.StatusPartialContent || len(direct.Body) != 4096 {
				return result, fmt.Errorf("%s %s direct playback failed: status=%d bytes=%d error=%v", viewer, name, direct.Status, len(direct.Body), err)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:" + directPath, false, direct})
			h.Del("Range")
			directStopPath := "/video/:/transcode/universal/stop?session=" + directSession
			directReq.Path = directStopPath
			stopped, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, directReq, name, viewerIDs[viewer], false)
			if err != nil || stopped.Status != http.StatusOK {
				return result, fmt.Errorf("%s %s direct stop: status=%d error=%v", viewer, name, stopped.Status, err)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:" + directStopPath, false, stopped})
			for _, mode := range []struct{ name, query string }{{"remux", "directPlay=0&directStream=1"}, {"transcode", "directPlay=0&directStream=0"}} {
				session := id + "-" + mode.name
				decisionPath := "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&protocol=hls&session=" + session + "&" + mode.query
				h.Set("Accept", "application/xml")
				decisionReq := protocol.Request{Method: http.MethodGet, Path: decisionPath, Headers: map[string]http.Header{name: h}}
				decision, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, decisionReq, name, viewerIDs[viewer], false)
				if err != nil || decision.Status != http.StatusOK {
					return result, fmt.Errorf("%s %s %s decision status %d: %v", viewer, name, mode.name, decision.Status, err)
				}
				if err = requireDecision(decision.Body, mode.name); err != nil {
					return result, fmt.Errorf("%s %s: %w: %s", viewer, name, err, decision.Body)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:" + decisionPath, false, decision})
				path := "/video/:/transcode/universal/start.m3u8?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&protocol=hls&session=" + session + "&" + mode.query
				h.Set("Accept", "application/vnd.apple.mpegurl")
				req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{name: h}}
				response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
				if err != nil || response.Status != http.StatusOK || !bytes.HasPrefix(response.Body, []byte("#EXTM3U")) {
					return result, fmt.Errorf("%s %s %s start failed: status=%d error=%v body=%q", viewer, name, mode.name, response.Status, err, response.Body)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:" + path, false, response})
				// Follow the server-generated stream grant; never invent an ID.
				var uri string
				for _, line := range strings.Split(string(response.Body), "\n") {
					if line != "" && !strings.HasPrefix(line, "#") {
						uri = line
						break
					}
				}
				if uri == "" {
					return result, fmt.Errorf("%s %s missing HLS media URI", viewer, name)
				}
				playlistPath := "/video/:/transcode/universal/" + uri
				req.Path = playlistPath
				playlist, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
				if err != nil || playlist.Status != http.StatusOK || !bytes.HasPrefix(playlist.Body, []byte("#EXTM3U")) {
					return result, fmt.Errorf("%s %s media playlist failed: %d %v", viewer, name, playlist.Status, err)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:media-playlist:" + mode.name, false, playlist})
				stop := "/video/:/transcode/universal/stop?session=" + session
				req.Path = stop
				response, err = protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
				if err != nil || response.Status != http.StatusOK {
					return result, fmt.Errorf("%s %s %s stop failed: status=%d error=%v", viewer, name, mode.name, response.Status, err)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:" + stop, false, response})
			}
		}
	}
	return result, nil
}
func diffTranscripts(a, b transcript, out io.Writer) (int, error) {
	if !reflect.DeepEqual(a.ids, b.ids) {
		return 0, fmt.Errorf("fixture discovery differs: baseline %v candidate %v", a.ids, b.ids)
	}
	if len(a.captures) != len(b.captures) {
		return 0, fmt.Errorf("capture counts differ: %d and %d", len(a.captures), len(b.captures))
	}
	n := 0
	for i, x := range a.captures {
		y := b.captures[i]
		if x.viewer != y.viewer || x.client != y.client || x.path != y.path || x.json != y.json {
			return n, fmt.Errorf("corpus order mismatch at %d", i)
		}
		if x.response.Status != y.response.Status || !bytes.Equal(normalizeResponse(x.response.Body), normalizeResponse(y.response.Body)) || !reflect.DeepEqual(normalizeHeaders(x.response.Headers), normalizeHeaders(y.response.Headers)) {
			n++
			commit, err := approved(x, y)
			if err != nil {
				return n, err
			}
			if commit != "" && !approvedHeaders(x, y) {
				commit = ""
			}
			label := "REGRESSION"
			if commit != "" {
				label = "APPROVED " + commit
			}
			fmt.Fprintf(out, "%s ", label)
			fmt.Fprintf(out, "viewer=%s client=%s json=%t route=%s\nbaseline status=%d headers=%v body=%q\ncandidate status=%d headers=%v body=%q\n", x.viewer, x.client, x.json, x.path, x.response.Status, normalizeHeaders(x.response.Headers), x.response.Body, y.response.Status, normalizeHeaders(y.response.Headers), y.response.Body)
		}
	}
	return n, nil
}
func compareCorpus(ctx context.Context, baseline, candidate string, out io.Writer) (int, error) {
	a, err := captureCorpus(ctx, baseline)
	if err != nil {
		return 0, fmt.Errorf("baseline: %w", err)
	}
	b, err := captureCorpus(ctx, candidate)
	if err != nil {
		return 0, fmt.Errorf("candidate: %w", err)
	}
	return diffTranscripts(a, b, out)
}
