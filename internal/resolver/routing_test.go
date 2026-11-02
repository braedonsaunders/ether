package resolver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type cachedSet map[string]bool

func (c cachedSet) KnownCached(h string) bool { return c[h] }

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// routingReleases: a well-grabbed Usenet 1080p leads on score; a torrent of
// the same quality tier sits a few points behind.
func routingReleases(torrentName string) []core.Release {
	u := rel("usenet", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)
	u.Grabs = 5000
	tr := rel("torrent", torrentName, 8000)
	tr.Grabs = 0
	tr.Protocol = core.ProtocolTorrent
	tr.InfoHash = hashA
	return []core.Release{u, tr}
}

func routingTitle(popular bool) *core.Title {
	t := matrixTitle()
	if popular {
		t.Popularity = 120
	}
	return t
}

func resolveOrder(t *testing.T, svc *Service, title *core.Title) []string {
	t.Helper()
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ids := []string{res.Release.ID}
	for _, a := range res.Alternates {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestRoutingPopularTitlePrefersCachedDebridAndHedgesUsenet(t *testing.T) {
	idx := &fakeIndexer{releases: routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	if got := resolveOrder(t, svc, routingTitle(true)); got[0] != "torrent" || got[1] != "usenet" {
		t.Fatalf("order %v, want cached torrent first and usenet as the hedge", got)
	}
	tr := svc.Traces()[0]
	if !strings.Contains(tr.Routing, "debrid first: popular title") {
		t.Fatalf("routing %q", tr.Routing)
	}
	notes := map[string]string{}
	for _, c := range tr.Candidates {
		notes[c.ReleaseID] = c.Route
	}
	if !strings.Contains(notes["torrent"], "cached on debrid") || !strings.HasPrefix(notes["usenet"], "hedge") {
		t.Fatalf("route notes %v", notes)
	}
}

func TestRoutingLeavesOrderAloneWithoutReason(t *testing.T) {
	for name, tc := range map[string]struct {
		popular bool
		cached  bool
	}{
		"unpopular, no measurements":   {popular: false, cached: true},
		"popular but not known cached": {popular: true, cached: false},
	} {
		t.Run(name, func(t *testing.T) {
			idx := &fakeIndexer{releases: routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")}
			svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
			svc.SetSourceSignals(cachedSet{hashA: tc.cached})
			if got := resolveOrder(t, svc, routingTitle(tc.popular)); got[0] != "usenet" {
				t.Fatalf("order %v, want the higher-scored usenet release first", got)
			}
		})
	}
}

func TestRoutingNeverTradesQualityForSpeed(t *testing.T) {
	// The cached torrent is 720p; the Usenet leader is 1080p.
	idx := &fakeIndexer{releases: routingReleases("The.Signal.1999.720p.BluRay.x264-OTHER")}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferDebrid
	svc.UpdateConfig(cfg)
	if got := resolveOrder(t, svc, routingTitle(true)); got[0] != "usenet" {
		t.Fatalf("order %v: a lower quality tier must not lead for speed", got)
	}
}

func TestDebridPreferenceTriesReadyCopyBeforeUnknownAvailability(t *testing.T) {
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferDebrid
	svc := New(nil, fakeHealth{}, nil, cfg, Options{})
	releases := routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")
	unknown := releases[1]
	unknown.ID, unknown.InfoHash, unknown.Score = "unknown", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 110
	releases[0].Score, releases[1].Score = 100, 95
	svc.SetSourceSignals(cachedSet{hashA: true})
	got := svc.routeSources([]candidate{{rel: unknown}, {rel: releases[0]}, {rel: releases[1]}}, routingTitle(false), nil)
	if got[0].rel.ID != "torrent" || got[1].rel.ID != "usenet" {
		t.Fatalf("ready copy did not lead with an independent hedge: %+v", got)
	}
}

func TestExplicitSourcePreferenceOverridesPreviousVerifiedSource(t *testing.T) {
	for _, mode := range []string{PreferAuto, PreferDebrid, PreferUsenet} {
		t.Run(mode, func(t *testing.T) {
			cfg := testCfg()
			cfg.Resolver.SourcePreference = mode
			svc := New(nil, fakeHealth{}, nil, cfg, Options{})
			releases := routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")
			releases[0].Score, releases[1].Score = 100, 95
			cands := []candidate{{rel: releases[0], verified: true}, {rel: releases[1]}}
			got := svc.routeSources(cands, routingTitle(true), nil)
			want := "usenet"
			if mode == PreferDebrid {
				want = "torrent"
			}
			if got[0].rel.ID != want {
				t.Fatalf("source preference %q ignored after previous play: got %s", mode, got[0].rel.ID)
			}
			// The owner preference still cannot promote a lower-resolution input.
			releases[1].Info.Height = 720
			got = svc.routeSources([]candidate{{rel: releases[0], verified: true}, {rel: releases[1]}}, routingTitle(true), nil)
			if got[0].rel.ID != "usenet" {
				t.Fatal("source preference reduced the selected quality tier")
			}
		})
	}
}

func TestRoutingFollowsMeasuredStartup(t *testing.T) {
	feed := func(svc *Service, usenet, debrid time.Duration) {
		for i := 0; i < minStartupSamples; i++ {
			svc.noteStartup(core.Release{}, usenet)
			svc.noteStartup(core.Release{Protocol: core.ProtocolTorrent}, debrid)
		}
	}
	idx := &fakeIndexer{releases: routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")}

	// Debrid measured faster: wins even for an unpopular title.
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	feed(svc, 2*time.Second, 300*time.Millisecond)
	if got := resolveOrder(t, svc, routingTitle(false)); got[0] != "torrent" {
		t.Fatalf("order %v, want measured-faster debrid first", got)
	}
	if r := svc.Traces()[0].Routing; !strings.Contains(r, "measured debrid") {
		t.Fatalf("routing %q", r)
	}

	// Usenet measured faster: keeps the lead even for a popular title.
	svc = New(idx, fakeHealth{}, nil, testCfg(), Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	feed(svc, 300*time.Millisecond, 2*time.Second)
	if got := resolveOrder(t, svc, routingTitle(true)); got[0] != "usenet" {
		t.Fatalf("order %v, want measured-faster usenet first", got)
	}

	// Too close to call: popularity decides.
	svc = New(idx, fakeHealth{}, nil, testCfg(), Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	feed(svc, 500*time.Millisecond, 400*time.Millisecond)
	if got := resolveOrder(t, svc, routingTitle(true)); got[0] != "torrent" {
		t.Fatalf("order %v, want popularity to break a near tie", got)
	}
}

func TestRoutingOwnerPreferenceUsenet(t *testing.T) {
	idx := &fakeIndexer{releases: routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")}
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferUsenet
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	for i := 0; i < minStartupSamples; i++ {
		svc.noteStartup(core.Release{Protocol: core.ProtocolTorrent}, 100*time.Millisecond)
		svc.noteStartup(core.Release{}, 3*time.Second)
	}
	if got := resolveOrder(t, svc, routingTitle(true)); got[0] != "usenet" || got[1] != "torrent" {
		t.Fatalf("order %v, want usenet first with the torrent as hedge", got)
	}
}

func TestStartupMeasuredFromRealChecksOnly(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err != nil {
		t.Fatal(err)
	}
	st := svc.SourceStartups()
	if len(st) != 1 || st[0].Protocol != core.ProtocolUsenet || st[0].Samples != 1 {
		t.Fatalf("startups %+v", st)
	}
	// The second resolution is answered by the verified cache: no sample.
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Exclude: []string{"nope"}}); err != nil {
		t.Fatal(err)
	}
	if st := svc.SourceStartups(); st[0].Samples != 1 {
		t.Fatalf("verified-cache answer was measured: %+v", st)
	}
}

func TestExplicitSourcePreferenceFindsNonAdjacentEqualQuality(t *testing.T) {
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferDebrid
	svc := New(nil, fakeHealth{}, nil, cfg, Options{})
	releases := routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")
	releases[0].Score, releases[1].Score = 100, 95
	lower := releases[0]
	lower.ID = "lower"
	lower.Info.Height = 720
	lower.Score = 80
	cands := []candidate{{rel: releases[0], verified: true}, {rel: lower, verified: true}, {rel: releases[1]}}
	got := svc.routeSources(cands, routingTitle(true), nil)
	if got[0].rel.ID != "torrent" || got[1].rel.ID != "usenet" || got[2].rel.ID != "lower" {
		t.Fatalf("non-adjacent source preference: %+v", got)
	}
	releases[1].Score = 50
	cands[2].rel = releases[1]
	if got = svc.routeSources(cands, routingTitle(true), nil); got[0].rel.ID != "usenet" {
		t.Fatal("source preference exceeded score margin")
	}
}

// "Always" source order: the preferred source goes first even when the other
// source has the better release. A known-cached debrid copy beats a higher
// Usenet release, an uncached torrent does not, and the other source stays a
// fallback. Usenet-always works the same way the other round.
func TestSourceAlwaysFirstBeatsQuality(t *testing.T) {
	releases := routingReleases("The.Signal.1999.720p.WEB-DL.DDP5.1.H.264-OTHER")
	usenet, torrent := releases[0], releases[1]
	usenet.Score, torrent.Score = 100, 40
	torrent.Info.Height = 720
	uncached := torrent
	uncached.ID, uncached.InfoHash = "uncached", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cands := func() []candidate {
		return []candidate{{rel: usenet, langOK: true}, {rel: uncached, langOK: true}, {rel: torrent, langOK: true}}
	}
	cfg := testCfg()
	cfg.Resolver.SourcePreference, cfg.Resolver.SourceAlwaysFirst = PreferDebrid, true
	svc := New(nil, fakeHealth{}, nil, cfg, Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	if got := svc.routeSources(cands(), routingTitle(false), nil); got[0].rel.ID != "torrent" || got[1].rel.ID != "usenet" || got[2].rel.ID != "uncached" {
		t.Fatalf("debrid always: %v %v %v", got[0].rel.ID, got[1].rel.ID, got[2].rel.ID)
	}
	// A language nobody asked for still loses.
	wrong := cands()
	wrong[2].langOK = false
	if got := svc.routeSources(wrong, routingTitle(false), nil); got[0].rel.ID != "usenet" {
		t.Fatalf("debrid always picked a wrong-language release: %v", got[0].rel.ID)
	}

	cfg.Resolver.SourcePreference = PreferUsenet
	svc = New(nil, fakeHealth{}, nil, cfg, Options{})
	flipped := []candidate{{rel: torrent, langOK: true}, {rel: usenet, langOK: true}}
	if got := svc.routeSources(flipped, routingTitle(false), nil); got[0].rel.ID != "usenet" {
		t.Fatalf("usenet always: %v", got[0].rel.ID)
	}
	// Without "always", Debrid first stays a tie-breaker between equals.
	cfg.Resolver.SourcePreference, cfg.Resolver.SourceAlwaysFirst = PreferDebrid, false
	svc = New(nil, fakeHealth{}, nil, cfg, Options{})
	svc.SetSourceSignals(cachedSet{hashA: true})
	if got := svc.routeSources(cands(), routingTitle(false), nil); got[0].rel.ID != "usenet" {
		t.Fatalf("tie-breaker only: %v", got[0].rel.ID)
	}
}
