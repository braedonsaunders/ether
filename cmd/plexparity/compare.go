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
		if x.response.Status != y.response.Status || !bytes.Equal(x.response.Body, y.response.Body) || x.response.Headers.Get("Content-Type") != y.response.Headers.Get("Content-Type") {
			n++
			ok, err := approved(x, y)
			if err != nil {
				return n, err
			}
			label := "REGRESSION"
			if ok {
				label = "APPROVED c82a7830"
			}
			fmt.Fprintf(out, "%s ", label)
			fmt.Fprintf(out, "viewer=%s client=%s json=%t route=%s\nbaseline status=%d content-type=%q body=%q\ncandidate status=%d content-type=%q body=%q\n", x.viewer, x.client, x.json, x.path, x.response.Status, x.response.Headers.Get("Content-Type"), x.response.Body, y.response.Status, y.response.Headers.Get("Content-Type"), y.response.Body)
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
