package sessions

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// readinessTranscoder reports a fixed extraction-cache state per stream index.
// The map is shared so a test can flip an entry after the broker is built.
type readinessTranscoder struct {
	fakeBrokerTranscoder
	cached map[int]bool
}

func (r readinessTranscoder) CachedSubtitle(_ context.Context, index int, _ string) ([]byte, bool) {
	if r.cached[index] {
		return []byte("WEBVTT\n"), true
	}
	return nil, false
}

// SubtitleSidecarReady answers whether serving a track is a cache read (or an
// external fetch) rather than an extraction that walks the whole source. A
// frontend uses it to decide whether it can afford to make its client wait.
func TestSubtitleSidecarReadyDistinguishesCheapFromWholeSourceReads(t *testing.T) {
	ctx := context.Background()
	cached := map[int]bool{}
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{
		SurfacePolicy: testSessionPolicy,
		Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: k}, nil
		},
		Playback:     p,
		Transcoder:   readinessTranscoder{cached: cached},
		Progress:     &fakeProgress{},
		ActiveViewer: active,
		CanPlay:      func(*core.Viewer, bool, time.Time) bool { return true },
		BitrateLimit: func(*core.Viewer) int { return 0 },
		OnTimeline:   func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {},
		Events:       events,
		OpenSubtitle: func(id, format string) (io.ReadCloser, string, error) {
			return io.NopCloser(nil), format, nil
		},
		ExtractSubtitle: func(context.Context, string, int, string, io.Writer) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	device := core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	for _, ps := range p.sessions {
		ps.Info.Streams = []core.StreamInfo{
			{Index: 3, Type: "subtitle", Codec: "srt"},
			{Index: 4, Type: "subtitle", Codec: "srt", External: true, ExternalURL: "ether-subtitle:cached"},
			{Index: 5, Type: "subtitle", Codec: "pgs"},
		}
	}
	p.mu.Unlock()
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}

	for _, tc := range []struct {
		name  string
		index int
		want  bool
	}{
		// An embedded track that is not extracted yet costs a walk over the
		// source, and this client declares no progressive subtitle profile.
		{"embedded uncached", 3, false},
		// External tracks come from the subtitle provider, not the source.
		{"external", 4, true},
		// Image subtitles cannot be served as a text sidecar at all.
		{"image", 5, false},
		// An unknown index is not a deliverable track.
		{"unknown", 99, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.SubtitleSidecarReady(ctx, access, session.ID, tc.index); got != tc.want {
				t.Fatalf("SubtitleSidecarReady(index=%d) = %v, want %v", tc.index, got, tc.want)
			}
		})
	}

	// Once the text is extracted, serving it is a cache read.
	cached[3] = true
	if !b.SubtitleSidecarReady(ctx, access, session.ID, 3) {
		t.Fatal("extracted track must be reported ready")
	}

	// The predicate authorizes; another viewer must not learn anything from it.
	other := frontend.SessionAccess{Viewer: &core.Viewer{ID: 3}, Device: device, Protocol: "silo"}
	if b.SubtitleSidecarReady(ctx, other, session.ID, 3) {
		t.Fatal("readiness must not be answered for an unrelated viewer")
	}
}
