package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// infuseDirectResume covers a direct media-part request without a preceding
// transcode decision, followed by Infuse-style POST timeline updates. Each
// probe uses an isolated fixture reset, never a real account or server.
func infuseDirectResume(ctx context.Context, client *http.Client, base string, headers http.Header, vars map[string]string) ([]capture, error) {
	localVars := make(map[string]string, len(vars))
	for k, v := range vars {
		localVars[k] = v
	}
	h := headers.Clone()
	h.Set("X-Plex-Product", "Infuse")
	h.Set("X-Plex-Platform", "tvOS")
	h.Set("X-Plex-Client-Identifier", "parity-infuse-direct")
	h.Del("X-Plex-Session-Identifier")
	h.Set("Accept", "application/json")
	var captures []capture
	fetch := func(method, label, path string, want int) (protocol.Response, error) {
		req := protocol.Request{Method: method, Path: path, Headers: map[string]http.Header{"appletv-json": h}}
		r, err := strictFetch(ctx, client, base, "parity-owner", req, "appletv", localVars, true)
		if err != nil || r.Status != want || (want == http.StatusPartialContent && len(r.Body) != 4096) {
			return r, fmt.Errorf("infuse direct %s: status=%d want=%d: %v", label, r.Status, want, err)
		}
		captures = append(captures, capture{"owner", "appletv", "playback:infuse-resume:" + label, true, r})
		return r, nil
	}
	// The ordinary corpus deliberately dismisses movieKey from Continue
	// Watching. Discover a different real fixture title so that sticky dismissal
	// cannot mask whether this probe's progress appears in the hub.
	listing, err := fetch(http.MethodGet, "discover", "/library/sections/{{movieSection}}/all?title=Fixture%20Movie%20004&X-Plex-Container-Size=100", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var titles struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
				Title     string `json:"title"`
				Type      string `json:"type"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(listing.Body, &titles); err != nil {
		return nil, err
	}
	key := ""
	for _, title := range titles.MediaContainer.Metadata {
		if title.Title == "Fixture Movie 004" && title.Type == "movie" {
			if key != "" || title.RatingKey == "" {
				return nil, fmt.Errorf("infuse fixture title is ambiguous or lacks an ID")
			}
			key = title.RatingKey
		}
	}
	if key == "" || key == vars["movieKey"] {
		return nil, fmt.Errorf("infuse fixture needs a distinct, discovered movie")
	}
	localVars["movieKey"] = key
	for _, millis := range []int{30000, 61000, 120000, 201274} {
		label := fmt.Sprint(millis)
		if _, err := fetch(http.MethodGet, label+":reset", "/:/unscrobble?key={{movieKey}}", http.StatusOK); err != nil {
			return nil, err
		}
		h.Set("Range", "bytes=0-4095")
		part := fmt.Sprintf("/library/parts/{{movieKey}}/%d/file.mp4", fixtureNow.Unix())
		if _, err := fetch(http.MethodGet, label+":part", part, http.StatusPartialContent); err != nil {
			return nil, err
		}
		h.Del("Range")
		for _, state := range []string{"playing", "stopped"} {
			path := fmt.Sprintf("/:/timeline?ratingKey={{movieKey}}&key=%%2Flibrary%%2Fmetadata%%2F{{movieKey}}&state=%s&time=%d&duration=5820000", state, millis)
			if _, err := fetch(http.MethodPost, label+":"+state, path, http.StatusOK); err != nil {
				return nil, err
			}
		}
		metadata, err := fetch(http.MethodGet, label+":metadata", "/library/metadata/{{movieKey}}", http.StatusOK)
		if err != nil {
			return nil, err
		}
		var item struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey  string `json:"ratingKey"`
					ViewOffset int64  `json:"viewOffset"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(metadata.Body, &item); err != nil || len(item.MediaContainer.Metadata) != 1 || item.MediaContainer.Metadata[0].RatingKey != key {
			return nil, fmt.Errorf("infuse %s metadata missing discovered movie: %v", label, err)
		}
		if os.Getenv("ETHER_PARITY_EXPECT_EARLY_RESUME") == "1" {
			want := int64(0)
			if millis >= 60000 {
				want = int64(millis)
			}
			if got := item.MediaContainer.Metadata[0].ViewOffset; got != want {
				return nil, fmt.Errorf("infuse %s resume offset=%d, want %d", label, got, want)
			}
		}
		hub, err := fetch(http.MethodGet, label+":continue", "/hubs/continueWatching", http.StatusOK)
		if err != nil {
			return nil, err
		}
		var contents struct {
			MediaContainer struct {
				Hub []struct {
					Metadata []struct {
						RatingKey string `json:"ratingKey"`
					} `json:"Metadata"`
				} `json:"Hub"`
			} `json:"MediaContainer"`
		}
		if err := json.Unmarshal(hub.Body, &contents); err != nil {
			return nil, err
		}
		found := false
		for _, row := range contents.MediaContainer.Hub {
			for _, entry := range row.Metadata {
				found = found || entry.RatingKey == key
			}
		}
		if want := item.MediaContainer.Metadata[0].ViewOffset > 0; found != want {
			return nil, fmt.Errorf("infuse %s movie in Continue Watching=%v but metadata resumable=%v", label, found, want)
		}
	}
	return captures, nil
}
