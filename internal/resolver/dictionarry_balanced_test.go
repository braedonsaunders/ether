package resolver

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// The published profile prefers WEB-DLs and selected BluRay groups. Ordinary
// BluRays must not acquire another profile's tier scores during import.
func TestDictionarryBalancedImportScoreInvariant(t *testing.T) {
	sql, err := os.ReadFile("testdata/dictionarry-balanced.sql")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := compileDictionarryArchive(t.Context(), buildDictionarryTarGz(t, map[string]string{"ops/0.snapshot.sql": string(sql)}))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDictionarry(DictionarryOptions{})
	d.cached, d.at = cat, d.o.Now()
	imp, found, err := d.Import(t.Context(), AppRadarr, "1080p Balanced")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanQualityImport(imp, AppRadarr, found, config.Config{})
	if *plan.Profile.MinCustomFormatScore != 200000 {
		t.Fatal("lost minimum")
	}
	scorer, err := NewFormatScorer(plan.Definitions)
	if err != nil {
		t.Fatal(err)
	}
	// AIOStreams in the issue table is the indexer label, not part of a release name.
	names := []string{
		"My.Life.As.A.Zucchini.2016.RERIP.1080p.BluRay.x264-RedBlade",
		"My.Life.as.a.Zucchini.2016.1080p.BluRay.x264-FOXM",
		"La.Mia.Vita.da.Zucchina.(2016).BDRip.H264.AC3.ITA.FRA.MultiSub.1080p.[iCV-MIRCrew]",
		"Ma vie de Courgette_2016_BDRip 1080p",
		"Ma.Vie.de.Courgette.2016.1080p.Bluray.x264.LOST",
		"My.Life.As.A.Zucchini.2016.1080p.BluRay.x264-[YTS.AM]",
		"Ma.vie.de.Courgette.2016.BDREMUX.1080p",
		"Ma.vie.de.courgette.2016.tt2321405.1080p.REMUX-merricat",
	}
	title := &core.Title{Language: "fr"}
	wantScores := []int{6, 0, 0, -999999, -999999, -999999, -999999, -999999}
	for i, name := range names {
		r := core.Release{Name: name, Info: releaseparse.Parse(name)}
		score, matches, err := scorer.ScoreForProfile(r, plan.Profile.CustomFormatScores, title)
		if err != nil {
			t.Fatal(err)
		}
		if score != wantScores[i] || score >= *plan.Profile.MinCustomFormatScore {
			t.Fatalf("unexpected acceptance: %s = %d", name, score)
		}
		t.Logf("%s: %d (%v)", name, score, matches)
	}
	// Both positive group tiers and a standard WEB-DL survive the import.
	for _, tc := range []struct {
		suffix string
		want   int
	}{
		{"1080p.BluRay.x264-hallowed", 861000},
		{"1080p.BluRay.x264-BHDStudio", 860000},
		{"1080p.WEB-DL.H264-GRP", 860000},
	} {
		name := "My.Life.As.A.Zucchini.2016." + tc.suffix
		got, _, err := scorer.ScoreForProfile(core.Release{Name: name, Info: releaseparse.Parse(name)}, plan.Profile.CustomFormatScores, title)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %d, want %d (%v)", name, got, tc.want, err)
		}
	}
	// A failed resolution fallback must retain the strict score verdict.
	cfg := testCfg()
	cfg.Resolver.CustomFormats = plan.Definitions
	s := New(&fakeIndexer{}, nil, nil, cfg, Options{})
	defer s.Close()
	profile := plan.Profile
	profile.FallbackBelowMin = true
	profile.MinHeight = 1080
	profile.MinSizeMBPerMin = 0
	rel := core.Release{ID: "rerip", Name: names[0], Info: releaseparse.Parse(names[0]), Size: 8 << 30}
	title = matrixTitle()
	title.Title, title.Year, title.Language = "My Life As A Zucchini", 2016, "fr"
	req := core.ResolveRequest{Title: title, Client: appleTVCaps()}
	var trace Trace
	_, _, err = s.rankCandidates(context.Background(), req, searchRequestFor(req.Title), profile, []core.Release{rel}, &trace, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Candidates) != 1 || trace.Candidates[0].MinimumFormatScore == nil || *trace.Candidates[0].MinimumFormatScore != 200000 || trace.Candidates[0].Reason != fmt.Sprintf("Score %d is below this profile's minimum of %d", 6, 200000) {
		t.Fatalf("lost strict score verdict: %+v", trace.Candidates)
	}
	// Lower-resolution candidates still replace their strict resolution verdict,
	// and indexes remain relative to the caller's pre-existing trace rows.
	lowName := "My.Life.As.A.Zucchini.2016.720p.WEB-DL.H264-GRP"
	low := core.Release{ID: "low", Name: lowName, Info: releaseparse.Parse(lowName), Size: 4 << 30}
	trace = Trace{Candidates: []CandidateTrace{{ReleaseID: "previous"}}}
	cands, _, err := s.rankCandidates(t.Context(), req, searchRequestFor(req.Title), profile, []core.Release{rel, low}, &trace, "")
	if err != nil || len(cands) != 1 || cands[0].trace != 2 || len(trace.Candidates) != 3 || trace.Candidates[1].Reason != "Score 6 is below this profile's minimum of 200000" || trace.Candidates[2].Rejected {
		t.Fatalf("fallback trace mapping: candidates=%+v trace=%+v err=%v", cands, trace.Candidates, err)
	}
}
