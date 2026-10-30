package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type localRevisionFile struct{ *os.File }

func (f localRevisionFile) Size() int64         { st, _ := f.Stat(); return st.Size() }
func (localRevisionFile) ReleaseID() string     { return "local:test" }
func (f localRevisionFile) LocalFile() *os.File { return f.File }

type localRevisionPlayback struct {
	*fakePlayback
	source localRevisionFile
}

func (p *localRevisionPlayback) PrepareSurface(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	ps, err := p.fakePlayback.PrepareSurface(ctx, u, key, caps, release, exact)
	if err == nil {
		ps.Source = p.source
		ps.Resolution = &core.Resolution{Release: core.Release{ID: "local:test"}}
	}
	return ps, err
}
func (p *localRevisionPlayback) PinSourceRevision(_ context.Context, id, revision string) error {
	ps, ok := p.Session(id)
	if !ok || core.SourceRevision(ps) != revision {
		return core.ErrSourceChanged
	}
	return nil
}
func (*localRevisionPlayback) UnpinSourceRevision(string) {}

func TestPinnedLocalMediaRejectsFileReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "video.mp4")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	playback := &localRevisionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, source: localRevisionFile{f}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	b, err := NewBroker(BrokerDeps{Events: events, Playback: playback, Lookup: lookup, MediaLookup: lookup, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: &fakeProgress{}, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 2}, core.Device{ID: "tv"}
	req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true}}}
	detail, err := b.DetailPlan(ctx, req)
	if err != nil || detail.ETag == "" {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	req.ExpectedSourceETag = detail.ETag
	plan, err := b.Plan(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: req.Protocol, PlanID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: req.Protocol}
	mediaPath, err := b.StartStream(ctx, access, s.ID, frontend.StreamRequest{Format: frontend.StreamDirect})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	b.ServeMediaFor("jellyfin", w, httptest.NewRequest(http.MethodGet, mediaPath, nil))
	if w.Code != http.StatusNotFound || w.Body.String() != "Error processing request." {
		t.Fatalf("replacement stream: %d %q", w.Code, w.Body.String())
	}
	head := httptest.NewRecorder()
	b.ServeMediaFor("jellyfin", head, httptest.NewRequest(http.MethodHead, mediaPath, nil))
	if head.Code != http.StatusNotFound || head.Body.Len() != 0 {
		t.Fatalf("replacement HEAD: %d %q", head.Code, head.Body.String())
	}
}
