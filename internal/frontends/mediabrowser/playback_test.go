package mediabrowser

import (
	"context"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestMediaDetailsPreserveChapterAndInterlaceMetadata(t *testing.T) {
	media := frontend.MediaInfo{Chapters: []core.Chapter{{Title: "Opening", Start: 15 * time.Second}}, Streams: []frontend.StreamInfo{{Type: "video", Index: 0, Interlaced: true}, {Type: "audio", Index: 1}}}
	source := map[string]any{}
	addChapters(source, media)
	chapters := source["Chapters"].([]any)
	chapter := chapters[0].(map[string]any)
	if chapter["Name"] != "Opening" || chapter["StartPositionTicks"] != int64(150000000) {
		t.Fatal(chapter)
	}
	streams := mediaStreams(media.Streams)
	if streams[0].(map[string]any)["IsInterlaced"] != true {
		t.Fatal(streams)
	}
	if streams[1].(map[string]any)["IsInterlaced"] != false {
		t.Fatal(streams)
	}
}

func TestMediaStreamUsesReportedDispositions(t *testing.T) {
	yes, no := true, false
	streams := mediaStreams([]frontend.StreamInfo{{Type: "audio", Default: true, DefaultDisposition: &no, ForcedDisposition: &yes, HearingImpaired: &yes, Original: &yes}})
	got := streams[0].(map[string]any)
	if got["IsDefault"] != false || got["IsForced"] != true || got["IsHearingImpaired"] != true || got["IsOriginal"] != true {
		t.Fatal(got)
	}
}

func TestStreamCapacityResponseIncludesRetryDelay(t *testing.T) {
	w := httptest.NewRecorder()
	streamError(w, &frontend.TranscodeCapacityError{RetryAfter: 1500 * time.Millisecond})
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" {
		t.Fatal(w.Code, w.Header())
	}
}

func TestPlaybackInfoEstimatesAndOpens(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var calls []string
	sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, req frontend.PlanRequest) (frontend.PlayPlan, error) {
		if req.Viewer != viewer || req.Protocol != "jellyfin" || req.Item != 5 || req.Intent.Options.DirectStream || req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding {
			t.Fatal(req)
		}
		if req.Intent.Estimate {
			calls = append(calls, "estimate")
		} else {
			calls = append(calls, "plan")
		}
		tag := ""
		if !req.Intent.Estimate {
			tag = "source-revision"
		}
		return frontend.PlayPlan{ID: "plan", Estimated: req.Intent.Estimate, Media: frontend.MediaInfo{Container: "mp4"}, Decision: frontend.Decision{Kind: "directplay"}, DirectPlay: true, ETag: tag}, nil
	}, OpenFunc: func(_ context.Context, req frontend.OpenRequest) (frontend.Session, error) {
		calls = append(calls, "open")
		return frontend.Session{ID: "session"}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, id string, _ frontend.StreamRequest) (string, error) {
		calls = append(calls, "stream")
		return "/sessions/media/path/grant/direct", nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions, Capabilities: frontendtest.Capabilities{}, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/jellyfin", nil }})
	if err != nil {
		t.Fatal(err)
	}
	path := "/jellyfin/Items/" + ids.RatingKeyGUID(5) + "/PlaybackInfo"
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	w := call(`{"EnableDirectStream":false,"EnableTranscoding":false}`)
	if w.Code != 200 || strings.Join(calls, ",") != "estimate" || !strings.Contains(w.Body.String(), `"SupportsDirectPlay":true`) || !strings.Contains(w.Body.String(), `"SupportsTranscoding":false`) {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("POST", strings.TrimPrefix(path, "/jellyfin"), w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Fatal(e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if w := call(`{"IsPlayback":true,"EnableDirectStream":false,"EnableTranscoding":false}`); w.Code != 200 || strings.Join(calls, ",") != "estimate,plan,open,stream" || !strings.Contains(w.Body.String(), "https://ether.example/jellyfin/sessions/media/") || !strings.Contains(w.Body.String(), `"ETag":"source-revision"`) {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
}

func TestTimelineReportsAndStops(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var states []string
	sessions := frontendtest.Sessions{ReportFunc: func(_ context.Context, a frontend.SessionAccess, id string, timeline frontend.Timeline) error {
		if a.Viewer != viewer || id != "session" || timeline.Position.Milliseconds() != 1000 {
			t.Fatal(a, id, timeline)
		}
		states = append(states, timeline.State)
		return nil
	}, StopFunc: func(_ context.Context, _ frontend.SessionAccess, id string) error {
		states = append(states, "stop")
		return nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	var doc *openapi.Document
	if e == nil {
		doc, e = openapi.Parse(spec)
	}
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	for _, p := range []string{"/Sessions/Playing/Progress", "/Sessions/Playing/Stopped"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", p, strings.NewReader(`{"PlaySessionId":"session","PositionTicks":10000000}`))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
		if doc != nil {
			if e := doc.Validate("POST", p, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
				t.Fatal(e)
			}
		}
	}
	if strings.Join(states, ",") != "playing,stopped,stop" {
		t.Fatal(states)
	}
}

func TestStreamAliasChecksSessionItemAndOmitsCredential(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	calls := 0
	sessions := frontendtest.Sessions{SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
		return []frontend.Session{{ID: "session", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}}}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
		calls++
		if id != "session" || req.Format != frontend.StreamHLS {
			t.Fatal(id, req)
		}
		return "/sessions/media/session/scoped/index.m3u8", nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	call := func(key frontend.RatingKey) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/jellyfin/Videos/"+ids.RatingKeyGUID(key)+"/master.m3u8?PlaySessionId=session&api_key=secret", nil)
		r.Header.Set("X-Emby-Device-Id", "tv")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call(6); w.Code != 404 || calls != 0 {
		t.Fatal(w.Code, calls)
	}
	if w := call(5); w.Code != 307 || calls != 1 || w.Header().Get("Location") != "/jellyfin/sessions/media/session/scoped/index.m3u8" || strings.Contains(w.Header().Get("Location"), "secret") {
		t.Fatal(w.Code, w.Header(), calls)
	}
}

func TestPlaybackQueryPassesSelectionsAndVerifiedPeer(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	capsCalled := false
	planned := false
	capabilities := frontendtest.Capabilities{BuildFunc: func(ctx context.Context, r frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		ip, ok := frontend.ClientIP(ctx)
		if !ok || ip.String() != "192.0.2.1" || r.Profile.MaxStreamingBitrate != 4000000 {
			t.Fatal(ip, r.Profile)
		}
		capsCalled = true
		return frontend.CapabilityResult{}, nil
	}}
	broker := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		if r.Intent.Position != 2*time.Second || r.Intent.Options.AudioStream != 3 || r.Intent.Options.SubtitleStream != -1 || !r.Intent.AudioSet || !r.Intent.SubtitleSet || r.Intent.AllowTranscoding == nil || *r.Intent.AllowTranscoding {
			t.Fatal(r.Intent)
		}
		planned = true
		return frontend.PlayPlan{Estimated: true}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: broker, Capabilities: capabilities, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.1"), true }})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo?startTimeTicks=20000000&audioStreamIndex=3&subtitleStreamIndex=-1&maxStreamingBitrate=4000000&enableTranscoding=false", strings.NewReader(`{}`))
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !capsCalled || !planned {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestVersionSelectionDelegatesToBroker(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	var version string
	broker := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		version = r.Version
		return frontend.PlayPlan{Estimated: true}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Sessions: broker, Capabilities: frontendtest.Capabilities{}, VersionHandles: &testVersionHandles{item: 5, handle: "variant-1"}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/Items/"+ids.RatingKeyGUID(5)+"/PlaybackInfo?MediaSourceId=variant-1", strings.NewReader(`{}`))
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || version != "variant-1" {
		t.Fatal(w.Code, version, w.Body.String())
	}
}

func TestSessionBusyReturnsRetryAfterOnPlaybackAndStream(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	broker := frontendtest.Sessions{
		PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
			return frontend.PlayPlan{ID: "plan", Decision: frontend.Decision{Kind: "directplay"}}, nil
		},
		OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
			return frontend.Session{ID: "private-session"}, nil
		},
		StartStreamFunc: func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error) {
			return "", &frontend.SessionBusyError{RetryAfter: 1500 * time.Millisecond}
		},
		SessionsFunc: func(context.Context, frontend.SessionAccess) ([]frontend.Session, error) {
			return []frontend.Session{{ID: "private-session", Item: 5, Protocol: "jellyfin", Device: frontend.Device{ID: "tv"}}}, nil
		},
	}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: identity, Sessions: broker, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	id := ids.RatingKeyGUID(5)
	for _, tc := range []struct{ method, path, body string }{{"POST", "/Items/" + id + "/PlaybackInfo", `{"IsPlayback":true}`}, {"GET", "/Videos/" + id + "/stream?PlaySessionId=private-session", ""}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("X-Emby-Token", "account-secret")
		req.Header.Set("X-Emby-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, req)
		if w.Code != 503 || w.Header().Get("Retry-After") != "2" || strings.Contains(w.Body.String(), "account-secret") || strings.Contains(w.Body.String(), "private-session") {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
	}
}
