package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
	"github.com/braedonsaunders/ether/internal/core"
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
	for _, key := range []string{"section", "movieSection", "showSection", "movieKey", "showKey", "seasonKey", "episodeKey", "collectionKey", "playlistKey"} {
		if ids.Values[key] == "" {
			return transcript{}, fmt.Errorf("discovery missing required %s", key)
		}
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
	// Populate Plex's asynchronous, persistent UltraBlur cache before recording
	// responses. Otherwise scheduling alone changes which rows contain colors.
	var warmPaths []string
	for i := int64(1); i <= 60; i++ {
		warmPaths = append(warmPaths, fmt.Sprintf("/library/metadata/%d", core.MovieKey(10000+i)))
	}
	for i := int64(1); i <= 20; i++ {
		warmPaths = append(warmPaths, fmt.Sprintf("/library/metadata/%d", core.ShowKey(20000+i)))
	}
	h := profiles["web"].Headers.Clone()
	h.Set("Accept", "application/json")
	h.Set("X-Plex-Client-Identifier", "parity-owner")
	for _, path := range warmPaths {
		req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{"web-json": h}}
		if _, err := protocol.Fetch(ctx, client, base, "parity-owner", req, "web", nil, true); err != nil {
			return result, fmt.Errorf("warm artwork %s: %w", path, err)
		}
	}
	for _, path := range warmPaths {
		req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{"web-json": h}}
		deadline := time.Now().Add(30 * time.Second)
		for {
			resp, err := protocol.Fetch(ctx, client, base, "parity-owner", req, "web", nil, true)
			if err != nil || resp.Status != http.StatusOK {
				return result, fmt.Errorf("warm artwork %s: status=%d error=%v", path, resp.Status, err)
			}
			if bytes.Contains(resp.Body, []byte(`"UltraBlurColors"`)) {
				break
			}
			if time.Now().After(deadline) {
				return result, fmt.Errorf("warm artwork %s: colors missing", path)
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
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
					if strings.HasPrefix(path, "/hubs/items") && (resp.Status != http.StatusOK || !bytes.Contains(resp.Body, []byte("Fixture Movie 020"))) {
						return result, fmt.Errorf("%s %s missing continue shelf: status=%d body=%s", viewer, path, resp.Status, resp.Body)
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
	if os.Getenv("ETHER_PARITY_ATTRIBUTION") != "1" {
		for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
			for _, name := range names {
				for _, path := range imagePaths() {
					h := profiles[name].Headers.Clone()
					h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
					req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{name: h}}
					resp, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
					if err != nil {
						return result, fmt.Errorf("image %s %s: %w", viewer, path, err)
					}
					if viewer == "owner" && (resp.Status != http.StatusOK || len(resp.Body) == 0 || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "image/")) {
						return result, fmt.Errorf("image fixture %s failed: status=%d content-type=%s bytes=%d", path, resp.Status, resp.Headers.Get("Content-Type"), len(resp.Body))
					}
					result.captures = append(result.captures, capture{viewer, name, "image:" + path, false, resp})
				}
			}
		}
	}
	if os.Getenv("ETHER_PARITY_ATTRIBUTION") != "1" {
		// UltraBlur is produced asynchronously. Poll until real colors exist, then
		// retain the complete metadata response for every viewer and client.
		for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
			for _, name := range names {
				for _, key := range []string{"movieKey", "showKey"} {
					if (viewer == "teen" || viewer == "child") && key == "showKey" {
						continue
					} // teen lacks the TV section; child cannot view this show
					h := profiles[name].Headers.Clone()
					h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
					h.Set("Accept", "application/json")
					path := "/library/metadata/{{" + key + "}}"
					req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{name + "-json": h}}
					deadline := time.Now().Add(15 * time.Second)
					for {
						resp, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], true)
						if err != nil {
							return result, err
						}
						if resp.Status != http.StatusOK {
							return result, fmt.Errorf("ultrablur %s %s returned %d", viewer, key, resp.Status)
						}
						if bytes.Contains(resp.Body, []byte(`"UltraBlurColors"`)) {
							result.captures = append(result.captures, capture{viewer, name, "ultrablur:" + path, true, resp})
							break
						}
						if time.Now().After(deadline) {
							return result, fmt.Errorf("ultrablur colors missing for %s %s", viewer, key)
						}
						select {
						case <-ctx.Done():
							return result, ctx.Err()
						case <-time.After(100 * time.Millisecond):
						}
					}
				}
			}
		}
	}
	// Mutations are replayed only after the read-only probes, keeping the
	// initial response signatures independent of mutation ordering.
	// Ether deliberately has no bundled resources; this registered wildcard
	// route always returns 404, so assert that contract rather than a fixture ID.
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			h := profiles[name].Headers.Clone()
			h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
			request := protocol.Request{Method: http.MethodGet, Path: "/:/resources/fixture", Headers: map[string]http.Header{name: h}}
			response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, request, name, nil, false)
			if err != nil || response.Status != http.StatusNotFound {
				return result, fmt.Errorf("resource route: status=%d error=%v", response.Status, err)
			}
			result.captures = append(result.captures, capture{viewer, name, request.Path, false, response})
		}
	}
	ownerHeaders := profiles["web"].Headers.Clone()
	ownerHeaders.Set("Accept", "application/xml")
	ownerHeaders.Set("X-Plex-Client-Identifier", "parity-owner")
	gridReq := protocol.Request{Method: http.MethodGet, Path: "/tv.plex.providers.epg.xmltv:1/grid?beginsAt=1792324800&endsAt=1792328400", Headers: map[string]http.Header{"web": ownerHeaders}}
	grid, err := protocol.Fetch(ctx, client, base, "parity-owner", gridReq, "web", nil, false)
	var guide struct {
		Videos []struct {
			Key string `xml:"ratingKey,attr"`
		} `xml:"Video"`
	}
	if err != nil || grid.Status != http.StatusOK || xml.Unmarshal(grid.Body, &guide) != nil || len(guide.Videos) == 0 || guide.Videos[0].Key == "" {
		return result, fmt.Errorf("live programme discovery: status=%d error=%v body=%s", grid.Status, err, grid.Body)
	}
	result.ids["programmeKey"] = guide.Videos[0].Key
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			h := profiles[name].Headers.Clone()
			h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
			h.Set("Accept", "application/xml")
			req := protocol.Request{Method: http.MethodGet, Path: "/tv.plex.providers.epg.xmltv:1/metadata/" + url.PathEscape(guide.Videos[0].Key), Headers: map[string]http.Header{name: h}}
			response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, nil, false)
			if err != nil || (viewer == "owner" && response.Status != http.StatusOK) {
				return result, fmt.Errorf("%s %s live programme: status=%d error=%v", viewer, name, response.Status, err)
			}
			result.captures = append(result.captures, capture{viewer, name, "livetv:programme", false, response})
		}
	}
	sequence := []string{
		"/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&state=playing&time=1200000&duration=5400000",
		"/hubs/continueWatching", "/library/onDeck", "/hubs",
		"/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&state=paused&time=1250000&duration=5400000",
		"/hubs/continueWatching", "/library/onDeck", "/hubs",
		"/:/scrobble?identifier=com.plexapp.plugins.library&key={{episodeKey}}",
		"/library/onDeck", "/hubs",
		"/:/timeline?ratingKey={{movieKey}}&key=%2Flibrary%2Fmetadata%2F{{movieKey}}&state=stopped&time=1300000&duration=5400000",
		"/hubs/continueWatching", "/library/onDeck", "/hubs",
		"/:/progress?key={{movieKey}}&state=paused&time=900000",
		"/hubs/continueWatching",
		"/:/rate?key={{movieKey}}&rating=8", "/library/metadata/{{movieKey}}",
		"/:/unscrobble?key={{movieKey}}", "/library/onDeck",
		"/actions/removeFromContinueWatching?key={{movieKey}}", "/hubs/continueWatching", "/hubs",
	}
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			for _, path := range sequence {
				if viewer == "teen" || viewer == "child" {
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
	// Create queues through Plex and use the returned ID; a fabricated queue
	// identifier would only exercise the not-found handler.
	for _, viewer := range []string{"owner", "adult", "teen", "child", "friend"} {
		for _, name := range names {
			h := profiles[name].Headers.Clone()
			h.Set("X-Plex-Client-Identifier", "parity-"+viewer)
			h.Set("Accept", "application/xml")
			path := "/playQueues?type=video&uri=" + url.QueryEscape("/library/metadata/") + "{{movieKey}}"
			req := protocol.Request{Method: http.MethodPost, Path: path, Headers: map[string]http.Header{name: h}}
			created, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
			var queue struct {
				ID string `xml:"playQueueID,attr"`
			}
			if err != nil || created.Status != http.StatusOK || xml.Unmarshal(created.Body, &queue) != nil || queue.ID == "" {
				return result, fmt.Errorf("%s %s play queue fixture missing: status=%d error=%v body=%s", viewer, name, created.Status, err, created.Body)
			}
			result.captures = append(result.captures, capture{viewer, name, "queue:create", false, created})
			for _, jsonMode := range []bool{false, true} {
				if jsonMode {
					h.Set("Accept", "application/json")
				} else {
					h.Set("Accept", "application/xml")
				}
				req.Method, req.Path, req.Headers = http.MethodGet, "/playQueues/"+queue.ID, map[string]http.Header{name: h, name + "-json": h}
				response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, nil, jsonMode)
				if err != nil || response.Status != http.StatusOK {
					return result, fmt.Errorf("%s %s play queue read: status=%d error=%v", viewer, name, response.Status, err)
				}
				result.captures = append(result.captures, capture{viewer, name, "queue:read", jsonMode, response})
			}
		}
	}
	// Each start uses its own client/session and is explicitly stopped before
	// the next mode. Successful HLS manifests are required, not error pages.
	if os.Getenv("ETHER_PARITY_ATTRIBUTION") != "1" {
		h := profiles["web"].Headers.Clone()
		h.Set("X-Plex-Client-Identifier", "parity-owner-debrid")
		h.Set("X-Plex-Session-Identifier", "parity-owner-debrid")
		h.Set("Accept", "application/xml")
		key := fmt.Sprint(core.MovieKey(10002))
		meta := protocol.Request{Method: http.MethodGet, Path: "/library/metadata/" + key, Headers: map[string]http.Header{"web": h}}
		response, err := protocol.Fetch(ctx, client, base, "parity-owner", meta, "web", nil, false)
		if err != nil || response.Status != http.StatusOK || !bytes.Contains(response.Body, []byte("Fixture Movie 002")) {
			return result, fmt.Errorf("debrid fixture metadata missing: status=%d error=%v", response.Status, err)
		}
		path := "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F" + key + "&hasMDE=1&protocol=hls&session=parity-owner-debrid&directPlay=1&directStream=1"
		request := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{"web": h}}
		response, err = protocol.Fetch(ctx, client, base, "parity-owner", request, "web", nil, false)
		if err != nil || response.Status != http.StatusOK || requireDecision(response.Body, "direct") != nil {
			return result, fmt.Errorf("debrid HTTP direct play decision failed: status=%d error=%v body=%s", response.Status, err, response.Body)
		}
		result.captures = append(result.captures, capture{"owner", "web", "playback:debrid-http:" + path, false, response})
		h.Set("Range", "bytes=0-4095")
		request.Path = fmt.Sprintf("/library/parts/%s/%d/file.mp4", key, fixtureNow.Unix())
		response, err = protocol.Fetch(ctx, client, base, "parity-owner", request, "web", nil, false)
		if err != nil || response.Status != http.StatusPartialContent || len(response.Body) == 0 {
			return result, fmt.Errorf("debrid HTTP direct start failed: status=%d error=%v", response.Status, err)
		}
		result.captures = append(result.captures, capture{"owner", "web", "playback:debrid-http:bytes", false, response})
		h.Del("Range")
		request.Path = "/video/:/transcode/universal/stop?session=parity-owner-debrid"
		response, err = protocol.Fetch(ctx, client, base, "parity-owner", request, "web", nil, false)
		if err != nil || response.Status != http.StatusOK {
			return result, fmt.Errorf("debrid HTTP stop failed: status=%d error=%v", response.Status, err)
		}
		result.captures = append(result.captures, capture{"owner", "web", "playback:debrid-http:stop", false, response})
	}
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
			directReq.Path = "/library/parts/{{movieKey}}/file.mp4"
			shortPart, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, directReq, name, viewerIDs[viewer], false)
			if err != nil || shortPart.Status != http.StatusPartialContent || len(shortPart.Body) != 4096 {
				return result, fmt.Errorf("%s %s short direct part failed: status=%d bytes=%d error=%v", viewer, name, shortPart.Status, len(shortPart.Body), err)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:short-part", false, shortPart})
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
				segmentURI := ""
				for _, line := range strings.Split(string(playlist.Body), "\n") {
					if line != "" && !strings.HasPrefix(line, "#") {
						segmentURI = line
						break
					}
				}
				if segmentURI == "" {
					return result, fmt.Errorf("%s %s %s playlist has no segment", viewer, name, mode.name)
				}
				playlistURL, err := url.Parse(playlistPath)
				if err != nil {
					return result, err
				}
				segmentURL, err := url.Parse(segmentURI)
				if err != nil {
					return result, err
				}
				resolved := playlistURL.ResolveReference(segmentURL)
				if resolved.IsAbs() || resolved.Host != "" {
					return result, fmt.Errorf("external HLS segment URL %q", segmentURI)
				}
				req.Path = resolved.String()
				segment, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
				if err != nil || segment.Status != http.StatusOK || len(segment.Body) == 0 {
					return result, fmt.Errorf("%s %s %s segment failed: status=%d bytes=%d error=%v", viewer, name, mode.name, segment.Status, len(segment.Body), err)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:segment:" + mode.name, false, segment})
				ping := "/video/:/transcode/universal/ping?session=" + session
				req.Path = ping
				response, err = protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
				if err != nil || response.Status != http.StatusOK {
					return result, fmt.Errorf("%s %s %s ping failed: status=%d error=%v", viewer, name, mode.name, response.Status, err)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:" + ping, false, response})
				stop := "/video/:/transcode/universal/stop?session=" + session
				req.Path = stop
				response, err = protocol.Fetch(ctx, client, base, "parity-"+viewer, req, name, viewerIDs[viewer], false)
				if err != nil || response.Status != http.StatusOK {
					return result, fmt.Errorf("%s %s %s stop failed: status=%d error=%v", viewer, name, mode.name, response.Status, err)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:" + stop, false, response})
			}
			dashSession := id + "-dash"
			dashDecisionPath := "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&protocol=dash&session=" + dashSession + "&directPlay=0&directStream=1"
			h.Set("Accept", "application/xml")
			dashDecisionReq := protocol.Request{Method: http.MethodGet, Path: dashDecisionPath, Headers: map[string]http.Header{name: h}}
			dashDecision, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, dashDecisionReq, name, viewerIDs[viewer], false)
			if err != nil || dashDecision.Status != http.StatusOK || requireDecision(dashDecision.Body, "remux") != nil {
				return result, fmt.Errorf("%s %s DASH decision: status=%d error=%v body=%s", viewer, name, dashDecision.Status, err, dashDecision.Body)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:dash:decision", false, dashDecision})
			dashPath := "/video/:/transcode/universal/start.mpd?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&protocol=dash&session=" + dashSession + "&directPlay=0&directStream=1"
			h.Set("Accept", "application/dash+xml")
			dashReq := protocol.Request{Method: http.MethodGet, Path: dashPath, Headers: map[string]http.Header{name: h}}
			mpd, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, dashReq, name, viewerIDs[viewer], false)
			if err != nil || mpd.Status != http.StatusOK || !bytes.Contains(mpd.Body, []byte("<MPD")) {
				return result, fmt.Errorf("%s %s DASH start: status=%d error=%v body=%s", viewer, name, mpd.Status, err, mpd.Body)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:dash:manifest", false, mpd})
			for _, part := range []string{"header", "0.m4s"} {
				dashReq.Path = "/video/:/transcode/universal/session/" + dashSession + "/0/" + part
				response, err := protocol.Fetch(ctx, client, base, "parity-"+viewer, dashReq, name, viewerIDs[viewer], false)
				if err != nil || response.Status != http.StatusOK || len(response.Body) == 0 {
					return result, fmt.Errorf("%s %s DASH %s: status=%d error=%v", viewer, name, part, response.Status, err)
				}
				result.captures = append(result.captures, capture{viewer, name, "playback:dash:" + part, false, response})
			}
			dashReq.Path = "/video/:/transcode/universal/stop?session=" + dashSession
			stopped, err = protocol.Fetch(ctx, client, base, "parity-"+viewer, dashReq, name, viewerIDs[viewer], false)
			if err != nil || stopped.Status != http.StatusOK {
				return result, fmt.Errorf("%s %s DASH stop: status=%d error=%v", viewer, name, stopped.Status, err)
			}
			result.captures = append(result.captures, capture{viewer, name, "playback:dash:stop", false, stopped})
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
		if x.response.Status != y.response.Status || !bytes.Equal(normalizeCaptureBody(x), normalizeCaptureBody(y)) || !reflect.DeepEqual(normalizeCaptureHeaders(x), normalizeCaptureHeaders(y)) {
			noise, err := baselineNondeterministic(x, y)
			if err != nil {
				return n, err
			}
			commit, err := approved(x, y)
			if err != nil {
				return n, err
			}
			if commit != "" && !approvedHeaders(x, y) {
				commit = ""
			}
			label := "REGRESSION"
			if noise {
				label = "BASELINE-NONDETERMINISTIC (also on main)"
			} else if commit != "" {
				label = "APPROVED " + commit
			} else {
				n++
			}
			fmt.Fprintf(out, "%s ", label)
			fmt.Fprintf(out, "viewer=%s client=%s json=%t route=%s\nbaseline status=%d headers=%v body=%q\ncandidate status=%d headers=%v body=%q\n", x.viewer, x.client, x.json, x.path, x.response.Status, normalizeCaptureHeaders(x), x.response.Body, y.response.Status, normalizeCaptureHeaders(y), y.response.Body)
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
