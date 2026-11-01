package mediabrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type hlsLazyStub struct {
	lazyStub
	hlsPath string
	err     error
	calls   int
	hlsSeen frontend.PlanRequest
	output  frontend.StreamRequest
}

func (s *hlsLazyStub) IssueHLS(_ context.Context, credential string, req frontend.PlanRequest, output frontend.StreamRequest) (string, error) {
	s.calls++
	s.credential, s.hlsSeen, s.output = credential, req, output
	return s.hlsPath, s.err
}

func TestLazyHLSSourceURLRejectsUnscopedPaths(t *testing.T) {
	for _, path := range []string{"/sessions/media/session/grant/master.m3u8", "https://foreign.invalid/master.m3u8", "/sessions/lazy/../hls/master.m3u8", "/sessions/lazy/a%2fb/hls/master.m3u8", "/sessions/lazy/grant/hls/master.m3u8?api_key=secret", "/sessions/lazy/grant/file", "/sessions/lazy/grant/hls/base/index.m3u8"} {
		stub := &hlsLazyStub{hlsPath: path}
		i := &instance{host: frontend.Host{LazyStreams: stub, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example", nil }}}
		if got := i.lazyHLSSourceURL(httptest.NewRequest("GET", "/", nil), "secret", frontend.PlanRequest{}); got != "" {
			t.Fatalf("accepted %q", got)
		}
	}
}

func TestPlaybackInfoAuthoritativeHLSContainer(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby"} {
		for _, tc := range []struct {
			name, container, want string
			eligible              bool
		}{
			{"mpegts", "mpegts", "ts", true}, {"fmp4", "fmp4", "mp4", true},
			{"unknown", "", "", true}, {"ineligible", "fmp4", "", false},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				sessions := frontendtest.Sessions{
					PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
						return frontend.PlayPlan{ID: "plan", Media: frontend.MediaInfo{Container: "mkv"}, Decision: frontend.Decision{Kind: "transcode"}, Transcoding: true, HLS: tc.eligible, HLSContainer: tc.container}, nil
					},
					OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
						return frontend.Session{ID: "session"}, nil
					},
					StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
						if r.Format != frontend.StreamHLS {
							t.Fatal(r)
						}
						return "/sessions/media/session/grant/master.m3u8", nil
					},
				}
				host := frontend.Host{Sessions: sessions, Capabilities: frontendtest.Capabilities{}, Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }}}
				var front frontend.Frontend = Jellyfin{MachineID: "test"}
				if protocol == "emby" {
					front = Emby{MachineID: "test"}
				}
				inst, err := front.Mount(context.Background(), host)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", "/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true}`))
				r.Header.Set("Authorization", `MediaBrowser Token="secret", DeviceId="device"`)
				w := httptest.NewRecorder()
				inst.Handler().ServeHTTP(w, r)
				var dto struct{ MediaSources []map[string]any }
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &dto) != nil || len(dto.MediaSources) != 1 {
					t.Fatal(w.Code, w.Body.String())
				}
				got, present := dto.MediaSources[0]["TranscodingContainer"]
				if tc.want == "" {
					if present {
						t.Fatal("invented container", got)
					}
				} else if got != tc.want {
					t.Fatal(got, tc.want)
				}
			})
		}
	}
}
