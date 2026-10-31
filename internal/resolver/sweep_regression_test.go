package resolver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

func withParsed(r core.Release) core.Release {
	r.Info = releaseparse.Parse(r.Name)
	return r
}

// A size outside one profile's range must not mark the title unavailable for
// every other profile (here: a download request against a remux-only title).
func TestProfileSizeRejectionDoesNotCacheUnavailable(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{
		rel("remux", "The.Signal.1999.1080p.BluRay.REMUX.AVC-GRP", 30<<10),
	}}
	s := New(idx, nil, nil, testCfg(), Options{})
	defer s.Close()
	dl := core.ClientCaps{Download: true, VideoCodecs: []string{"h264"}}
	if _, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: dl}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("download resolve err = %v", err)
	}
	if known, _ := s.Available(context.Background(), matrixTitle().Key); known {
		t.Fatal("a download profile's size ceiling cached the title as unavailable")
	}
	if _, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}); err != nil {
		t.Fatalf("4K client resolve: %v", err)
	}
}

// The absolute floor applies even when the profile sets no per-minute range.
func TestImplausiblySmallRejectedWithRuntime(t *testing.T) {
	cfg := testCfg()
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].MinSizeMBPerMin = 0
	}
	s := New(&fakeIndexer{}, nil, nil, cfg, Options{})
	defer s.Close()
	r := rel("tiny", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 1)
	profile := s.ProfileFor(appleTVCaps(), nil)
	reason := s.rejectReason(withParsed(r), matrixTitle(), searchRequestFor(matrixTitle()), profile, s.resolverCfg(), appleTVCaps(), nil, false)
	if reason != "implausibly small" {
		t.Fatalf("reason %q", reason)
	}
}

// An uncapped profile must not let height swamp every other preference.
func TestUncappedProfileHeightScoreIsBounded(t *testing.T) {
	s := New(&fakeIndexer{}, nil, nil, testCfg(), Options{})
	defer s.Close()
	p := core.QualityProfile{Name: "any"}
	hi := s.score(withParsed(rel("a", "The.Signal.1999.2160p.WEB-DL.H264-GRP", 0)), p, matrixTitle())
	lo := s.score(withParsed(rel("b", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 0)), p, matrixTitle())
	if hi-lo <= 0 || hi-lo > 50 {
		t.Fatalf("height gap %v", hi-lo)
	}
}

func TestRemuxQualityModifierMatchesTrashValue(t *testing.T) {
	defs, err := ImportCustomFormats([]byte(`{"name":"Remux","specifications":[{"implementation":"QualityModifierSpecification","required":true,"fields":{"value":5}}],"trash_scores":{"default":100}}`))
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := NewFormatScorer(defs, nil)
	if got, _, _ := sc.Score(core.Release{Info: core.ReleaseInfo{Source: core.SourceRemux}}); got != 100 {
		t.Fatalf("remux score %d", got)
	}
	if got, _, _ := sc.Score(core.Release{Info: core.ReleaseInfo{Source: core.SourceBluRay}}); got != 0 {
		t.Fatalf("bluray score %d", got)
	}
}

func TestNotEnglishFormatSkipsUntaggedEnglishRelease(t *testing.T) {
	cfg := testCfg()
	defs, err := ImportCustomFormats([]byte(`{"name":"Not English","specifications":[{"implementation":"LanguageSpecification","negate":true,"fields":{"value":1}}],"trash_scores":{"default":-10000}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolver.CustomFormats = defs
	s := New(&fakeIndexer{}, nil, nil, cfg, Options{})
	defer s.Close()
	profile := s.ProfileFor(appleTVCaps(), nil)
	eng, err := s.scoreRelease(withParsed(rel("e", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 4000)), profile, appleTVCaps(), matrixTitle())
	if err != nil || eng.format != 0 {
		t.Fatalf("untagged English release format score %d (%v)", eng.format, err)
	}
	fr, _ := s.scoreRelease(withParsed(rel("f", "The.Signal.1999.FRENCH.1080p.WEB-DL.H264-GRP", 4000)), profile, appleTVCaps(), matrixTitle())
	if fr.format != -10000 {
		t.Fatalf("French release format score %d", fr.format)
	}
}

func TestVariantBitrateUsesEpisodeNotPackSize(t *testing.T) {
	ep := &core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, Runtime: 45 * time.Minute}
	pack := withParsed(rel("pack", "Show.S01.1080p.WEB-DL.H264-GRP", 40<<10))
	pack.Protocol = core.ProtocolTorrent
	out := pickVariants(ep, core.ClientCaps{}, core.QualityProfile{}, pack, nil, nil, 2, 4, false)
	if len(out) != 1 || out[0].Info.BitrateKbps > 20000 {
		t.Fatalf("pack variant bitrate %+v", out)
	}
}

func TestDVDQualityImportMatchesDVDRip(t *testing.T) {
	if got := releaseQuality(withParsed(rel("d", "The.Signal.1999.DVDRip.XviD-GRP", 700)).Info); got != "dvd-480" {
		t.Fatalf("quality %q", got)
	}
}

type flakyFetchIndexer struct {
	*fakeIndexer
	down string
}

func (f flakyFetchIndexer) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	if r.ID == f.down {
		return nil, core.ErrBudgetExhausted
	}
	return f.fakeIndexer.FetchNZB(ctx, r)
}

// A pin whose indexer is temporarily out of grabs must not make the title
// unplayable while other releases work.
func TestPinTransientFailureFallsBackToRanking(t *testing.T) {
	ctx := context.Background()
	idx := flakyFetchIndexer{fakeIndexer: &fakeIndexer{releases: movieReleases()}, down: "sd720"}
	s := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	defer s.Close()
	pinned := withParsed(movieReleases()[2])
	if _, err := s.Pin(ctx, matrixTitle().Key, "", pinned, ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID == "sd720" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if p, _ := s.PinFor(matrixTitle().Key); p.LastStatus != PinUnavailable {
		t.Fatalf("pin status %q", p.LastStatus)
	}
	// A viewer's pick behaves the same way.
	res, err = s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps(), Prefer: "sd720"})
	if err != nil || res.Release.ID == "sd720" {
		t.Fatalf("prefer res=%+v err=%v", res, err)
	}
}

func TestSizeFormatIgnoresSeasonPackTotal(t *testing.T) {
	cfg := testCfg()
	defs, err := ImportCustomFormats([]byte(`{"name":"Huge","specifications":[{"implementation":"SizeSpecification","fields":{"min":20,"max":0}}],"trash_scores":{"default":-1000}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolver.CustomFormats = defs
	s := New(&fakeIndexer{}, nil, nil, cfg, Options{})
	defer s.Close()
	pack := withParsed(rel("pack", "Show.S01.1080p.WEB-DL.H264-GRP", 40<<10))
	pack.Protocol = core.ProtocolTorrent
	ep := &core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, Runtime: 45 * time.Minute}
	sc, err := s.scoreRelease(pack, s.ProfileFor(appleTVCaps(), nil), appleTVCaps(), ep)
	if err != nil || sc.format != 0 {
		t.Fatalf("pack size format score %d (%v)", sc.format, err)
	}
}

func TestSonarrRemuxQualityImports(t *testing.T) {
	imported, err := ImportQualityProfile([]byte(`{"name":"Remux","items":[{"name":"Bluray-1080p Remux","allowed":true},{"name":"WEBDL-1080p","allowed":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := imported.Profile.AllowedQualities; len(got) != 2 || got[0] != "remux-1080" {
		t.Fatalf("qualities %v", got)
	}
}

// A format regex that times out on one hostile release name only drops that
// release; the title still resolves.
func TestFormatRegexTimeoutRejectsOnlyThatRelease(t *testing.T) {
	cfg := testCfg()
	defs, err := ImportCustomFormats([]byte(`{"name":"Slow","specifications":[{"implementation":"ReleaseTitleSpecification","fields":{"value":"Hybrid(([\\w.-])+)+@"}}],"trash_scores":{"default":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolver.CustomFormats = defs
	idx := &fakeIndexer{releases: []core.Release{
		rel("hostile", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264.Hybrid.Remastered.Extended-GROUPNAME", 8000),
		rel("ok", "The.Signal.1999.1080p.WEB", 8000),
	}}
	s := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer s.Close()
	res, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "ok" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
