package sessions

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestScopedSubtitleExtractionAndSidecars(t *testing.T) {
	ctx := context.Background()
	p := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 1}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, Playback: p, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: events,
		OpenSubtitle: func(id, format string) (io.ReadCloser, string, error) {
			if id != "ether-subtitle:cached" {
				return nil, "", core.ErrNotFound
			}
			return io.NopCloser(strings.NewReader("sidecar")), format, nil
		},
		ExtractSubtitle: func(_ context.Context, input string, index int, codec string, w io.Writer) error {
			if !strings.HasPrefix(input, "http://loopback/") || index != 3 || codec != "srt" {
				t.Fatal("wrong extraction input")
			}
			_, err := io.WriteString(w, "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHello\n")
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.Viewer{ID: 2}
	device := core.Device{ID: "tv"}
	plan, err := b.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	for _, ps := range p.sessions {
		ps.Info.Streams = []core.StreamInfo{{Index: 3, Type: "subtitle", Codec: "srt"}, {Index: 4, Type: "subtitle", Codec: "srt", External: true, ExternalURL: "ether-subtitle:cached"}, {Index: 5, Type: "subtitle", Codec: "pgs"}, {Index: 6, Type: "subtitle", Codec: "srt", External: true, ExternalURL: "https://secret.invalid/?token=credential"}}
	}
	p.mu.Unlock()
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "jellyfin"}
	path, err := b.SubtitleURL(ctx, access, session.ID, 3, "vtt")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.ServeMedia(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	if got := get(path); got.Code != 200 || !strings.Contains(got.Body.String(), "WEBVTT") {
		t.Fatalf("embedded %d %q", got.Code, got.Body.String())
	}
	srt, err := b.SubtitleURL(ctx, access, session.ID, 3, "srt")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(srt); got.Code != 200 || !strings.Contains(got.Body.String(), "00:00:01,000") {
		t.Fatalf("srt %d %q", got.Code, got.Body.String())
	}
	sidecar, err := b.SubtitleURL(ctx, access, session.ID, 4, "vtt")
	if err != nil {
		t.Fatal(err)
	}
	if got := get(sidecar); got.Code != 200 || got.Body.String() != "sidecar" {
		t.Fatalf("sidecar %d %q", got.Code, got.Body.String())
	}
	for _, index := range []int{5, 6, 99} {
		if _, err := b.SubtitleURL(ctx, access, session.ID, index, "vtt"); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("index %d: %v", index, err)
		}
	}
	if _, err := b.SubtitleURL(ctx, frontend.SessionAccess{Viewer: &core.Viewer{ID: 3}, Device: device, Protocol: "jellyfin"}, session.ID, 3, "vtt"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("foreign viewer: %v", err)
	}
	wrong := httptest.NewRecorder()
	BindSurface(b, "emby").ServeMedia(wrong, httptest.NewRequest(http.MethodGet, path, nil))
	if wrong.Code != 404 {
		t.Fatal("cross-surface subtitle")
	}
	if strings.Contains(path, "credential") || strings.Contains(sidecar, "ether-subtitle") {
		t.Fatal("upstream credential leaked")
	}
	if err := b.Stop(ctx, access, session.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(path); got.Code != 404 {
		t.Fatal("stopped grant remained valid")
	}
}
