package mediabrowser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type hlsDetailPlanner struct {
	*detailPlannerStub
	plan frontend.PlayPlan
	seen frontend.PlanRequest
}

func (p *hlsDetailPlanner) DetailPlan(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
	p.seen = req
	return p.plan, nil
}

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

func TestResolvedDetailLazyHLSOffers(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby"} {
		for _, tc := range []struct {
			name                                                                  string
			direct, remux, encode, hls, estimated, missingRevision, missingIssuer bool
			issueErr                                                              error
		}{
			{name: "eligible", direct: true, remux: true, encode: true, hls: true},
			{name: "hls-only", encode: true, hls: true},
			{name: "direct-only", direct: true},
			{name: "dash-only", direct: true, encode: true},
			{name: "no-eligible-hls", remux: true, encode: true},
			{name: "authorization-failure", direct: true, encode: true, hls: true, issueErr: errors.New("unauthorized")},
			{name: "missing-issuer", direct: true, encode: true, hls: true, missingIssuer: true},
			{name: "estimated", encode: true, hls: true, estimated: true},
			{name: "missing-revision", encode: true, hls: true, missingRevision: true},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				viewer := &frontend.Viewer{ID: 1}
				planner := &hlsDetailPlanner{detailPlannerStub: &detailPlannerStub{embeddedSessions: frontendtest.Sessions{}}, plan: frontend.PlayPlan{
					DetailSource: "82b0c3fc-7616-46da-8528-fc26ec5cb4ab", ETag: "source-revision", HLS: tc.hls,
					DirectPlay: tc.direct, DirectStream: tc.remux, Transcoding: tc.encode, Estimated: tc.estimated,
					Media: frontend.MediaInfo{Container: "mp4", Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264"}}},
				}}
				if tc.missingRevision {
					planner.plan.ETag = ""
				}
				lazy := &hlsLazyStub{lazyStub: lazyStub{path: "/sessions/lazy/direct/file"}, hlsPath: "/sessions/lazy/scoped/hls/master.m3u8", err: tc.issueErr}
				var lazyService frontend.LazyStreams = lazy
				if tc.missingIssuer {
					lazyService = &lazy.lazyStub
				}
				clientCaps := frontend.ClientCaps{MaxBitrateKbps: 1234, VideoCodecs: []string{"h264"}, HLSContainers: []string{"mpegts"}}
				host := frontend.Host{Sessions: planner, LazyStreams: lazyService,
					Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
					Experience: frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
						return frontend.ItemDetail{Title: frontend.Title{Key: 5, Type: "movie"}}, nil
					}},
					Capabilities: frontendtest.Capabilities{BuildFunc: func(context.Context, frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
						return frontend.CapabilityResult{Client: clientCaps}, nil
					}},
					SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/" + protocol, nil },
				}
				var front frontend.Frontend = Jellyfin{MachineID: "test"}
				if protocol == "emby" {
					front = Emby{MachineID: "test"}
				}
				inst, err := front.Mount(context.Background(), host)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("GET", "/Items/"+ids.RatingKeyGUID(5), nil)
				r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
				w := httptest.NewRecorder()
				inst.Handler().ServeHTTP(w, r)
				var dto struct{ MediaSources []map[string]any }
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &dto) != nil || len(dto.MediaSources) != 1 {
					t.Fatal(w.Code, w.Body.String())
				}
				source := dto.MediaSources[0]
				offered := tc.hls && tc.issueErr == nil && !tc.estimated && !tc.missingRevision && !tc.missingIssuer
				_, hasURL := source["TranscodingUrl"]
				if hasURL != offered || source["SupportsDirectStream"] != (offered && tc.remux) || source["SupportsTranscoding"] != (offered && tc.encode) {
					t.Fatal(source)
				}
				if strings.Contains(w.Body.String(), "account-secret") || strings.Contains(w.Body.String(), "/sessions/media/") {
					t.Fatal("credential leak", w.Body.String())
				}
				if offered {
					if source["TranscodingUrl"] != "https://ether.example/"+protocol+lazy.hlsPath || source["TranscodingSubProtocol"] != "hls" || source["TranscodingContainer"] != "ts" {
						t.Fatal(source)
					}
					req := lazy.hlsSeen
					if req.Viewer.ID != 1 || req.Device.ID != "device" || req.Protocol != frontend.Protocol(protocol) || req.Item != 5 || req.DetailSource != planner.plan.DetailSource || req.ExpectedSourceETag != "source-revision" || req.Version != "" || req.Intent.ExactVersion || !reflect.DeepEqual(req.Caps, clientCaps) || lazy.output.Format != frontend.StreamHLS {
						t.Fatal("lost scope or constraints", req)
					}
				}
				if !tc.hls && lazy.calls != 0 {
					t.Fatal("issued without HLS eligibility")
				}
				if tc.direct && !tc.estimated && !tc.missingRevision && (source["SupportsDirectPlay"] != true || source["Path"] != "https://ether.example/"+protocol+"/sessions/lazy/direct/file") {
					t.Fatal("lost independent direct fallback", source)
				}
			})
		}
	}
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
