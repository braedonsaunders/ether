package plex

import (
	"bytes"
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"reflect"
	"testing"
	"time"
)

func (s *Server) controlLegacyDiscoveryHubs(ctx context.Context, pr *Request, limit int) []hubSpec {
	d := s.d.Discovery
	if d == nil || pr.User == nil {
		return nil
	}
	var out []hubSpec
	add := func(hubs []core.Hub) {
		for _, h := range hubs {
			h.Items = s.visibleTitles(pr.User, h.Items)
			if len(h.Items) > 0 {
				out = append(out, hubSpec{Hub: h})
			}
		}
	}
	if d.Watchlist != nil {
		h, _ := d.Watchlist.Hubs(ctx, pr.User, limit)
		add(h)
	}
	if d.Personal != nil {
		h, _ := d.Personal.Hubs(ctx, pr.User, limit)
		add(h)
	}
	return out
}

func TestDiscoveryLegacyControlRealWatchlist(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	defer f.srv.Close()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	watch := watchlist.New(db, watchlist.Options{Catalog: f.catalog, Now: func() time.Time { return fixedNow }})
	if err = watch.SaveSettings(ctx, watchlist.Settings{Enabled: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	if err = watch.Remind(ctx, &ownerUser, fightClub.Key); err != nil {
		t.Fatal(err)
	}
	f.srv.d.Discovery = &DiscoveryFeatures{Watchlist: watch}
	pr := &Request{User: &ownerUser, Format: model.JSON}
	for _, limit := range []int{0, 1, 10} {
		want := f.srv.controlLegacyDiscoveryHubs(ctx, pr, limit)
		got := f.srv.discoveryHubs(ctx, pr, limit)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("limit=%d legacy=%+v shared=%+v", limit, want, got)
		}
		legacy := hubContainer()
		legacy.Hub = f.srv.renderHubs(ctx, pr, func() []core.Hub {
			out := make([]core.Hub, len(want))
			for i, h := range want {
				out[i] = h.Hub
			}
			return out
		}(), hubCtx{})
		legacy.Size = len(legacy.Hub)
		shared := hubContainer()
		shared.Hub = f.srv.renderHubs(ctx, pr, func() []core.Hub {
			out := make([]core.Hub, len(got))
			for i, h := range got {
				out[i] = h.Hub
			}
			return out
		}(), hubCtx{})
		shared.Size = len(shared.Hub)
		a, ae := model.Marshal(legacy, model.JSON)
		b, be := model.Marshal(shared, model.JSON)
		if ae != nil || be != nil || !bytes.Equal(a, b) {
			t.Fatalf("legacy=%s %v shared=%s %v", a, ae, b, be)
		}
	}
	// The legacy boundary ignores source errors. A closed backing store forces
	// the real source Settings lookup to fail for both paths.
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	want := f.srv.controlLegacyDiscoveryHubs(ctx, pr, 5)
	got := f.srv.discoveryHubs(ctx, pr, 5)
	if !reflect.DeepEqual(want, got) || len(got) != 0 {
		t.Fatalf("source error legacy=%+v shared=%+v", want, got)
	}
}
