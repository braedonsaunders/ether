package resolver

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestNativeCustomFormatLookaroundsAndGrouping(t *testing.T) {
	defs, err := ImportCustomFormats([]byte(`{"trash_id":"atmos","name":"Audio","trash_scores":{"default":5000},"specifications":[{"name":"TrueHD","implementation":"ReleaseTitleSpecification","required":true,"fields":{"value":"True[ .-]?HD"}},{"name":"not DD","implementation":"ReleaseTitleSpecification","required":true,"negate":true,"fields":{"value":"(?<!e-)\\b(ac-?3)\\b"}},{"name":"optional","implementation":"ReleaseTitleSpecification","fields":{"value":"DoesNotMatch"}},{"name":"group","implementation":"ReleaseGroupSpecification","fields":{"value":"^GRP$"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewFormatScorer(defs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, group string
		score       int
	}{{"Movie.TrueHD.Atmos", "GRP", 5000}, {"Movie.TrueHD.AC3", "GRP", 0}, {"Movie.TrueHD", "OTHER", 0}, {"Movie.EAC3", "GRP", 0}} {
		score, matches, err := s.Score(core.Release{Name: tt.name, Info: core.ReleaseInfo{Group: tt.group}})
		if err != nil || score != tt.score {
			t.Fatalf("%s: score %d matches %v err %v", tt.name, score, matches, err)
		}
	}
	score, _, err := s.ScoreWithScores(core.Release{Name: "Movie.TrueHD", Info: core.ReleaseInfo{Group: "GRP"}}, map[string]int{"atmos": -500})
	if err != nil || score != -500 {
		t.Fatalf("override %d %v", score, err)
	}
	if _, err := ImportCustomFormats([]byte(`{"name":"unknown","specifications":[{"implementation":"ImaginarySpecification"}]}`)); err == nil {
		t.Fatal("unknown spec silently imported")
	}
}

func TestNativeSourceAndQualityProfile(t *testing.T) {
	defs, err := ImportCustomFormats([]byte(`{"name":"WEB","specifications":[{"implementation":"SourceSpecification","fields":{"value":7}}],"trash_scores":{"default":100}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewFormatScorer(defs, nil)
	score, _, err := s.Score(core.Release{Info: core.ReleaseInfo{Source: core.SourceWEBDL}})
	if err != nil || score != 100 {
		t.Fatalf("native WEB enum %d %v", score, err)
	}
	imported, err := ImportQualityProfile([]byte(`{"name":"HD","minFormatScore":5,"items":[{"name":"WEB 1080p","allowed":true,"items":["WEBDL-1080p","WEBRip-1080p"]},{"name":"Remux-2160p","allowed":false}],"formatItems":{"Audio":"atmos"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Profile.AllowedQualities) != 2 || imported.Profile.MaxHeight != 1080 || *imported.Profile.MinCustomFormatScore != 5 || imported.FormatIDs["Audio"] != "atmos" {
		t.Fatalf("import: %+v", imported)
	}
}

type controlledHealth struct {
	slowStarted   chan struct{}
	slowCancelled chan struct{}
}

func (h controlledHealth) Check(ctx context.Context, nzb []byte) (bool, error) {
	if strings.Contains(string(nzb), "uhd") {
		close(h.slowStarted)
		<-ctx.Done()
		close(h.slowCancelled)
		return false, ctx.Err()
	}
	select {
	case <-h.slowStarted:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return true, nil
}
func TestHealthRaceCancelsSlowPreferredCandidate(t *testing.T) {
	cfg := testCfg()
	cfg.Resolver.HealthRace = 2
	h := controlledHealth{make(chan struct{}), make(chan struct{})}
	svc := New(&fakeIndexer{releases: movieReleases()}, h, nil, cfg, Options{})
	defer svc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("race got %+v %v", res, err)
	}
	select {
	case <-h.slowCancelled:
	default:
		t.Fatal("slow loser not joined")
	}
}
func TestFlightKeyIncludesBandwidthAndCodecs(t *testing.T) {
	req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
	p := defaultProfile()
	a := flightKey(req, p)
	req.Client.MaxBitrateKbps = 3000
	if a == flightKey(req, p) {
		t.Fatal("different remote caps coalesced")
	}
}
func TestRemoteBandwidthFitWins(t *testing.T) {
	cfg := testCfg()
	cfg.Quality.Rules = nil // the remote profile applies, not the Apple TV rule
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, nil, nil, cfg, Options{})
	defer svc.Close()
	c := appleTVCaps()
	c.Remote = true
	c.MaxBitrateKbps = 6000
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: c})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("bandwidth fit %+v %v", res, err)
	}
}

type backgroundBadHealth struct{ entered, release chan struct{} }

func (h backgroundBadHealth) Check(ctx context.Context, _ []byte) (bool, error) {
	close(h.entered)
	select {
	case <-h.release:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
func TestOptimisticStartChecksAndMarksBad(t *testing.T) {
	cfg := testCfg()
	cfg.Resolver.OptimisticStart = true
	h := backgroundBadHealth{make(chan struct{}), make(chan struct{})}
	svc := New(&fakeIndexer{releases: movieReleases()}, h, nil, cfg, Options{})
	defer svc.Close()
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("optimistic start %+v %v", res, err)
	}
	<-h.entered
	close(h.release)
	svc.bgWG.Wait()
	if !svc.isBad("uhd") {
		t.Fatal("background failure did not mark bad")
	}
}
func TestCustomFormatScoresAndMinimumAppearInTrace(t *testing.T) {
	cfg := testCfg()
	defs, err := ImportCustomFormats([]byte(`{"trash_id":"small","name":"720","trash_scores":{"default":1000},"specifications":[{"implementation":"ResolutionSpecification","fields":{"value":720}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolver.CustomFormats = defs
	minimum := 1
	cfg.Resolver.MinCustomFormatScore = &minimum
	svc := New(&fakeIndexer{releases: movieReleases()}, nil, nil, cfg, Options{})
	defer svc.Close()
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("custom score %+v %v", res, err)
	}
	found := false
	for _, c := range svc.Traces()[0].Candidates {
		if c.ReleaseID == "sd720" {
			found = len(c.CustomFormats) == 1 && c.CustomFormats[0].Score == 1000
		}
	}
	if !found {
		t.Fatal("score explanation absent")
	}
}

func TestClientRejectionDoesNotHideTitleForOtherClients(t *testing.T) {
	svc := New(&fakeIndexer{releases: movieReleases()[:1]}, nil, nil, testCfg(), Options{})
	defer svc.Close()
	if _, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err == nil {
		t.Fatal("web accepted incompatible 4K DV")
	}
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("client rejection poisoned availability: %+v %v", res, err)
	}
}

func TestNativeLanguageSpecifications(t *testing.T) {
	defs, err := ImportCustomFormats([]byte(`[
		{"trash_id":"not-original","name":"Language: Not Original","trash_scores":{"default":-10000},"specifications":[{"name":"Not Original","implementation":"LanguageSpecification","negate":true,"required":false,"fields":{"value":-2}}]},
		{"trash_id":"korean","name":"Korean","trash_scores":{"default":10},"specifications":[{"name":"Korean","implementation":"LanguageSpecification","fields":{"value":21}}]},
		{"trash_id":"not-only-english","name":"Not Only English","trash_scores":{"default":1},"specifications":[{"name":"Except English","implementation":"LanguageSpecification","fields":{"value":1,"exceptLanguage":true}}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewFormatScorer(defs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		langs    []string
		original string
		score    int
	}{
		{[]string{"korean"}, "korean", 10 + 1},
		{[]string{"english"}, "korean", -10000},
		{[]string{"english"}, "english", 0},
		{[]string{"english", "korean"}, "korean", 10 + 1},
		// Unknown original language: Original names no language, as in Radarr.
		{[]string{"english"}, "", -10000},
	} {
		score, matches, err := s.ScoreForTitle(core.Release{Info: core.ReleaseInfo{Languages: tt.langs}}, nil, tt.original)
		if err != nil || score != tt.score {
			t.Fatalf("%v original=%q: score %d matches %v err %v", tt.langs, tt.original, score, matches, err)
		}
	}
	for _, v := range []int{-3, 59} {
		if _, err := NewFormatScorer([]core.CustomFormat{{Name: "bad", Specifications: []core.FormatSpecification{{Implementation: "LanguageSpecification", Fields: map[string]json.RawMessage{"value": json.RawMessage(strconv.Itoa(v))}}}}}, nil); err == nil {
			t.Fatalf("language value %d imported", v)
		}
	}
	if got := originalLanguage(&core.Title{Language: "ko"}); got != "korean" {
		t.Fatalf("originalLanguage(ko) = %q", got)
	}
}
