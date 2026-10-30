package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type embeddedSessions interface { frontend.Sessions }

type detailPlannerStub struct {
	embeddedSessions
	calls   int
	failure bool
}

func (s *detailPlannerStub) DetailPlan(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	s.calls++
	if req.Item != 5 || req.Device.ID != "device" || req.Intent.Estimate {
		return frontend.PlayPlan{}, errors.New("wrong scope")
	}
	if s.failure {
		return frontend.PlayPlan{}, context.DeadlineExceeded
	}
	return frontend.PlayPlan{Media: frontend.MediaInfo{Container: "mp4", Size: 113973614, Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264"}, {Type: "audio", Codec: "aac"}}}, DirectPlay: true, DirectStream: true, Transcoding: true}, nil
}

func TestResolvedSingleDetailUsesBrokerFactsAndScopedPath(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	experience := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "episode"}}, nil
	}}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		planner := &detailPlannerStub{embeddedSessions: frontendtest.Sessions{}}
		lazy := &lazyStub{path: "/sessions/lazy/scoped/file"}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: experience, Sessions: planner, Capabilities: frontendtest.Capabilities{}, LazyStreams: lazy, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example", nil }})
		if err != nil {
			t.Fatal(err)
		}
		call := func(path string) *httptest.ResponseRecorder {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			return w
		}
		w := call("/Items/" + ids.RatingKeyGUID(5))
		for _, want := range []string{`"Container":"mp4"`, `"Size":113973614`, `"Codec":"h264"`, `"VideoType":"VideoFile"`, `"SupportsDirectPlay":true`, `"Path":"https://ether.example/sessions/lazy/scoped/file"`} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
				t.Fatal(tc.name, want, w.Code, w.Body.String())
			}
		}
		if strings.Contains(w.Body.String(), "account-secret") || lazy.seen.Item != 5 || lazy.seen.Version != "" {
			t.Fatal("unsafe scope", lazy.seen, w.Body.String())
		}
		calls := planner.calls
		w = call("/Items?Recursive=true")
		if w.Code != 200 || planner.calls != calls {
			t.Fatal("listing resolved a source", tc.name, w.Code, planner.calls)
		}
		planner.failure = true
		w = call("/Items/" + ids.RatingKeyGUID(5))
		if w.Code != 200 || strings.Contains(w.Body.String(), `"Container":"mp4"`) || !strings.Contains(w.Body.String(), `"SupportsDirectPlay":false`) {
			t.Fatal("timeout did not preserve generic source", tc.name, w.Code, w.Body.String())
		}
	}
}
