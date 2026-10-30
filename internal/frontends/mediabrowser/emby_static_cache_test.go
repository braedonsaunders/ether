package mediabrowser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type staticCacheBroker struct {
	lazyStub
	calls  int
	status int
	policy string
}

func (s *staticCacheBroker) ServeAuthenticated(w http.ResponseWriter, r *http.Request, credential string, plan frontend.PlanRequest) {
	s.calls++
	s.credential, s.seen = credential, plan
	if s.policy != "" {
		w.Header().Set("Cache-Control", s.policy)
	}
	if s.status != 0 {
		http.Error(w, "broker denied", s.status)
		return
	}
	w.Header().Set("ETag", `"source-revision"`)
	http.ServeContent(w, r, "video.mp4", time.Time{}, strings.NewReader("abcdefghij"))
}

// The captured Emby SDK start/middle/suffix 206 responses all contain exactly
// private, no-transform. Test actual mounted routes and range bytes, without
// reproducing the reference's known ignored-suffix-range defect.
func TestEmbyStaticCacheMounted(t *testing.T) {
	for _, protocol := range []string{"emby", "jellyfin"} {
		for _, tc := range []struct {
			name, method, rangeHeader, token, query, conditional, policy string
			status, brokerStatus, calls                                  int
			body, contentRange                                           string
		}{
			{name: "full", method: "GET", token: "valid", status: 200, calls: 1, body: "abcdefghij"},
			{name: "head", method: "HEAD", token: "valid", status: 200, calls: 1},
			{name: "start", method: "GET", token: "valid", rangeHeader: "bytes=0-2", status: 206, calls: 1, body: "abc", contentRange: "bytes 0-2/10"},
			{name: "middle", method: "GET", token: "valid", rangeHeader: "bytes=3-5", status: 206, calls: 1, body: "def", contentRange: "bytes 3-5/10"},
			{name: "suffix", method: "GET", token: "valid", rangeHeader: "bytes=-2", status: 206, calls: 1, body: "ij", contentRange: "bytes 8-9/10"},
			{name: "range-error", method: "GET", token: "valid", rangeHeader: "bytes=99-100", status: 416, calls: 1},
			{name: "anonymous", method: "GET", status: 401},
			{name: "revoked", method: "GET", token: "revoked", status: 401},
			{name: "broker-revoked", method: "GET", token: "valid", status: 401, brokerStatus: 401, calls: 1},
			{name: "unavailable", method: "GET", token: "valid", status: 503, brokerStatus: 503, calls: 1},
			{name: "source-binding", method: "GET", token: "valid", query: "&MediaSourceId=unknown", status: 404},
			{name: "duplicate-source", method: "GET", token: "valid", query: "&MediaSourceId=a&MediaSourceId=b", status: 400},
			{name: "conditional", method: "GET", token: "valid", conditional: `"source-revision"`, status: 304, calls: 1},
			{name: "stricter-broker", method: "GET", token: "valid", policy: "no-store", status: 200, calls: 1, body: "abcdefghij"},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				broker := &staticCacheBroker{status: tc.brokerStatus, policy: tc.policy}
				viewer := &frontend.Viewer{ID: 1}
				host := frontend.Host{
					Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
						if token != "valid" {
							return nil, errors.New("revoked")
						}
						return viewer, nil
					}},
					Sessions: frontendtest.Sessions{}, Capabilities: frontendtest.Capabilities{}, LazyStreams: broker,
				}
				var front frontend.Frontend = Emby{MachineID: "test"}
				if protocol == "jellyfin" {
					front = Jellyfin{MachineID: "test"}
				}
				inst, err := front.Mount(context.Background(), host)
				if err != nil {
					t.Fatal(err)
				}
				defer inst.Close(context.Background())
				r := httptest.NewRequest(tc.method, "/"+protocol+"/Videos/"+ids.RatingKeyGUID(5)+"/stream?Static=true"+tc.query, nil)
				r.Header.Set("X-Emby-Token", tc.token)
				r.Header.Set("X-Emby-Device-Id", "device")
				r.Header.Set("Range", tc.rangeHeader)
				r.Header.Set("If-None-Match", tc.conditional)
				w := httptest.NewRecorder()
				inst.Handler().ServeHTTP(w, r)
				if w.Code != tc.status || broker.calls != tc.calls {
					t.Fatalf("status=%d calls=%d body=%s", w.Code, broker.calls, w.Body.String())
				}
				wantPolicy := tc.policy
				if protocol == "emby" {
					if tc.status >= 400 {
						wantPolicy = "no-store"
					} else if wantPolicy == "" {
						wantPolicy = "private, no-transform"
					}
				}
				if got := w.Result().Header.Get("Cache-Control"); got != wantPolicy {
					t.Fatalf("Cache-Control=%q want %q", got, wantPolicy)
				}
				if tc.status < 400 && w.Body.String() != tc.body {
					t.Fatalf("body=%q want %q", w.Body.String(), tc.body)
				}
				if tc.contentRange != "" && w.Header().Get("Content-Range") != tc.contentRange {
					t.Fatal(w.Header())
				}
				if broker.calls > 0 && (broker.credential != "valid" || broker.seen.Viewer != viewer || broker.seen.Item != 5 || broker.seen.Device.ID != "device" || broker.seen.Protocol != frontend.Protocol(protocol)) {
					t.Fatalf("wrong broker binding: %+v", broker.seen)
				}
			})
		}
	}
}

func TestEmbyStaticCacheRechecksSameCredential(t *testing.T) {
	revoked := false
	broker := &staticCacheBroker{}
	inst, err := (Emby{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
			if revoked {
				return nil, errors.New("revoked")
			}
			return &frontend.Viewer{ID: 1}, nil
		}}, Sessions: frontendtest.Sessions{}, Capabilities: frontendtest.Capabilities{}, LazyStreams: broker,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(context.Background())
	for _, status := range []int{206, 401} {
		r := httptest.NewRequest("GET", "/Videos/"+ids.RatingKeyGUID(5)+"/stream?Static=true", nil)
		r.Header.Set("X-Emby-Token", "valid")
		r.Header.Set("X-Emby-Device-Id", "device")
		r.Header.Set("Range", "bytes=0-2")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != status || broker.calls != 1 {
			t.Fatalf("status=%d calls=%d", w.Code, broker.calls)
		}
		if revoked && (w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "abc")) {
			t.Fatal(w.Header(), w.Body.String())
		}
		revoked = true
	}
}

func TestEmbyStaticCacheSessionRedirect(t *testing.T) {
	inst, err := (Emby{MachineID: "test"}).Mount(context.Background(), frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }},
		Sessions: frontendtest.Sessions{
			SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
				return []frontend.Session{{ID: "session", Item: 5, Protocol: "emby", Device: frontend.Device{ID: "device"}}}, nil
			},
			StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
				return "/sessions/media/session/grant/file", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(context.Background())
	for _, static := range []string{"true", "false"} {
		r := httptest.NewRequest("GET", "/emby/Videos/"+ids.RatingKeyGUID(5)+"/stream?Static="+static+"&PlaySessionId=session", nil)
		r.Header.Set("X-Emby-Token", "valid")
		r.Header.Set("X-Emby-Device-Id", "device")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		want := ""
		if static == "true" {
			want = "no-store"
		}
		if w.Code != 307 || w.Header().Get("Cache-Control") != want || w.Header().Get("Location") != "/emby/sessions/media/session/grant/file" {
			t.Fatal(w.Code, w.Header())
		}
	}
}
