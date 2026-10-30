package sessions

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type memTrickplay struct {
	ensured  []core.TrickplaySource
	canceled []string
	frames   map[string][]byte
	interval time.Duration
	width    int
	closed   bool
}

func (m *memTrickplay) EnsureTrickplay(_ context.Context, src core.TrickplaySource) {
	m.ensured = append(m.ensured, src)
}
func (m *memTrickplay) TrickplayAvailable(id string) bool  { _, ok := m.frames[id]; return ok }
func (m *memTrickplay) TrickplayBIF(string) ([]byte, bool) { return nil, false }
func (m *memTrickplay) TrickplayFrame(id string, _ time.Duration) ([]byte, bool) {
	frame, ok := m.frames[id]
	return frame, ok && len(frame) > 0
}
func (m *memTrickplay) SetTrickplayFallback(string, []byte) {}
func (m *memTrickplay) CancelTrickplay(id string)           { m.canceled = append(m.canceled, id) }
func (m *memTrickplay) PreviewInterval() time.Duration      { return m.interval }
func (m *memTrickplay) PreviewWidth() int                   { return m.width }
func (m *memTrickplay) Close() error {
	m.closed = true
	return nil
}

func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	img.Set(0, 0, color.RGBA{R: 20, G: 40, B: 60, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBorrowedTrickplayRevalidatesBeforeTiles(t *testing.T) {
	frame := tinyJPEG(t)
	provider := &memTrickplay{frames: map[string][]byte{"rel-1": frame}, interval: 10 * time.Second, width: 8}
	item := core.RatingKey(5)
	viewer := &core.Viewer{ID: 7}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	play := &core.PlaySession{ID: "play", Resolution: &core.Resolution{Release: core.Release{ID: "rel-1"}}, Info: core.MediaInfo{Duration: 15 * time.Second}}
	b := &Broker{
		deps: BrokerDeps{
			Trickplay: provider,
			Playback:  &fakePlayback{sessions: map[string]*core.PlaySession{"play": play}},
			ActiveViewer: func(context.Context, int64) (*core.Viewer, error) {
				return viewer, nil
			},
			Lookup: func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: item}, nil
			},
			Now: func() time.Time { return now },
		},
		sessions: map[string]*brokerSession{
			"s": {public: frontend.Session{ID: "s", Protocol: "jellyfin", ViewerID: 7, Device: core.Device{ID: "dev"}, Item: item, StartedAt: now}, playID: "play", exactRelease: "rel-1"},
		},
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: core.Device{ID: "dev"}, Protocol: "jellyfin"}
	offer, ok := b.TrickplayOffer(context.Background(), access, item)
	if !ok || offer.Width != 8 || offer.Height != 4 || offer.Columns != 10 || offer.Count != 2 || offer.Interval != 10*time.Second {
		t.Fatalf("%+v %v", offer, ok)
	}
	sheet, err := b.TrickplaySheet(context.Background(), access, item, 0)
	if err != nil || len(sheet) == 0 {
		t.Fatal(err, len(sheet))
	}
	if _, err := jpeg.Decode(bytes.NewReader(sheet)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.TrickplaySheet(context.Background(), access, item, 1); err == nil {
		t.Fatal("sheet past the thumbnail count")
	}
	if _, ok := b.TrickplayOffer(context.Background(), frontend.SessionAccess{Viewer: &core.Viewer{ID: 9}, Device: access.Device, Protocol: access.Protocol}, item); ok {
		t.Fatal("other viewer received tiles")
	}
	b.sessions["s"].exactRelease = "other"
	if _, ok := b.TrickplayOffer(context.Background(), access, item); ok {
		t.Fatal("stale release received tiles")
	}
	b.sessions["s"].exactRelease = ""
	b.sessions["s"].versionHandle = "version"
	if _, ok := b.TrickplayOffer(context.Background(), access, item); ok {
		t.Fatal("unpinned version received tiles")
	}
	b.sessions["s"].versionHandle = ""
	b.sessions["s"].exactRelease = "rel-1"
	b.sessions["s"].sourcePinned = true
	b.sessions["s"].sourceETag = "src-current"
	if _, ok := b.TrickplayOffer(context.Background(), access, item); ok {
		t.Fatal("stale source generation received tiles")
	}
	b.ensureTrickplay(context.Background(), b.sessions["s"], time.Second)
	if len(provider.ensured) != 0 {
		t.Fatal("stale generation started thumbnail work")
	}
	b.sessions["s"].sourcePinned = false
	b.ensureTrickplay(context.Background(), b.sessions["s"], 5*time.Second)
	if len(provider.ensured) != 1 || provider.ensured[0].ReleaseID != "rel-1" || provider.ensured[0].InputURL == "" {
		t.Fatal(provider.ensured)
	}
	if provider.closed {
		t.Fatal("broker closed the shared provider")
	}
}

func TestBrokerDoesNotCancelSharedTrickplay(t *testing.T) {
	provider := &memTrickplay{frames: map[string][]byte{"rel-1": tinyJPEG(t)}}
	item := core.RatingKey(5)
	viewer := &core.Viewer{ID: 7}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	play := &core.PlaySession{ID: "play", Resolution: &core.Resolution{Release: core.Release{ID: "rel-1"}}, Info: core.MediaInfo{Duration: 15 * time.Second}}
	b := &Broker{
		deps: BrokerDeps{
			Trickplay: provider,
			Playback:  &fakePlayback{sessions: map[string]*core.PlaySession{"play": play, "plex": {ID: "plex", Resolution: &core.Resolution{Release: core.Release{ID: "rel-1"}}}}},
			ActiveViewer: func(context.Context, int64) (*core.Viewer, error) {
				return viewer, nil
			},
			Lookup: func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
				return &core.Title{Key: item}, nil
			},
			Now: func() time.Time { return now },
		},
		sessions: map[string]*brokerSession{
			"s": {public: frontend.Session{ID: "s", Protocol: "jellyfin", ViewerID: 7, Device: core.Device{ID: "dev"}, Item: item, StartedAt: now}, playID: "play", exactRelease: "rel-1"},
		},
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: core.Device{ID: "dev"}, Protocol: "jellyfin"}
	if _, ok := b.TrickplayOffer(context.Background(), access, item); !ok {
		t.Fatal("offer")
	}
	b.ensureTrickplay(context.Background(), b.sessions["s"], time.Second)
	delete(b.sessions, "s")
	if len(provider.canceled) != 0 || provider.closed {
		t.Fatal("broker canceled or closed the shared provider", provider.canceled, provider.closed)
	}
}
