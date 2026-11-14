package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type trackOutputTranscoder struct {
	*fakeMediaTranscoder
	last core.HLSRequest
}

func (f *trackOutputTranscoder) Decide(_ core.MediaInfo, _ core.ClientCaps, o core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, Container: "fmp4", AudioStream: o.AudioStream, SubtitleStream: o.SubtitleStream}
}
func (f *trackOutputTranscoder) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	f.last = r
	return f.fakeMediaTranscoder.StartHLS(ctx, r)
}

func TestScopedOutputTrackRestart(t *testing.T) {
	ctx := context.Background()
	b, p := newDecisionBroker(t)
	defer b.Close(ctx)
	v := &core.Viewer{ID: 2}
	device := core.Device{ID: "device"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: v, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	ps := p.Sessions()[0]
	ps.Info = core.MediaInfo{Container: "mkv", Duration: 10 * time.Minute, Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080}, {Index: 1, Type: "audio", Codec: "aac"}, {Index: 7, Type: "audio", Codec: "aac", Language: "fr"}}}
	tc := &trackOutputTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}}
	b.deps.Transcoder = tc
	bound := BindSurface(b, "jellyfin")
	reader, ok := bound.(frontend.StreamDecisionReader)
	if !ok {
		t.Fatal("surface hides track output decision")
	}
	audio := 7
	access := frontend.SessionAccess{Viewer: v, Device: device, Protocol: "jellyfin"}
	req := frontend.StreamRequest{Format: frontend.StreamHLS, Position: 47 * time.Second, AudioStream: &audio, LazyStart: true, Continuous: true}
	wrong := access
	wrong.Device.ID = "another-device"
	if _, err := bound.StartStream(ctx, wrong, s.ID, req); err == nil {
		t.Fatal("track restart accepted another device")
	}
	if _, err := bound.StartStream(ctx, access, s.ID, req); err != nil {
		t.Fatal(err)
	}
	if tc.last.Decision.AudioStream != 7 || tc.last.StartOffset != 47*time.Second || !tc.last.LazyStart || !tc.last.Continuous {
		t.Fatalf("wrong output request: %+v", tc.last)
	}
	d, err := reader.StreamDecision(ctx, access, s.ID)
	if err != nil || d.AudioStream != 7 {
		t.Fatal("unavailable admitted output decision", d, err)
	}
	old := tc.sessions[s.ID]
	audio = 999
	if _, err := bound.StartStream(ctx, access, s.ID, req); err == nil {
		t.Fatal("unprobed source track accepted")
	}
	if tc.sessions[s.ID] != old {
		t.Fatal("invalid selection replaced existing output")
	}
	caps := core.ClientCaps{Profiles: &core.ClientCapabilityProfiles{Subtitles: []core.ClientSubtitleProfile{{Format: "webvtt", Method: "external"}}}}
	info := ps.Info
	info.Size = 1 << 30
	info.Streams = append(info.Streams, core.StreamInfo{Index: 9, Type: "subtitle", Codec: "srt"}, core.StreamInfo{Index: 10, Type: "subtitle", Codec: "hdmv_pgs_subtitle"})
	if len(progressiveTextTracks("vela", caps, info)) != 0 {
		t.Fatal("ordinary text support disabled direct play")
	}
	caps.Profiles.Subtitles[0].Format = "ether-vtt-window"
	tracks := progressiveTextTracks("vela", caps, info)
	if len(tracks) != 1 || tracks[0] != 9 {
		t.Fatal("cue tee includes image subtitles or omitted explicit text capability", tracks)
	}
}
