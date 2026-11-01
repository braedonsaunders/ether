package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

// Infuse opens PlaybackInfo, then ignores that session and reads
// /Videos/{id}/stream?Static=true. It keeps posting the PlaybackInfo
// PlaySessionId. That id is a retired broker session; state_watch must
// still move.
func TestInfuseStaticStreamProgressReachesWatchState(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	watch := state.New(db, state.Options{})
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeMovie, Title: "Abzurdah"}, nil
	}
	broker, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, Transcoder: fakeBrokerTranscoder{},
		Progress: watch, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: lookup, MediaLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	grants := infuseGrant{}
	lazy, err := NewLazyStreams(db, broker, "jellyfin", grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: 1}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Sessions: broker, LazyStreams: lazy,
		Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: core.ClientCaps{Protocol: "jellyfin", ClientID: req.Device.ClientID, Product: req.Device.Product}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(ctx)
	key := core.MovieKey(531)
	guid := ids.RatingKeyGUID(key)
	call := func(method, path, body, rang string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9", Token="grant"`)
		if rang != "" {
			r.Header.Set("Range", rang)
		}
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	opened := call(http.MethodPost, "/jellyfin/Items/"+guid+"/PlaybackInfo", `{"IsPlayback":true,"MediaSourceId":"`+guid+`"}`, "")
	if opened.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo %d %s", opened.Code, opened.Body.String())
	}
	var play struct {
		PlaySessionId string
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &play); err != nil || play.PlaySessionId == "" {
		t.Fatalf("play session %q %v", opened.Body.String(), err)
	}
	userID, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	streamPath := "/jellyfin/Videos/" + guid + "/stream?MediaSourceId=" + guid + "&Static=true&UserId=" + userID
	head := call(http.MethodHead, streamPath, "", "")
	if head.Code != http.StatusOK && head.Code != http.StatusPartialContent {
		t.Fatalf("static HEAD %d %s", head.Code, head.Body.String())
	}
	stream := call(http.MethodGet, streamPath, "", "bytes=0-3")
	if stream.Code != http.StatusOK && stream.Code != http.StatusPartialContent {
		t.Fatalf("static stream %d %s", stream.Code, stream.Body.String())
	}
	listed, err := broker.Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: core.Device{ID: "appletv"}, Protocol: "jellyfin"})
	if err != nil || len(listed) != 1 || listed[0].ID != play.PlaySessionId {
		t.Fatalf("static stream did not reuse PlaybackInfo session %+v play=%s err=%v", listed, play.PlaySessionId, err)
	}
	report := func(path string, ticks int64) {
		t.Helper()
		body := `{"ItemId":"` + guid + `","MediaSourceId":"` + guid + `","PlaySessionId":"` + play.PlaySessionId + `","PlayMethod":"DirectStream","PositionTicks":` + strconv.FormatInt(ticks, 10) + `,"RunTimeTicks":54000000000}`
		w := call(http.MethodPost, path, body, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s %d %s", path, w.Code, w.Body.String())
		}
	}
	report("/jellyfin/Sessions/Playing", 100000000)
	report("/jellyfin/Sessions/Playing/Progress", 9000000000)
	const stoppedTicks int64 = 17983060777
	report("/jellyfin/Sessions/Playing/Stopped", stoppedTicks)
	var offset int64
	if err := db.QueryRow(`SELECT view_offset_ns FROM state_watch WHERE user_id=1 AND rating_key=?`, int64(key)).Scan(&offset); err != nil {
		t.Fatal(err)
	}
	want := int64(time.Duration(stoppedTicks) * 100 * time.Nanosecond)
	if offset != want {
		t.Fatalf("state_watch offset %d, want %d", offset, want)
	}
}

type failingPrepare struct {
	*fakePlayback
	err error
}

func (p *failingPrepare) Prepare(context.Context, *core.User, core.RatingKey, core.ClientCaps) (*core.PlaySession, error) {
	return nil, p.err
}
func (p *failingPrepare) PrepareSurface(context.Context, *core.User, core.RatingKey, core.ClientCaps, string, bool) (*core.PlaySession, error) {
	return nil, p.err
}

func TestJellyfinStaticMissingSourceMatchesMissingFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		retry   string
		body    string
		content string
	}{
		{name: "not-found", err: core.ErrNotFound, status: http.StatusNotFound, body: "Error processing request.", content: "text/plain"},
		{name: "unavailable", err: core.ErrNotAvailable, status: http.StatusNotFound, body: "Error processing request.", content: "text/plain"},
		{name: "busy", err: &frontend.SessionBusyError{RetryAfter: 1500 * time.Millisecond}, status: http.StatusServiceUnavailable, retry: "2", body: "playback unavailable\n", content: "text/plain; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := store.OpenTest(t)
			active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
			events := NewEventBus(nil, active)
			defer events.Close()
			lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: key, Type: core.TypeMovie}, nil
			}
			broker, err := NewBroker(BrokerDeps{
				Events: events, Playback: &failingPrepare{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, err: tc.err},
				Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active,
				CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 },
				OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close(ctx)
			lazy, err := NewLazyStreams(db, broker, "jellyfin", infuseGrant{}, time.Hour, func(*http.Request) bool { return false })
			if err != nil {
				t.Fatal(err)
			}
			viewer := &frontend.Viewer{ID: 1}
			inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
				Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
				Sessions: broker, LazyStreams: lazy,
				Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
					return frontend.CapabilityResult{Client: core.ClientCaps{Protocol: "jellyfin", ClientID: req.Device.ClientID}}, nil
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer inst.Close(ctx)
			guid := ids.RatingKeyGUID(core.MovieKey(8))
			r := httptest.NewRequest(http.MethodGet, "/jellyfin/Videos/"+guid+"/stream?Static=true", nil)
			r.Header.Set("Authorization", `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9"`)
			r.Header.Set("X-Emby-Token", "grant")
			r.Header.Set("Range", "bytes=0-")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != tc.status || w.Body.String() != tc.body || w.Header().Get("Retry-After") != tc.retry || w.Header().Get("Content-Type") != tc.content {
				t.Fatalf("status=%d retry=%q type=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Header().Get("Content-Type"), w.Body.String())
			}
		})
	}
}

func TestInfuseStaticStreamOpensWithoutPlaybackInfo(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeMovie, Title: "Sintel"}, nil
	}
	broker, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, Transcoder: fakeBrokerTranscoder{},
		Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: lookup, MediaLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	lazy, err := NewLazyStreams(db, broker, "jellyfin", infuseGrant{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: 1}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Sessions: broker, LazyStreams: lazy,
		Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: core.ClientCaps{Protocol: "jellyfin", ClientID: req.Device.ClientID, Product: req.Device.Product}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(ctx)
	guid := ids.RatingKeyGUID(core.MovieKey(9))
	userID, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/jellyfin/Videos/" + guid + "/stream?MediaSourceId=" + guid + "&Static=true&UserId=" + userID
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9", Token="grant"`)
		r.Header.Set("User-Agent", "Infuse-Direct/8.5.9")
		if method == http.MethodGet {
			r.Header.Set("Range", "bytes=0-3")
		}
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK && w.Code != http.StatusPartialContent {
			t.Fatalf("%s static stream %d %s", method, w.Code, w.Body.String())
		}
		if method == http.MethodHead && w.Body.Len() != 0 {
			t.Fatalf("HEAD body %q", w.Body.String())
		}
	}
	listed, err := broker.Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: core.Device{ID: "appletv"}, Protocol: "jellyfin"})
	if err != nil || len(listed) != 1 || listed[0].Item != core.MovieKey(9) {
		t.Fatalf("static stream did not open a broker session %+v err=%v", listed, err)
	}
}

type remuxBrokerTranscoder struct{ fakeBrokerTranscoder }

func (remuxBrokerTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "mp4"}
}

func TestInfuseRemuxPlaybackInfoStillServesStaticStream(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeMovie, Title: "Sintel"}, nil
	}
	broker, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, Transcoder: remuxBrokerTranscoder{},
		Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: lookup, MediaLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	lazy, err := NewLazyStreams(db, broker, "jellyfin", infuseGrant{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: 1}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }},
		Sessions: broker, LazyStreams: lazy,
		Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: core.ClientCaps{Protocol: "jellyfin", ClientID: req.Device.ClientID, Product: req.Device.Product}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(ctx)
	guid := ids.RatingKeyGUID(core.MovieKey(12))
	auth := `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9", Token="grant"`
	info := httptest.NewRequest(http.MethodPost, "/jellyfin/Items/"+guid+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true,"MediaSourceId":"`+guid+`"}`))
	info.Header.Set("Authorization", auth)
	opened := httptest.NewRecorder()
	inst.Handler().ServeHTTP(opened, info)
	if opened.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo %d %s", opened.Code, opened.Body.String())
	}
	var play struct {
		PlaySessionId string
		MediaSources  []map[string]any
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &play); err != nil || play.PlaySessionId == "" {
		t.Fatalf("play session %q %v", opened.Body.String(), err)
	}
	if len(play.MediaSources) != 1 {
		t.Fatalf("sources %+v", play.MediaSources)
	}
	path, _ := play.MediaSources[0]["Path"].(string)
	if !strings.Contains(path, "/sessions/media/") || !strings.HasSuffix(path, "/file") {
		t.Fatalf("remux PlaybackInfo Path %q", path)
	}
	userID, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/jellyfin/Videos/"+guid+"/stream?MediaSourceId="+guid+"&Static=true&UserId="+userID, nil)
	r.Header.Set("Authorization", auth)
	r.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK && w.Code != http.StatusPartialContent {
		t.Fatalf("static remux stream %d %s", w.Code, w.Body.String())
	}
	listed, err := broker.Sessions(ctx, frontend.SessionAccess{Viewer: viewer, Device: core.Device{ID: "appletv"}, Protocol: "jellyfin"})
	if err != nil || len(listed) != 1 || listed[0].ID != play.PlaySessionId {
		t.Fatalf("remux static did not reuse session %+v play=%s err=%v", listed, play.PlaySessionId, err)
	}
}

func TestInfuseStaticStreamWithoutTokenIsUnauthorized(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeMovie}, nil
	}
	broker, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, Transcoder: fakeBrokerTranscoder{},
		Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: lookup, MediaLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	lazy, err := NewLazyStreams(db, broker, "jellyfin", infuseGrant{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
			return &frontend.Viewer{ID: 1}, nil
		}},
		Sessions: broker, LazyStreams: lazy, Capabilities: frontendtest.Capabilities{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(ctx)
	guid := ids.RatingKeyGUID(core.MovieKey(11))
	r := httptest.NewRequest(http.MethodGet, "/jellyfin/Videos/"+guid+"/stream?Static=true", nil)
	r.Header.Set("Authorization", `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9"`)
	r.Header.Set("User-Agent", "Infuse-Direct/8.5.9")
	r.Header.Set("Range", "bytes=0-")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing Infuse token %d %s", w.Code, w.Body.String())
	}
}

type transcodeBrokerTranscoder struct{ fakeBrokerTranscoder }

func (transcodeBrokerTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", AudioCodec: "aac", Container: "mpegts"}
}

func TestInfuseEstimateAndTranscodeResumeStayPlayable(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeMovie, Title: "Sintel"}, nil
	}
	broker, err := NewBroker(BrokerDeps{
		Events: events, Playback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}, Transcoder: transcodeBrokerTranscoder{},
		Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Lookup: lookup, MediaLookup: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	lazy, err := NewLazyStreams(db, broker, "jellyfin", infuseGrant{}, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	inst, err := (mediabrowser.Jellyfin{MachineID: "test"}).Mount(ctx, frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) {
			return &frontend.Viewer{ID: 1}, nil
		}},
		Sessions: broker, LazyStreams: lazy,
		Capabilities: frontendtest.Capabilities{BuildFunc: func(_ context.Context, req frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
			return frontend.CapabilityResult{Client: core.ClientCaps{Protocol: "jellyfin", ClientID: req.Device.ClientID, Product: req.Device.Product}}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(ctx)
	guid := ids.RatingKeyGUID(core.MovieKey(38))
	auth := `MediaBrowser Client="Infuse-Direct", Device="AppleTV", DeviceId="appletv", Version="8.5.9", Token="grant"`
	estimate := httptest.NewRequest(http.MethodPost, "/jellyfin/Items/"+guid+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":false}`))
	estimate.Header.Set("Authorization", auth)
	got := httptest.NewRecorder()
	inst.Handler().ServeHTTP(got, estimate)
	var preview struct {
		MediaSources []map[string]any
	}
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &preview) != nil || len(preview.MediaSources) != 1 {
		t.Fatalf("estimate %d %s", got.Code, got.Body.String())
	}
	if preview.MediaSources[0]["SupportsDirectPlay"] != true || preview.MediaSources[0]["Protocol"] != "File" {
		t.Fatalf("estimate source %+v", preview.MediaSources[0])
	}
	resume := httptest.NewRequest(http.MethodPost, "/jellyfin/Items/"+guid+"/PlaybackInfo", strings.NewReader(`{"IsPlayback":true,"StartTimeTicks":23000000000}`))
	resume.Header.Set("Authorization", auth)
	opened := httptest.NewRecorder()
	inst.Handler().ServeHTTP(opened, resume)
	var play struct {
		PlaySessionId string
		MediaSources  []map[string]any
	}
	if opened.Code != http.StatusOK || json.Unmarshal(opened.Body.Bytes(), &play) != nil || len(play.MediaSources) != 1 {
		t.Fatalf("resume %d %s", opened.Code, opened.Body.String())
	}
	if play.MediaSources[0]["SupportsDirectPlay"] != true || play.MediaSources[0]["Protocol"] != "File" {
		t.Fatalf("resume source %+v", play.MediaSources[0])
	}
	path, _ := play.MediaSources[0]["Path"].(string)
	if play.PlaySessionId == "" || !strings.Contains(path, "/sessions/media/") || !strings.HasSuffix(path, "/file") {
		t.Fatalf("resume session %q path %q", play.PlaySessionId, path)
	}
}

type infuseGrant struct{}

func (infuseGrant) GrantReference(context.Context, string, string) (string, *core.Viewer, error) {
	return "reference", &core.Viewer{ID: 1}, nil
}
func (infuseGrant) ValidateGrantReference(context.Context, string, string, int64) error {
	return nil
}
