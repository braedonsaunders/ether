package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestPlaybackInfoReportsEtherProducedSourcesAsLocal(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby"} {
		for _, tc := range []struct {
			name   string
			kind   core.DecisionKind
			live   bool
			remote bool
		}{{"direct", core.DecisionDirectPlay, false, true}, {"transcoded", core.DecisionTranscode, false, false}, {"live", core.DecisionDirectPlay, true, false}} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				viewer := &frontend.Viewer{ID: 1}
				host := frontend.Host{
					Identity:     frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
					Capabilities: frontendtest.Capabilities{},
					Experience: frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
						return frontend.ItemDetail{Markers: []frontend.Marker{{Type: "intro"}}}, nil
					}},
					Sessions: frontendtest.Sessions{PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
						return frontend.PlayPlan{ID: "plan", Decision: frontend.Decision{Kind: tc.kind}}, nil
					}},
					LiveTV: frontendtest.LiveTV{ResolveChannelFunc: func(context.Context, *core.Viewer, string) (frontend.LiveChannel, error) {
						return frontend.LiveChannel{ID: "channel"}, nil
					}},
				}
				inst, err := mount(context.Background(), host, "test", frontend.Protocol(protocol))
				if err != nil {
					t.Fatal(err)
				}
				itemID := ids.RatingKeyGUID(5)
				if tc.live {
					itemID = "channel"
				}
				r := httptest.NewRequest("POST", "/Items/"+itemID+"/PlaybackInfo", strings.NewReader(`{}`))
				r.Header.Set("X-Emby-Token", "grant")
				w := httptest.NewRecorder()
				inst.Handler().ServeHTTP(w, r)
				var body struct{ MediaSources []struct{ IsRemote bool } }
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.MediaSources) != 1 || body.MediaSources[0].IsRemote != tc.remote {
					t.Fatal(w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), `"HasSegments":true`) {
					t.Fatal("session markers were attributed to an unrelated source", w.Body.String())
				}
				if protocol == "jellyfin" && !strings.Contains(w.Body.String(), `"UseMostCompatibleTranscodingProfile":false`) {
					t.Fatal(w.Body.String())
				}
				if protocol == "emby" && strings.Contains(w.Body.String(), "UseMostCompatibleTranscodingProfile") {
					t.Fatal(w.Body.String())
				}
				if strings.Contains(w.Body.String(), `"VideoType"`) {
					t.Fatal("empty media claimed a video file", w.Body.String())
				}
				spec, err := openapi.CachedSpec("../../conform/specs/manifest.json", protocol, "")
				if os.IsNotExist(err) {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				doc, err := openapi.Parse(spec)
				if err != nil {
					t.Fatal(err)
				}
				if err = doc.Validate("POST", "/Items/"+itemID+"/PlaybackInfo", w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
