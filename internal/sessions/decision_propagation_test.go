package sessions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// decisionRecordingPlayback wraps fakePlayback and records every decision
// the broker reports for a playback (source) session, the way
// playback.Manager.SetDecision would receive it.
type decisionRecordingPlayback struct {
	*fakePlayback
	mu        sync.Mutex
	decisions map[string][]core.Decision
}

func (f *decisionRecordingPlayback) SetDecision(id string, d core.Decision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decisions == nil {
		f.decisions = map[string][]core.Decision{}
	}
	f.decisions[id] = append(f.decisions[id], d)
}

func (f *decisionRecordingPlayback) lastDecision(id string) (core.Decision, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.decisions[id]
	if len(got) == 0 {
		return core.Decision{}, false
	}
	return got[len(got)-1], true
}

// remuxAudioTranscoder starts a real (fake) HLS session but decides a
// remux that copies the video and transcodes only the audio — the live
// "Starting" case from production.
type remuxAudioTranscoder struct {
	*fakeMediaTranscoder
}

func (remuxAudioTranscoder) Decide(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision {
	return core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: false, AudioCodec: "aac", Container: "mpegts"}
}

func newDecisionBroker(t *testing.T) (*Broker, *decisionRecordingPlayback) {
	t.Helper()
	p := &decisionRecordingPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}}
	tc := remuxAudioTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	t.Cleanup(bus.Close)
	broker, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: tc, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	return broker, p
}

// A broker-started HLS remux must reach the playback session, replacing a
// stale direct-play mark from an earlier byte read. Before the fix the
// broker kept the decision to itself, so Manager.Sessions() showed
// "Starting" (no decision) or the stale "directplay".
func TestBrokerStartStreamRecordsDecisionOnPlayback(t *testing.T) {
	ctx := context.Background()
	broker, p := newDecisionBroker(t)
	v := &core.Viewer{ID: 2}
	device := core.Device{ID: "tv"}
	plan, err := broker.Plan(ctx, frontend.PlanRequest{Viewer: v, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := broker.Open(ctx, frontend.OpenRequest{Viewer: v, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: v, Device: device, Protocol: "jellyfin"}

	// Direct delivery first: the playback session learns it is direct play.
	if _, err := broker.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
		t.Fatal(err)
	}
	playID := p.Sessions()[0].ID
	if dec, ok := p.lastDecision(playID); !ok || dec.Kind != core.DecisionDirectPlay {
		t.Fatalf("direct start decision = %+v (recorded %v), want directplay", dec, ok)
	}

	// Switching the same session to HLS must replace the direct decision
	// with the remux actually started (video copied, audio transcoded).
	if _, err := broker.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
		t.Fatal(err)
	}
	dec, ok := p.lastDecision(playID)
	if !ok {
		t.Fatal("HLS start recorded no decision on the playback session")
	}
	if dec.Kind != core.DecisionRemux || !dec.VideoCopy || dec.AudioCopy {
		t.Fatalf("HLS start decision = %+v, want remux with video copy and audio transcode", dec)
	}
}

// The Plex adapter starts ffmpeg itself and reports through
// NotePlexEncoder; that note must also reach the playback session.
func TestNotePlexEncoderRecordsDecisionOnPlayback(t *testing.T) {
	ctx := context.Background()
	broker, p := newDecisionBroker(t)
	viewer := &core.Viewer{ID: 2}
	ps, err := p.Prepare(ctx, &core.User{ID: 2}, core.MovieKey(1), core.ClientCaps{ClientID: "plex-tv"})
	if err != nil {
		t.Fatal(err)
	}
	plexCtx := WithPlexAdmission(ctx, func(context.Context) (PlexAdmission, error) {
		return PlexAdmission{Source: ps, Media: ps.Info, Caps: core.ClientCaps{ClientID: "plex-tv"}, Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}, nil
	})
	dev := core.Device{ID: "plex-tv"}
	plan, err := broker.Plan(plexCtx, frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: "plex", Item: core.MovieKey(1), Caps: core.ClientCaps{ClientID: "plex-tv"}, Intent: frontend.PlayIntent{ClientSessionID: "plex-1", Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = broker.Open(plexCtx, frontend.OpenRequest{Viewer: viewer, Device: dev, Protocol: "plex", PlanID: plan.ID}); err != nil {
		t.Fatal(err)
	}
	want := core.Decision{Kind: core.DecisionTranscode, VideoCopy: false, AudioCopy: true, Container: "mpegts"}
	broker.NotePlexEncoder("plex-1", frontend.StreamHLS, want, 0)
	dec, ok := p.lastDecision(ps.ID)
	if !ok {
		t.Fatal("NotePlexEncoder recorded no decision on the playback session")
	}
	if dec.Kind != want.Kind || dec.VideoCopy != want.VideoCopy || dec.AudioCopy != want.AudioCopy {
		t.Fatalf("recorded decision = %+v, want %+v", dec, want)
	}
}
