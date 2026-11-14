package resolver

import (
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Soft preferences must not become permission to cross a hard veto, even
// when the entire search is below the minimum or positive scores offset it.
func TestSoftMinimumHardRules(t *testing.T) {
	cfg := testCfg()
	defs, err := ImportCustomFormats([]byte(`[
 {"trash_id":"repack","name":"Repack","specifications":[{"implementation":"ReleaseTitleSpecification","fields":{"value":"REPACK"}}]},
 {"trash_id":"never","name":"Never","specifications":[{"implementation":"ReleaseTitleSpecification","fields":{"value":"BANNED"}}]}
 ]`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolver.CustomFormats = defs
	minimum := 200000
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].MinCustomFormatScore = &minimum
		cfg.Quality.Profiles[i].CustomFormatScores["repack"] = 6
		cfg.Quality.Profiles[i].CustomFormatScores["never"] = -10000
	}
	best := rel("best", "The.Signal.1999.REPACK.1080p.BluRay.H264-GRP", 8000)
	ordinary := rel("ordinary", "The.Signal.1999.1080p.BluRay.H264-GRP", 8000)
	banned := rel("banned", "The.Signal.1999.BANNED.REPACK.1080p.BluRay.H264-GRP", 8000)
	blocked := rel("blocked", "The.Signal.1999.REPACK.1080p.WEB-DL.H264-GRP", 8000)
	s := New(&fakeIndexer{releases: []core.Release{banned, blocked, ordinary, best}}, fakeHealth{}, nil, cfg, Options{})
	defer s.Close()
	if err := s.MarkBad(t.Context(), blocked.ID, "owner blocked"); err != nil {
		t.Fatal(err)
	}
	req := core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}
	res, err := s.Resolve(t.Context(), req)
	if err != nil || res.Release.ID != "best" || !strings.Contains(res.QualityFallback, "score 6 of 200000") {
		t.Fatalf("closest match=%+v err=%v", res, err)
	}
	variants, err := s.Variants(t.Context(), req, 7)
	if err != nil || len(variants) == 0 || variants[0].Release.ID != "best" || !strings.Contains(variants[0].Label, "score 6 of 200000") {
		t.Fatalf("variants=%+v err=%v", variants, err)
	}
	// An offsetting positive score cannot cancel a never-format veto.
	profile := s.ProfileFor(webCaps(), nil)
	profile.CustomFormatScores["repack"] = 300000
	for _, release := range []core.Release{withParsed(banned), withParsed(blocked)} {
		var trace Trace
		cands, _, err := s.rankCandidates(t.Context(), req, searchRequestFor(req.Title), profile, []core.Release{release}, &trace, "")
		if err != nil || len(cands) != 0 || len(trace.Candidates) != 1 || !trace.Candidates[0].Rejected {
			t.Fatalf("hard veto=%+v trace=%+v err=%v", cands, trace, err)
		}
	}
}

// Soft matches must preserve the strict prefix and prefer the smallest
// shortfall even when source routing, language or connection fit disagree.
func TestSoftMinimumPreferenceOrder(t *testing.T) {
	for _, always := range []bool{false, true} {
		cfg := testCfg()
		cfg.Resolver.SourcePreference = PreferUsenet
		cfg.Resolver.SourceAlwaysFirst = always
		s := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
		strict := []candidate{
			{rel: core.Release{ID: "strict-a"}, keys: rankKeys{position: 10}},
			{rel: core.Release{ID: "strict-b"}, langOK: true, keys: rankKeys{position: 1}},
		}
		wantStrict := s.orderCandidates(slices.Clone(strict), matrixTitle(), nil)
		soft := []candidate{
			{rel: core.Release{ID: "far-score"}, belowScore: true, counted: -8, langOK: true, bandwidth: 1},
			{rel: core.Release{ID: "480p"}, fallback: true, keys: rankKeys{height: 480}, langOK: true},
			{rel: core.Release{ID: "near-score", Protocol: core.ProtocolTorrent}, belowScore: true, counted: -2},
			{rel: core.Release{ID: "720p", Protocol: core.ProtocolTorrent}, fallback: true, keys: rankKeys{height: 720}},
		}
		soft[2].policy.Fallback = 1
		soft[3].policy.Fallback = 1
		input := append(slices.Clone(soft), strict...)
		got := s.orderCandidates(input, matrixTitle(), nil)
		want := []string{wantStrict[0].rel.ID, wantStrict[1].rel.ID, "720p", "480p", "near-score", "far-score"}
		ids := make([]string, len(got))
		for i, c := range got {
			ids[i] = c.rel.ID
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("always=%t: order %v, want %v", always, ids, want)
		}
		s.Close()
	}
}
