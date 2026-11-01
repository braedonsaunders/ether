package sessions

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDetailSourceConcurrentScopeAndExpiry(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	now := time.Now()
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	b := &Broker{deps: BrokerDeps{DB: db, Now: func() time.Time { return now }, Lookup: lookup, ActiveViewer: func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }}}
	req := frontend.PlanRequest{Viewer: &core.Viewer{ID: 2}, Device: core.Device{ID: "tv"}, Protocol: "jellyfin", Item: core.MovieKey(1)}
	ps := &core.PlaySession{Resolution: &core.Resolution{Release: core.Release{ID: "private-release"}}}
	const n = 8
	results := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := b.issueDetailSource(ctx, req, ps, "src-revision")
			results <- id
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var handle string
	for id := range results {
		if handle != "" && id != handle {
			t.Fatal("concurrent issuance returned different handles")
		}
		handle = id
	}
	if handle == "" {
		t.Fatal("missing handle")
	}
	scope := frontend.VersionScope{Viewer: req.Viewer, Device: req.Device, Protocol: req.Protocol}
	target, err := b.ResolveDetailSource(ctx, scope, handle)
	if err != nil || target.Item != req.Item || target.ETag != "src-revision" {
		t.Fatalf("target: %+v %v", target, err)
	}
	wrong := scope
	wrong.Device.ID = "other"
	if _, err := b.ResolveDetailSource(ctx, wrong, handle); !errors.Is(err, frontend.ErrSourceGone) {
		t.Fatalf("foreign device: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := b.ResolveDetailSource(ctx, scope, handle); !errors.Is(err, frontend.ErrSourceGone) {
		t.Fatalf("expired: %v", err)
	}
}
