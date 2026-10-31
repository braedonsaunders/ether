package sessions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type offerRevisionPlayback struct{ *localRevisionPlayback }

func (p *offerRevisionPlayback) PrepareSurface(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if exact && release != "local:test" {
		return nil, core.ErrNotAvailable
	}
	ps, err := p.fakePlayback.Prepare(ctx, viewer, key, caps)
	if err == nil {
		ps.Source = p.source
		ps.Resolution = &core.Resolution{Release: core.Release{ID: "local:test"}}
		ps.Info = core.MediaInfo{Container: "mp4", Duration: 10 * time.Minute}
	}
	return ps, err
}

func TestSourceOffersTwoBrokerDetailPlayKeepsExactGeneration(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	path := filepath.Join(t.TempDir(), "source.mp4")
	if err := os.WriteFile(path, []byte("original source"), 0600); err != nil {
		t.Fatal(err)
	}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	makeBroker := func() (*Broker, *offerRevisionPlayback) {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		playback := &offerRevisionPlayback{&localRevisionPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 4}, source: localRevisionFile{f}}}
		events := NewEventBus(nil, active)
		t.Cleanup(events.Close)
		lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: key}, nil
		}
		b, err := NewBroker(BrokerDeps{DB: db, Events: events, Playback: playback, Lookup: lookup, MediaLookup: lookup, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: &fakeProgress{}, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close(ctx) })
		return b, playback
	}
	first, p1 := makeBroker()
	second, p2 := makeBroker()
	viewer := &core.Viewer{ID: 1}
	device := core.Device{ID: "tv"}
	scope := frontend.VersionScope{Viewer: viewer, Device: device, Protocol: "silo"}
	req := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, AudioStream: -1, SubtitleStream: -1}}}
	detail, err := first.DetailPlan(ctx, req)
	if err != nil || detail.DetailSource == "" || detail.ETag == "" {
		t.Fatalf("detail %+v %v", detail, err)
	}
	sourceAPI, err := NewSourceOffers(db, first, "silo")
	if err != nil {
		t.Fatal(err)
	}
	offer, err := sourceAPI.Issue(ctx, scope, frontend.SourceOfferTarget{Item: req.Item, DetailSource: detail.DetailSource, ETag: detail.ETag})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*Broker{first, second} {
		api, err := NewSourceOffers(db, node, "silo")
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := api.Resolve(ctx, scope, offer.ID)
		if err != nil {
			t.Fatal(err)
		}
		play := req
		play.DetailSource = resolved.Target.DetailSource
		play.ExpectedSourceETag = resolved.Target.ETag
		plan, err := node.Plan(ctx, play)
		if err != nil || plan.ETag != detail.ETag {
			t.Fatalf("play %+v %v", plan, err)
		}
		session, err := node.Open(ctx, frontend.OpenRequest{Protocol: "silo", Viewer: viewer, Device: device, PlanID: plan.ID})
		if err != nil {
			t.Fatal(err)
		}
		access := frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}
		if _, err = node.StartStream(ctx, access, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect}); err != nil {
			t.Fatal(err)
		}
		if err = node.MatchDetailSession(ctx, scope, detail.DetailSource, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	p1.mu.Lock()
	firstPrepares := p1.prepares
	p1.mu.Unlock()
	p2.mu.Lock()
	secondPrepares := p2.prepares
	p2.mu.Unlock()
	if firstPrepares != 1 || secondPrepares != 1 {
		t.Fatalf("unexpected prepare/reprobe calls same-node=%d second-node=%d", firstPrepares, secondPrepares)
	}
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	play := req
	play.DetailSource = detail.DetailSource
	play.ExpectedSourceETag = detail.ETag
	if _, err = second.Plan(ctx, play); err == nil {
		t.Fatal("same ID accepted a changed generation")
	}
}
