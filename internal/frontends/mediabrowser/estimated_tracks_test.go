package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestItemDetailOmitsEstimatedTracks(t *testing.T) {
	for _, protocol := range []frontend.Protocol{"jellyfin", "emby"} {
		for _, estimated := range []bool{true, false} {
			t.Run(string(protocol)+"/"+map[bool]string{true: "estimated", false: "reported"}[estimated], func(t *testing.T) {
				viewer := &frontend.Viewer{ID: 1}
				host := frontend.Host{
					Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
					Experience: frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
						return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}, Tracks: []frontend.StreamInfo{{Type: "video", Codec: "h264"}}, TracksEstimated: estimated}, nil
					}},
				}
				inst, err := mount(context.Background(), host, "test", protocol)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
				r.Header.Set("X-Emby-Token", "grant")
				w := httptest.NewRecorder()
				inst.Handler().ServeHTTP(w, r)
				var item map[string]any
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &item) != nil {
					t.Fatal(w.Code, w.Body.String())
				}
				_, present := item["MediaStreams"]
				if present == estimated {
					t.Fatalf("estimated=%v: %s", estimated, w.Body.String())
				}
			})
		}
	}
}
