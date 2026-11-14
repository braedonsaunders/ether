package sessions

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type originalAudioTranscoder struct {
	*fakeMediaTranscoder
	requests []core.HLSRequest
}

func (t *originalAudioTranscoder) AudioFallbackAvailable() bool { return true }
func (t *originalAudioTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	t.requests = append(t.requests, r)
	return t.fakeMediaTranscoder.StartHLS(ctx, r)
}

func TestOriginalPlaybackNegotiationAndGeneration(t *testing.T) {
	runtime := &frontend.OriginalPlaybackCapabilities{Version: 1, Range: true, DemuxContainers: []string{"mkv"}, VideoDecoders: []string{"hevc"}, AudioDecoders: []string{"truehd"}, WebGPU: true}
	info := core.MediaInfo{Container: "mkv", Size: 100, Streams: []core.StreamInfo{{Type: "video", Index: 0, Codec: "hevc", Height: 2160}, {Type: "audio", Index: 4, Codec: "truehd"}}}
	b := &Broker{}
	request := frontend.PlanRequest{Protocol: "vela", Caps: core.ClientCaps{Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, MaxHeight: 480, MaxBitrateKbps: 100}, Intent: frontend.PlayIntent{OriginalPlayback: runtime, Options: core.DecideOptions{DirectPlay: true, AudioStream: 4, SubtitleStream: -1}}}
	source := &core.PlaySession{Info: info}
	d, err := b.decideProfile(source, request)
	if err != nil || d.Kind != core.DecisionDirectPlay || d.Reason != "original_runtime" || d.MaxHeight != 0 || d.BitrateKbps != 0 || d.ToneMap {
		t.Fatalf("original budget altered: %+v %v", d, err)
	}
	for _, change := range []func(*frontend.PlanRequest){
		func(r *frontend.PlanRequest) { r.Intent.OriginalPlayback = nil },
		func(r *frontend.PlanRequest) { c := *runtime; c.Version = 2; r.Intent.OriginalPlayback = &c },
		func(r *frontend.PlanRequest) { c := *runtime; c.Range = false; r.Intent.OriginalPlayback = &c },
		func(r *frontend.PlanRequest) {
			c := *runtime
			c.AudioDecoders = []string{"aac"}
			r.Intent.OriginalPlayback = &c
		},
		func(r *frontend.PlanRequest) { r.Channel = "live" },
		func(r *frontend.PlanRequest) { r.Protocol = "plex" },
	} {
		req := request
		change(&req)
		d, _ := b.decideProfile(source, req)
		if d.Reason == "original_runtime" {
			t.Fatalf("unnegotiated original: %+v", req)
		}
	}
	ctx := context.Background()
	pb := &sourceTrackPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, source: &revisionSource{Reader: bytes.NewReader(nil), id: "release", name: "video.mkv", size: 100}, info: info}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	defer bus.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	tc := &originalAudioTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}}
	gated := &Broker{deps: BrokerDeps{Transcoder: tc}}
	candidate := request
	audioRuntime := *runtime
	audioRuntime.AudioDecoders = []string{"aac"}
	candidate.Intent.OriginalPlayback = &audioRuntime
	if d, err := gated.decideProfile(source, candidate); err != nil || d.Reason != "original_runtime" {
		t.Fatalf("real audio fallback not admitted: %+v %v", d, err)
	}
	audioRuntime.AudioDecoders = []string{"opus"}
	if d, _ := gated.decideProfile(source, candidate); d.Reason == "original_runtime" {
		t.Fatal("AAC fallback admitted without installed AAC decoder")
	}
	broker, err := NewBroker(BrokerDeps{Transcoder: tc, SurfacePolicy: testSessionPolicy, Events: bus, Playback: pb, Progress: &fakeProgress{}, ActiveViewer: active, Lookup: lookup, MediaLookup: lookup, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	viewer, device := &core.Viewer{ID: 1}, core.Device{ID: "runtime"}
	request.Viewer, request.Device, request.Item = viewer, device, core.MovieKey(1)
	plan, err := broker.Plan(ctx, request)
	if err != nil || pb.pins != 1 || plan.ETag == "" {
		t.Fatalf("plan %+v pins %d %v", plan, pb.pins, err)
	}
	session, err := broker.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "vela", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "vela"}
	path, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil {
		t.Fatal(err)
	}
	// Audio is a separate rendition: no video encode, unchanged direct URL,
	// independently cancelable/replaced generations, and the same source pin.
	audio, err := broker.StartAudioFallback(ctx, access, session.ID, 4, 33*time.Second)
	if err != nil || audio.Generation != 1 || len(tc.requests) != 1 {
		t.Fatalf("audio %+v %v", audio, err)
	}
	req := tc.requests[0]
	if len(req.Info.Streams) != 1 || req.Info.Streams[0].Type != "audio" || req.Decision.AudioStream != 4 || !req.Decision.VideoCopy || req.Decision.AudioCopy || req.StartOffset != 33*time.Second {
		t.Fatalf("audio crossed video/track boundary: %+v", req)
	}
	currentPath, err := broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil || currentPath != path || len(tc.sessions) != 1 {
		t.Fatal("audio changed direct video", err)
	}
	replacement, err := broker.StartAudioFallback(ctx, access, session.ID, 4, 60*time.Second)
	if err != nil || replacement.Generation != 2 || replacement.ID == audio.ID || len(tc.sessions) != 1 {
		t.Fatalf("replacement %+v %v", replacement, err)
	}
	if err = broker.StopAudioFallback(ctx, access, session.ID, audio.ID); err != nil || len(tc.sessions) != 1 {
		t.Fatal("stale stop canceled replacement")
	}
	if err = broker.StopAudioFallback(ctx, access, session.ID, replacement.ID); err != nil || len(tc.sessions) != 0 || pb.pins != 1 {
		t.Fatal("audio stop released video/source pin", err)
	}
	pb.source.id = "replacement"
	out := httptest.NewRecorder()
	broker.ServeMediaFor("vela", out, httptest.NewRequest(http.MethodGet, path, nil))
	if out.Code == 200 || out.Code == 206 {
		t.Fatal("mixed bytes from changed source", out.Code)
	}
	if _, err = broker.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); !errors.Is(err, frontend.ErrSourceGone) {
		t.Fatalf("reopened replacement: %v", err)
	}
}
