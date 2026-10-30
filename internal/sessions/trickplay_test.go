package sessions

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/trickplay"
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

func TestBrokerTrickplayLeasesReleaseAndFenceLateEnsure(t *testing.T) {
	ctx := context.Background()
	raw := &memTrickplay{}
	owner := trickplay.New(raw)
	defer owner.Close()
	provider := owner.Borrow()
	play := &fakePlayback{sessions: map[string]*core.PlaySession{"play": {ID: "play", Resolution: &core.Resolution{Release: core.Release{ID: "rel-1"}}}}}
	active := func(context.Context, int64) (*core.Viewer, error) { return &core.Viewer{ID: 7}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	b, err := NewBroker(BrokerDeps{Playback: play, Trickplay: provider, ActiveViewer: active, Events: events, Progress: &fakeProgress{}, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Title, error) {
		return &core.Title{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	s := &brokerSession{public: frontend.Session{ID: "s", ViewerID: 7}, playID: "play"}
	b.sessions["s"] = s
	plex, err := provider.(core.TrickplayLeaser).AcquireTrickplay(ctx, core.TrickplaySource{ReleaseID: "rel-1", InputURL: "plex"})
	if err != nil {
		t.Fatal(err)
	}
	b.ensureTrickplay(ctx, s, time.Second)
	// A source change must acquire the replacement before retiring the old hold.
	play.mu.Lock()
	play.sessions["play"] = &core.PlaySession{ID: "play", Resolution: &core.Resolution{Release: core.Release{ID: "rel-2"}}}
	play.mu.Unlock()
	b.ensureTrickplay(ctx, s, time.Second)
	if len(raw.canceled) != 0 {
		t.Fatal("swap canceled Plex")
	}
	if err := b.stopLocked(ctx, s); err != nil {
		t.Fatal(err)
	}
	if len(raw.canceled) != 1 || raw.canceled[0] != "rel-2" {
		t.Fatal("stop failed to release current lease", raw.canceled)
	}
	n := len(raw.ensured)
	b.ensureTrickplay(ctx, s, time.Second)
	if len(raw.ensured) != n {
		t.Fatal("late Ensure resurrected stopped session")
	}
	plex.Release(true)
	if len(raw.canceled) != 2 || raw.canceled[1] != "rel-1" {
		t.Fatal("old release leaked", raw.canceled)
	}
	if raw.closed {
		t.Fatal("broker closed owner")
	}
}

// The filesystem revision seam blocks before Stat. Stop must not wait for it.
type blockedRevisionSource struct {
	core.MediaSource
	entered, unblock chan struct{}
}

func (*blockedRevisionSource) ReleaseID() string     { return "rel-1" }
func (s *blockedRevisionSource) LocalFile() *os.File { close(s.entered); <-s.unblock; return nil }
func TestTrickplayBlockedRevisionDoesNotBlockStop(t *testing.T) {
	ctx := context.Background()
	src := &blockedRevisionSource{entered: make(chan struct{}), unblock: make(chan struct{})}
	raw := &memTrickplay{}
	owner := trickplay.New(raw)
	defer owner.Close()
	play := &fakePlayback{sessions: map[string]*core.PlaySession{"play": {ID: "play", Source: src, Resolution: &core.Resolution{Release: core.Release{ID: "rel-1"}}}}}
	events := NewEventBus(nil, func(context.Context, int64) (*core.Viewer, error) { return &core.Viewer{ID: 7}, nil })
	defer events.Close()
	s := &brokerSession{public: frontend.Session{ID: "s", ViewerID: 7}, playID: "play", sourcePinned: true, sourceETag: "expected"}
	b := &Broker{deps: BrokerDeps{Trickplay: owner.Borrow(), Playback: play, Now: time.Now, Events: events}, sessions: map[string]*brokerSession{"s": s}, retired: make(map[string]retiredSession), owners: NewPlaybackOwners(), bindings: NewBindingTable[frontend.PlayIntent]()}
	ensured := make(chan struct{})
	go func() { b.ensureTrickplay(ctx, s, time.Second); close(ensured) }()
	<-src.entered
	stopped := make(chan error, 1)
	go func() { stopped <- b.stopLocked(ctx, s) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(src.unblock)
		<-ensured
		t.Fatal("Stop waited for filesystem revision")
	}
	close(src.unblock)
	<-ensured
	if len(raw.ensured) != 0 {
		t.Fatal("blocked revision published after stop")
	}
}
