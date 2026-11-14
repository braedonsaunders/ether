package resolver

import (
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
