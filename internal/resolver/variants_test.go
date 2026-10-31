package resolver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// primeSearchCache runs one resolve so the search results are cached, the
// state a detail page's PreResolve creates before Variants is asked.
func primeSearchCache(t *testing.T, svc *Service, caps core.ClientCaps) {
	t.Helper()
	if _, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: caps}); err != nil {
		t.Fatalf("prime resolve: %v", err)
	}
}

func variantReleases() []core.Release {
	return []core.Release{
		rel("uhd", "The.Signal.1999.2160p.UHD.BluRay.REMUX.DV.HEVC.TrueHD.7.1-GRP", 50000),
		rel("uhd2", "The.Signal.1999.2160p.WEB-DL.DV.HDR.H.265-GRP", 20000), // same tier as uhd
		rel("hd", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000),
		rel("hdhevc", "The.Signal.1999.1080p.BluRay.x265.DTS-HD.MA.5.1-GRP", 12000),
		rel("dc", "The.Signal.1999.Directors.Cut.1080p.BluRay.x264-GRP", 9000),
		rel("sd720", "The.Signal.1999.720p.BluRay.x264-GRP", 4000),
	}
}

func TestVariantsCacheOnlyNoIndexerCalls(t *testing.T) {
	idx := &fakeIndexer{releases: variantReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()

	// Nothing cached yet: no variants, and no indexer search spent.
	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil || len(vs) != 0 {
		t.Fatalf("uncached variants = %+v err=%v", vs, err)
	}
	if idx.searches.Load() != 0 {
		t.Fatalf("Variants spent indexer budget: searches=%d", idx.searches.Load())
	}

	primeSearchCache(t, svc, appleTVCaps())
	before := idx.searches.Load()
	vs, err = svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if idx.searches.Load() != before {
		t.Fatalf("Variants searched again: %d -> %d", before, idx.searches.Load())
	}
	if len(vs) == 0 {
		t.Fatal("expected variants from cache")
	}
}

func TestVariantsTiersEditionsAndLabels(t *testing.T) {
	idx := &fakeIndexer{releases: variantReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()
	primeSearchCache(t, svc, appleTVCaps())

	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) > 4 {
		t.Fatalf("too many variants: %d", len(vs))
	}
	// Auto first, and it is the release normal resolution picks.
	if !vs[0].Auto || vs[0].Release.ID != "uhd" || vs[0].Label != "Auto / Best for Device" {
		t.Fatalf("auto variant %+v", vs[0])
	}
	// No duplicate releases; a (cut, tier) repeats only as one of Auto's
	// same-quality alternatives (ETHE-5 C), which come right after Auto.
	seenKey := map[string]bool{}
	seenRel := map[string]bool{}
	for _, v := range vs {
		if seenRel[v.Release.ID] {
			t.Errorf("duplicate release %q", v.Release.ID)
		}
		seenRel[v.Release.ID] = true
		k := variantKey(v.Release.Info)
		if seenKey[k] && k != variantKey(vs[0].Release.Info) {
			t.Errorf("duplicate variant key %q", k)
		}
		seenKey[k] = true
		if v.ID != v.Release.ID || v.Label == "" {
			t.Errorf("variant id/label %+v", v)
		}
	}
	// The Director's Cut is offered as its own version.
	var dc *core.ReleaseVariant
	for i := range vs {
		if vs[i].Edition == "Director's Cut" {
			dc = &vs[i]
		}
	}
	if dc == nil || dc.Release.ID != "dc" {
		t.Fatalf("director's cut variant missing: %+v", vs)
	}
	if !strings.HasPrefix(dc.Label, "Director's Cut") {
		t.Errorf("edition label %q", dc.Label)
	}
	// Estimated info is filled in.
	info := vs[0].Info
	if !info.Estimated || info.Video() == nil || info.Video().Height != 2160 || !info.Video().DolbyVision || info.BitrateKbps <= 0 {
		t.Errorf("auto info %+v", info)
	}
	// The same-tier 4K WEB-DL is Auto's alternative, second in the list.
	if len(vs) < 2 || vs[1].Release.ID != "uhd2" || vs[1].Auto {
		t.Errorf("same-tier alternative not second: %+v", vs)
	}
}

func TestVariantsCapabilityGating(t *testing.T) {
	idx := &fakeIndexer{releases: variantReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()
	primeSearchCache(t, svc, appleTVCaps())

	// A 1080p SDR H.264-only client gets no 4K/DV/HEVC versions.
	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 || vs[0].Release.ID != "hd" || !vs[0].Auto {
		t.Fatalf("web variants %+v", vs)
	}
	for _, v := range vs {
		if v.Release.Info.Height > 1080 || v.Release.Info.DolbyVision || v.Release.Info.VideoCodec == "hevc" {
			t.Errorf("unplayable variant offered to web client: %+v", v.Release.Info)
		}
	}
	// The 4K-capable client does get a 4K version.
	vs, err = svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil {
		t.Fatal(err)
	}
	has4K := false
	for _, v := range vs {
		if v.Release.Info.Height >= 2160 {
			has4K = true
		}
	}
	if !has4K {
		t.Errorf("no 4K variant for Apple TV: %+v", vs)
	}
}

func TestVariantsEpisode(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{
		rel("ep4k", "Boundary.Bay.S01E01.2160p.WEB-DL.DV.HEVC-GRP", 8000),
		rel("ephd", "Boundary.Bay.S01E01.1080p.WEB-DL.DDP5.1.H.264-GRP", 3000),
		rel("ep720", "Boundary.Bay.S01E01.720p.HDTV.x264-GRP", 1500),
	}}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()
	title := &core.Title{
		Key: core.EpisodeKey(1396, 1, 1), Type: core.TypeEpisode,
		ShowTitle: "Boundary Bay", Season: 1, Episode: 1, Runtime: 49 * time.Minute,
		ShowIDs: core.IDs{TVDB: 81189},
	}
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()}); err != nil {
		t.Fatal(err)
	}
	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()}, 4)
	if err != nil || len(vs) < 2 {
		t.Fatalf("episode variants %+v err=%v", vs, err)
	}
	if !vs[0].Auto {
		t.Errorf("episode auto not first: %+v", vs[0])
	}
}

func TestVariantsMaxAndBadReleasesExcluded(t *testing.T) {
	idx := &fakeIndexer{releases: variantReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()
	primeSearchCache(t, svc, appleTVCaps())

	vs, err := svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 2)
	if err != nil || len(vs) != 2 {
		t.Fatalf("max=2 variants %+v err=%v", vs, err)
	}
	if err := svc.MarkBad(ctx, "uhd", "broken"); err != nil {
		t.Fatal(err)
	}
	vs, err = svc.Variants(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Release.ID == "uhd" {
			t.Errorf("marked-bad release offered as variant")
		}
	}
	if len(vs) == 0 || !vs[0].Auto {
		t.Fatalf("auto missing after MarkBad: %+v", vs)
	}
}

func TestResolvePreferWinsOverRanking(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()

	// Normal ranking picks uhd; the explicit pick must win.
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: "sd720"})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("prefer res=%+v err=%v", res, err)
	}
	if len(res.Alternates) == 0 {
		t.Error("alternates kept empty on preferred resolve")
	}
	// A 4K pick on the 1080p web profile: the profile height gate must not
	// reject the viewer's explicit choice (HEVC would be a hard client
	// gate, so use the 1080p profile with an H.264-capable 4K client via a
	// remote Apple TV which selects the 1080p profile).
	remote := appleTVCaps()
	remote.Remote = true
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: remote, Prefer: "uhd"})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("prefer above profile height res=%+v err=%v", res, err)
	}
}

func TestResolvePreferFallsBackWhenUnhealthyOrMissing(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{unhealthy: map[string]bool{"sd720": true}}, nil, testCfg(), Options{})
	ctx := context.Background()

	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: "sd720"})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("unhealthy prefer res=%+v err=%v", res, err)
	}
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: "no-such-release"})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("missing prefer res=%+v err=%v", res, err)
	}
}

func TestResolvePreferAlternatesPreferSameEdition(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{
		rel("uhd", "The.Signal.1999.2160p.UHD.BluRay.REMUX.DV.HEVC.TrueHD.7.1-GRP", 50000),
		rel("dc1080web", "The.Signal.1999.Directors.Cut.1080p.WEB-DL.x264-GRP", 5000),
		rel("dc", "The.Signal.1999.Directors.Cut.1080p.BluRay.x264-GRP", 9000),
		rel("hd", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000),
	}}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: "dc"})
	if err != nil || res.Release.ID != "dc" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(res.Alternates) == 0 || res.Alternates[0].ID != "dc1080web" {
		t.Fatalf("alternates should fail over within the cut first: %+v", res.Alternates)
	}
}

func TestEstimateVariantInfo(t *testing.T) {
	title := matrixTitle()
	r := rel("x", "ignored", 8000)
	r.Info.Height = 1080
	r.Info.VideoCodec = "h264"
	r.Info.AudioCodec = "eac3"
	r.Info.Channels = "5.1"
	info := estimateVariantInfo(title, r)
	if info.Video() == nil || info.Video().Width != 1920 || info.Video().Height != 1080 {
		t.Fatalf("video %+v", info.Video())
	}
	// 8000 MiB over 136 min ≈ 8235 kbps total.
	if info.BitrateKbps < 7500 || info.BitrateKbps > 9000 {
		t.Errorf("bitrate %d", info.BitrateKbps)
	}
	if info.Duration != title.Runtime || info.Size != r.Size || !info.Estimated || info.Container != "mkv" {
		t.Errorf("info %+v", info)
	}
	audio := info.Streams[1]
	if audio.Channels != 6 || audio.Codec != "eac3" {
		t.Errorf("audio %+v", audio)
	}
}

func TestVariantsDoNotWaitForInFlightPlaybackPreparation(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases(), delay: time.Hour}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolved := make(chan struct{})
	defer func() {
		cancel()
		select {
		case <-resolved:
		case <-time.After(5 * time.Second):
			t.Error("cancelled preparation did not end")
		}
	}()
	go func() {
		defer close(resolved)
		_, _ = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for idx.searches.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("playback preparation never started")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	vs, err := svc.Variants(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}, 4)
	if err != nil || len(vs) != 0 {
		t.Fatalf("cold version picker: %+v %v", vs, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("version picker waited for source resolution")
	}
	if idx.searches.Load() != 1 {
		t.Fatal("rendering started another indexer search")
	}
}

// ETHE-5 C: Auto first, then up to two same-cut/resolution/HDR alternatives
// in rank order, then today's tiers, with a label that tells the releases
// apart. Names are real scene-style episode releases.
func TestPickVariantsSameQualityAlternatives(t *testing.T) {
	ep := &core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, Runtime: 45 * time.Minute}
	mk := func(id, name string, sizeMB int64) candidate {
		return candidate{rel: withParsed(rel(id, name, sizeMB))}
	}
	pool := []candidate{
		mk("auto", "Show.S01E01.1080p.WEB-DL.DDP5.1.H.264-NTb", 2300),
		mk("dupe", "Show.S01E01.1080p.WEB-DL.DDP5.1.H.264-NTb", 2300), // same file from another source
		mk("flux", "Show.S01E01.1080p.WEB-DL.DDP5.1.H.264-FLUX", 2400),
		mk("edith", "Show.S01E01.1080p.WEB-DL.AAC2.0.H.264-EDITH", 1600),
		mk("third", "Show.S01E01.1080p.WEB-DL.AAC2.0.H.264-KiNGS", 1500), // over the cap of two
		mk("hevc4k", "Show.S01E01.2160p.WEB-DL.DV.HDR.H.265-GRP", 9000),
		mk("hd720", "Show.S01E01.720p.WEB-DL.H.264-GRP", 900),
	}
	caps := core.ClientCaps{MaxHeight: 2160}
	profile := core.QualityProfile{MaxHeight: 2160, AllowHDR: true, AllowDolbyVision: true}
	out := pickVariants(ep, caps, profile, pool[0].rel, pool, pool, 2, maxVariants)
	var ids []string
	for _, v := range out {
		ids = append(ids, v.ID)
	}
	want := []string{"auto", "flux", "edith", "hevc4k", "hd720"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("versions %v, want %v", ids, want)
	}
	if !out[0].Auto || out[1].Auto || out[2].Auto {
		t.Errorf("only the first version is Auto: %+v", out)
	}
	if got := out[1].Label; got != "1080p WEB-DL · DD+ 5.1 · 2.3 GB · FLUX" {
		// 2400 MB is 2.34 GB, which rounds to 2.3
		t.Errorf("alternative label %q", got)
	}
	if got := out[2].Label; got != "1080p WEB-DL · AAC 2.0 · 1.6 GB · EDITH" {
		t.Errorf("alternative label %q", got)
	}
	// A smaller cap never drops a tier to make room for an alternative.
	small := pickVariants(ep, caps, profile, pool[0].rel, pool, pool, 2, 4)
	var got []string
	for _, v := range small {
		got = append(got, v.ID)
	}
	if strings.Join(got, ",") != "auto,flux,hevc4k,hd720" {
		t.Errorf("max=4 versions %v", got)
	}
	// A pool where only Auto and its copy exist yields no alternatives.
	if out := pickVariants(ep, caps, profile, pool[0].rel, pool[:2], pool[:2], 2, maxVariants); len(out) != 1 {
		t.Errorf("duplicate offered as alternative: %+v", out)
	}
}

// ETHE-5 C: alternatives come from the strict profile pool, so a 4K release
// on a 1080p profile is never an alternative, and rendering stays cache-only.
func TestVariantsAlternativesFromStrictPoolCacheOnly(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{
		rel("a", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-TOP", 8000),
		rel("b", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-FLUX", 7000),
		rel("c", "The.Signal.1999.1080p.WEB-DL.AAC2.0.H.264-EDITH", 6000),
		rel("d", "The.Signal.1999.1080p.WEB-DL.AAC2.0.H.264-KiNGS", 5000),
		rel("uhd", "The.Signal.1999.2160p.WEB-DL.DV.HDR.H.265-GRP", 20000),
	}}
	cfg := testCfg()
	cfg.Resolver.SameQualityAlternatives = 2
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	primeSearchCache(t, svc, webCaps())
	before := idx.searches.Load()
	vs, err := svc.Variants(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}, core.MaxVariants)
	if err != nil {
		t.Fatal(err)
	}
	if idx.searches.Load() != before {
		t.Fatalf("Variants spent indexer budget")
	}
	if len(vs) < 3 || !vs[0].Auto {
		t.Fatalf("versions %+v", vs)
	}
	alts := 0
	for _, v := range vs[1:] {
		if v.Release.Info.Height == 2160 {
			t.Errorf("4K release offered on a 1080p profile: %+v", v)
		}
		if variantKey(v.Release.Info) == variantKey(vs[0].Release.Info) {
			alts++
		}
	}
	if alts != 2 {
		t.Errorf("%d alternatives, want 2: %+v", alts, vs)
	}
}

// The owner chooses how many same-quality alternatives are listed (0-3);
// the picker never exceeds core.MaxVariants.
func TestVariantsAlternativeCountSetting(t *testing.T) {
	releases := []core.Release{
		rel("a", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-TOP", 8000),
		rel("b", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-FLUX", 7000),
		rel("c", "The.Signal.1999.1080p.WEB-DL.AAC2.0.H.264-EDITH", 6000),
		rel("d", "The.Signal.1999.1080p.WEB-DL.AAC2.0.H.264-KiNGS", 5000),
		rel("e", "The.Signal.1999.1080p.WEB-DL.AAC2.0.H.264-NTb", 4000),
	}
	for _, tc := range []struct{ setting, want int }{{0, 0}, {1, 1}, {3, 3}, {9, 3}, {-2, 0}} {
		cfg := testCfg()
		cfg.Resolver.SameQualityAlternatives = tc.setting
		svc := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
		primeSearchCache(t, svc, webCaps())
		vs, err := svc.Variants(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}, core.MaxVariants)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, v := range vs[1:] {
			if variantKey(v.Release.Info) == variantKey(vs[0].Release.Info) {
				got++
			}
		}
		if got != tc.want || len(vs) > core.MaxVariants {
			t.Errorf("setting %d: %d alternatives (want %d) in %d versions", tc.setting, got, tc.want, len(vs))
		}
		svc.Close()
	}
}
