package resolver

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Broadcast selection must never promote a verified REMUX above a source
// that can actually feed the clock; throughput never relaxes the envelope.
func TestChannelSourceAdmission(t *testing.T) {
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, testCfg(), Options{})
	title := matrixTitle()
	caps := webCaps()
	caps.BroadcastBitrateKbps = 6000
	caps.MaxBitrateKbps = 6000
	small := rel("small", "The.Signal.1999.1080p.WEB-DL.H.264-GRP", 3000)
	heavy := rel("remux", "The.Signal.1999.1080p.BluRay.REMUX.H.264-GRP", 39000)
	if !svc.ChannelSourceAllowed(small, caps, title) || svc.ChannelSourceAllowed(heavy, caps, title) {
		t.Fatal("unmeasured sources did not respect rendition budget")
	}
	sample := func(r core.Release, rate float64) {
		svc.delivery.samples = map[string]deliverySample{deliveryKey(r): {rate: rate, at: svc.now(), count: 3}}
	}
	sample(heavy, 2_000_000)
	if svc.ChannelSourceAllowed(heavy, caps, title) {
		t.Fatal("REMUX slower than real time admitted")
	}
	sample(heavy, 10_000_000)
	if svc.ChannelSourceAllowed(heavy, caps, title) {
		t.Fatal("measured headroom admitted a forbidden remux")
	}
	sample(small, 100_000)
	if svc.ChannelSourceAllowed(small, caps, title) {
		t.Fatal("known slow small source admitted")
	}
	svc.delivery.samples[deliveryKey(small)] = deliverySample{rate: 100_000, at: svc.now().Add(-3 * time.Minute), count: 3}
	if !svc.ChannelSourceAllowed(small, caps, title) {
		t.Fatal("stale delivery poisoned selection")
	}

	t.Run("substitution requires fresh verified budget-fitting cached source", func(t *testing.T) {
		idx := &fakeIndexer{}
		svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
		ctx := context.Background()
		small.Protocol = core.ProtocolTorrent
		small.InfoHash = "cached-film"
		small.DebridCached = []string{"torbox"}
		svc.rememberSearchResults(searchRequestFor(title), []core.Release{heavy, small})
		req := core.ResolveRequest{Title: title, Client: caps}
		if _, ok := svc.ReadyChannelRelease(ctx, req); ok {
			t.Fatal("unverified source marked ready")
		}
		svc.markVerified(ctx, small.ID)
		svc.markVerified(ctx, heavy.ID)
		got, ok := svc.ReadyChannelRelease(ctx, req)
		if !ok || got.ID != small.ID {
			t.Fatalf("ready source=%s ok=%v", got.ID, ok)
		}
		svc.verified[small.ID] = svc.now().Add(-6 * time.Minute)
		if _, ok := svc.ReadyChannelRelease(ctx, req); ok {
			t.Fatal("stale health evidence admitted substitute")
		}
		svc.markVerified(ctx, small.ID)
		req.Prefer = small.ID
		if _, err := svc.ResolveReadyChannel(ctx, req); err != nil {
			t.Fatal(err)
		}
		if idx.searches.Load() != 0 {
			t.Fatal("substitution spent discovery API calls")
		}
	})
}
