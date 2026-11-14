package resolver

import (
	"context"
	"maps"
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
	imp.Source, imp.ProfileID = ImportSourceDictionarry, "1080p-balanced"
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
	wantScores := []int{6, 0, 400, -999999, -999999, -999999, -999999, -999999}
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
	// Slug collisions must retain both source formats and their own scores.
	ddID, ddPlusID := imp.FormatIDs["Dolby Digital"], imp.FormatIDs["Dolby Digital +"]
	if ddID == ddPlusID || ddID == "" || ddPlusID == "" ||
		plan.Profile.CustomFormatScores[ddID] != 400 || plan.Profile.CustomFormatScores[ddPlusID] != 600 {
		t.Fatalf("aliased audio formats: DD=%q DD+=%q scores=%v", ddID, ddPlusID, plan.Profile.CustomFormatScores)
	}
	for _, tc := range []struct {
		codec string
		want  int
	}{{"AC3", 400}, {"EAC3", 600}} {
		name := "Movie.2016.1080p.BluRay." + tc.codec + "-GRP"
		got, _, err := scorer.ScoreForProfile(core.Release{Name: name, Info: releaseparse.Parse(name)}, plan.Profile.CustomFormatScores, title)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %d, want %d (%v)", name, got, tc.want, err)
		}
	}
	// An install that imported the old shared ID is repaired by the normal
	// sync, including removal of the obsolete managed definition and score.
	var saved config.Config
	if err := plan.Apply(&saved, ""); err != nil {
		t.Fatal(err)
	}
	const legacyID = "dictionarry-radarr-dolby-digital"
	oldScores := maps.Clone(saved.Quality.Profiles[0].CustomFormatScores)
	delete(oldScores, ddID)
	delete(oldScores, ddPlusID)
	oldScores[legacyID] = 400
	saved.Quality.Profiles[0].CustomFormatScores = oldScores
	var oldDefs []core.CustomFormat
	for _, def := range saved.Resolver.CustomFormats {
		if def.ID == ddPlusID {
			continue
		}
		if def.ID == ddID {
			def.ID = legacyID
		}
		oldDefs = append(oldDefs, def)
	}
	saved.Resolver.CustomFormats = oldDefs
	syncer := NewQualitySyncer(QualitySyncDeps{
		Dictionarry: d,
		Config:      func() config.Config { return saved },
		Save: func(mutate func(*config.Config) error) error {
			if err := mutate(&saved); err != nil {
				return err
			}
			_, err := NewFormatScorer(saved.Resolver.CustomFormats)
			return err
		},
	})
	report, err := syncer.Sync(t.Context())
	if err != nil || report.ProfilesUpdated != 1 || report.FormatsRemoved != 1 {
		t.Fatalf("repair shared format ID: %+v (%v)", report, err)
	}
	newScores := saved.Quality.Profiles[0].CustomFormatScores
	if _, old := newScores[legacyID]; old || newScores[ddID] != 400 || newScores[ddPlusID] != 600 {
		t.Fatalf("shared score survived sync: %v", newScores)
	}
	for _, def := range saved.Resolver.CustomFormats {
		if def.ID == legacyID {
			t.Fatal("shared definition survived sync")
		}
	}
	report, err = syncer.Sync(t.Context())
	if err != nil || report.ProfilesUpdated != 0 || report.FormatsAdded != 0 || report.FormatsUpdated != 0 || report.FormatsRemoved != 0 {
		t.Fatalf("second sync changed repaired profile: %+v (%v)", report, err)
	}
	// An unreachable score minimum is a preference, with an honest reason.
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
	if len(trace.Candidates) != 1 || trace.Candidates[0].Rejected || trace.Candidates[0].MinimumFormatScore == nil || *trace.Candidates[0].MinimumFormatScore != 200000 || trace.Candidates[0].Reason != "Best available — below this profile's minimum (score 6 of 200000)" {
		t.Fatalf("lost strict score verdict: %+v", trace.Candidates)
	}
	// A release reaching the minimum outranks the closer resolution. Trace
	// indexes remain relative to the caller's pre-existing rows.
	lowName := "My.Life.As.A.Zucchini.2016.720p.WEB-DL.H264-GRP"
	low := core.Release{ID: "low", Name: lowName, Info: releaseparse.Parse(lowName), Size: 4 << 30}
	trace = Trace{Candidates: []CandidateTrace{{ReleaseID: "previous"}}}
	cands, _, err := s.rankCandidates(t.Context(), req, searchRequestFor(req.Title), profile, []core.Release{rel, low}, &trace, "")
	if err != nil || len(cands) != 2 || cands[0].trace != 2 || len(trace.Candidates) != 3 || trace.Candidates[1].Reason != "Best available — below this profile's minimum (score 6 of 200000)" || trace.Candidates[2].Rejected {
		t.Fatalf("fallback trace mapping: candidates=%+v trace=%+v err=%v", cands, trace.Candidates, err)
	}
}
