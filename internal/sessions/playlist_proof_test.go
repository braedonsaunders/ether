package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type proofRecorder struct {
	fakeMediaTranscoder
	last core.HLSRequest
}

func (f *proofRecorder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	f.last = req
	return f.fakeMediaTranscoder.StartHLS(ctx, req)
}

func TestBrokerRequestsPlaylistProofForMediaBrowser(t *testing.T) {
	for _, tc := range []struct {
		protocol frontend.Protocol
		want     bool
	}{{"emby", true}, {"jellyfin", true}, {"plex", false}} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			ctx := context.Background()
			p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}
			tcTranscoder := &proofRecorder{fakeMediaTranscoder: fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}}
			active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
			events := NewEventBus(nil, active)
			defer events.Close()
			b, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: key}, nil
			}, Playback: p, Transcoder: tcTranscoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events})
			if err != nil {
				t.Fatal(err)
			}
			viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
			plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: tc.protocol, Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: tc.protocol, PlanID: plan.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.StartStream(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: tc.protocol}, s.ID, frontend.StreamRequest{Format: frontend.StreamHLS}); err != nil {
				t.Fatal(err)
			}
			if tcTranscoder.last.RequireIndependentProof != tc.want {
				t.Fatalf("proof request = %v, want %v", tcTranscoder.last.RequireIndependentProof, tc.want)
			}
		})
	}
}
