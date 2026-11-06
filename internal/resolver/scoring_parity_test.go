package resolver

// Scoring parity harness: Ether vs Radarr/Sonarr (Servarr) custom-format
// semantics. No network, no secrets. The custom formats below are small
// hand-written stand-ins shaped like the TRaSH Guides formats (group-tier
// regexes, Repack/Proper, streaming services at 0, a quality-modifier +
// resolution Remux format); the group names and scores are illustrative, not
// a copy of the live guide. Expected values are computed from the Servarr
// rules: a format matches when, per specification type, every required spec
// matches and at least one spec (after negate) matches; the score is the sum
// of matched formats' profile scores.

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

func parityFormats(t *testing.T) []core.CustomFormat {
	t.Helper()
	defs, err := ImportCustomFormats([]byte(`[
 {"trash_id":"remux-t1","name":"Remux Tier 01","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(FraMeSToR|BiZKiT)$"}}]},
 {"trash_id":"remux-2160-alt","name":"Remux 2160p (Alternative)","specifications":[
   {"name":"remux","implementation":"QualityModifierSpecification","required":true,"fields":{"value":5}},
   {"name":"2160p","implementation":"ResolutionSpecification","required":true,"fields":{"value":2160}}]},
 {"trash_id":"uhd-t1","name":"UHD Bluray Tier 01","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(TERMiNAL|SPHD)$"}}]},
 {"trash_id":"hd-t1","name":"HD Bluray Tier 01","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(CRiSC|DON)$"}}]},
 {"trash_id":"hd-t2","name":"HD Bluray Tier 02","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(EbP|DiMENSION)$"}}]},
 {"trash_id":"hd-t3","name":"HD Bluray Tier 03","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(playHD|SA89)$"}}]},
 {"trash_id":"web-t1","name":"WEB Tier 01","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(ABBiE|FLUX)$"}}]},
 {"trash_id":"web-t2","name":"WEB Tier 02","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(CMRG|SiGMA)$"}}]},
 {"trash_id":"web-t3","name":"WEB Tier 03","specifications":[{"name":"g","implementation":"ReleaseGroupSpecification","fields":{"value":"^(Cinefile|BYNDR)$"}}]},
 {"trash_id":"repack","name":"Repack/Proper","specifications":[{"name":"r","implementation":"ReleaseTitleSpecification","fields":{"value":"\\b(repack\\d?|proper)\\b"}}]},
 {"trash_id":"amzn","name":"AMZN","specifications":[{"name":"a","implementation":"ReleaseTitleSpecification","fields":{"value":"\\b(amzn|amazon)\\b"}}]},
 {"trash_id":"nf","name":"NF","specifications":[{"name":"n","implementation":"ReleaseTitleSpecification","fields":{"value":"\\b(nf|netflix)\\b"}}]}
]`))
	if err != nil {
		t.Fatal(err)
	}
	return defs
}

// parityScores is the profile map: streaming services at 0 on purpose.
var parityScores = map[string]int{
	"remux-t1": 1950, "remux-2160-alt": 50, "uhd-t1": 1800,
	"hd-t1": 1800, "hd-t2": 1750, "hd-t3": 1700,
	"web-t1": 1700, "web-t2": 1650, "web-t3": 1600,
	"repack": 5, "amzn": 0, "nf": 0,
}

func parseRelease(name string, size int64) core.Release {
	return core.Release{Name: name, Size: size, Info: releaseparse.Parse(name)}
}

func TestParityGoldenReleaseScores(t *testing.T) {
	s, err := NewFormatScorer(parityFormats(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		total int
		cfs   []string // matched format IDs, in definition order
	}{
		{"The.Matrix.1999.2160p.UHD.BluRay.REMUX.HDR.HEVC.TrueHD.7.1.Atmos-FraMeSToR", 2000, []string{"remux-t1", "remux-2160-alt"}},
		{"The.Matrix.1999.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-BiZKiT", 1950, []string{"remux-t1"}},
		{"The.Matrix.1999.2160p.UHD.BluRay.REMUX.HDR.HEVC-NOGRP", 50, []string{"remux-2160-alt"}},
		{"The.Matrix.1999.2160p.UHD.BluRay.x265.HDR-TERMiNAL", 1800, []string{"uhd-t1"}},
		{"The.Matrix.1999.1080p.BluRay.x264-DON", 1800, []string{"hd-t1"}},
		{"The.Matrix.1999.1080p.BluRay.x264-EbP", 1750, []string{"hd-t2"}},
		{"The.Matrix.1999.1080p.BluRay.x264-playHD", 1700, []string{"hd-t3"}},
		{"The.Matrix.1999.REPACK.1080p.BluRay.x264-DON", 1805, []string{"hd-t1", "repack"}},
		{"The.Matrix.1999.PROPER.1080p.BluRay.x264-NOGRP", 5, []string{"repack"}},
		{"The.Matrix.1999.2160p.WEB-DL.DDP5.1.HDR.HEVC-FLUX", 1700, []string{"web-t1"}},
		{"The.Matrix.1999.1080p.WEB-DL.DDP5.1.H.264-CMRG", 1650, []string{"web-t2"}},
		{"The.Matrix.1999.1080p.WEB-DL.DDP5.1.H.264-Cinefile", 1600, []string{"web-t3"}},
		// Streaming services score 0 but still match.
		{"The.Matrix.1999.1080p.AMZN.WEB-DL.DDP5.1.H.264-ABBiE", 1700, []string{"web-t1", "amzn"}},
		{"The.Matrix.1999.1080p.NF.WEB-DL.DDP5.1.H.264-NOGRP", 0, []string{"nf"}},
		{"The.Matrix.1999.1080p.BluRay.x264-NOGRP", 0, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			total, matches, err := s.ScoreWithScores(parseRelease(tt.name, 0), parityScores)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			if total != tt.total || len(ids) != len(tt.cfs) {
				t.Fatalf("total %d matches %v, want %d %v (group %q)", total, ids, tt.total, tt.cfs, parseRelease(tt.name, 0).Info.Group)
			}
			for i := range ids {
				if ids[i] != tt.cfs[i] {
					t.Fatalf("matches %v, want %v", ids, tt.cfs)
				}
			}
		})
	}
}

// Servarr: group specs by type; per group all Required must match and at
// least one spec (negate applied) must match; a format's score is added once.
func TestParityRequiredNegateGrouping(t *testing.T) {
	defs, err := ImportCustomFormats([]byte(`{"trash_id":"x","name":"X","specifications":[
 {"name":"req","implementation":"ReleaseTitleSpecification","required":true,"fields":{"value":"hdr"}},
 {"name":"alt1","implementation":"ReleaseTitleSpecification","fields":{"value":"dv"}},
 {"name":"notdv","implementation":"ReleaseTitleSpecification","negate":true,"fields":{"value":"hybrid"}},
 {"name":"res","implementation":"ResolutionSpecification","fields":{"value":2160}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewFormatScorer(defs, map[string]int{"x": 7})
	for _, tt := range []struct {
		name string
		want int
	}{
		{"M.2160p.HDR.DV", 7},        // required + alt + negate ok + resolution
		{"M.2160p.HDR", 7},           // negated spec matches ("hybrid" absent) so the title group has an any-match
		{"M.2160p.DV", 0},            // required title spec fails
		{"M.1080p.HDR.DV", 0},        // resolution group has no match
		{"M.2160p.HDR.HYBRID.DV", 7}, // "dv" matches the any-group; negate spec fails but is not required
		{"M.2160p.HDR.HYBRID", 7},    // the required spec itself satisfies the "any match" test, as in Servarr (Matches.All(!match) is false)
	} {
		got, _, err := s.Score(parseRelease(tt.name, 0))
		if err != nil || got != tt.want {
			t.Errorf("%s: got %d err %v, want %d", tt.name, got, err, tt.want)
		}
	}
	// A format that matches twice via several specs still counts once.
	defs2, _ := ImportCustomFormats([]byte(`{"trash_id":"y","name":"Y","specifications":[
 {"implementation":"ReleaseTitleSpecification","fields":{"value":"a"}},{"implementation":"ReleaseTitleSpecification","fields":{"value":"b"}}]}`))
	s2, _ := NewFormatScorer(defs2, map[string]int{"y": 3})
	if got, _, _ := s2.Score(core.Release{Name: "ab"}); got != 3 {
		t.Errorf("multi-match format scored %d, want 3", got)
	}
}

func TestParitySonarrAndRadarrSourceEnums(t *testing.T) {
	mk := func(app string, v int) *FormatScorer {
		defs, err := ImportCustomFormats([]byte(`{"trash_id":"s","name":"S","specifications":[{"implementation":"SourceSpecification","fields":{"value":` + string(rune('0'+v)) + `}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		defs[0].App = app
		s, err := NewFormatScorer(defs, map[string]int{"s": 1})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, tt := range []struct {
		app    string
		value  int
		source core.Source
		want   int
	}{
		{AppRadarr, 7, core.SourceWEBDL, 1}, {AppRadarr, 8, core.SourceWEBRip, 1}, {AppRadarr, 9, core.SourceBluRay, 1},
		{AppRadarr, 9, core.SourceRemux, 1}, // Radarr: remux is source Bluray + modifier Remux
		{AppRadarr, 7, core.SourceBluRay, 0},
		{AppSonarr, 3, core.SourceWEBDL, 1}, {AppSonarr, 4, core.SourceWEBRip, 1}, {AppSonarr, 6, core.SourceBluRay, 1},
		{AppSonarr, 7, core.SourceRemux, 1}, // Sonarr: BlurayRaw
		{AppSonarr, 6, core.SourceRemux, 0},
	} {
		got, _, _ := mk(tt.app, tt.value).Score(core.Release{Info: core.ReleaseInfo{Source: tt.source}})
		if got != tt.want {
			t.Errorf("%s value %d source %s: got %d want %d", tt.app, tt.value, tt.source, got, tt.want)
		}
	}
}

// DIVERGENCE (low): Radarr's SizeSpecification is min < size <= max in GB
// (read from Radarr develop source); Ether is min <= size <= max, and
// treats max 0 as unbounded. Pins Ether's current behaviour.
func TestParityDivergenceSizeBounds(t *testing.T) {
	defs, _ := ImportCustomFormats([]byte(`{"trash_id":"z","name":"Z","specifications":[{"implementation":"SizeSpecification","fields":{"min":10,"max":20}}]}`))
	s, _ := NewFormatScorer(defs, map[string]int{"z": 1})
	const gib = int64(1 << 30)
	for _, tt := range []struct {
		size       int64
		ether, srv int
	}{{10 * gib, 1, 0}, {20 * gib, 1, 1}, {10*gib + 1, 1, 1}, {20*gib + 1, 0, 0}} {
		got, _, _ := s.Score(core.Release{Size: tt.size})
		if got != tt.ether {
			t.Errorf("size %d: ether %d, pinned %d", tt.size, got, tt.ether)
		}
		_ = tt.srv // Servarr expectation, documented only
	}
}

// DIVERGENCE (high if hit): a format missing from the profile's score map
// is scored by the global map / the TRaSH default in Ether, but contributes
// 0 in Radarr (formats not in the profile are ignored). Import and sync
// compensate by writing explicit 0 entries (qualitysync.go, qualityimport.go);
// hand-edited or older profiles do not get that protection.
func TestParityDivergenceUnlistedFormatInheritsDefault(t *testing.T) {
	defs := parityFormats(t)
	for i := range defs {
		if defs[i].ID == "web-t1" {
			defs[i].Scores = map[string]int{"default": 1700}
		}
	}
	s, _ := NewFormatScorer(defs, nil)
	rel := parseRelease("The.Matrix.1999.1080p.WEB-DL.DDP5.1.H.264-ABBiE", 0)
	if got, _, _ := s.ScoreWithScores(rel, map[string]int{"web-t1": 0}); got != 0 {
		t.Fatalf("explicit 0 must mask the guide default, got %d", got)
	}
	if got, _, _ := s.ScoreWithScores(rel, map[string]int{}); got != 1700 { // Radarr: 0
		t.Fatalf("unlisted format: got %d, pinned 1700", got)
	}
}

// 0-score formats: Servarr counts them as matched with +0, so they never
// change a total or the min-score test. In Ether too (total is identical);
// the only differences are that they appear in match lists/traces and that
// an explicit 0 is what blocks inheritance (previous test).
func TestParityZeroScoreFormatsDoNotMoveTotals(t *testing.T) {
	s, _ := NewFormatScorer(parityFormats(t), nil)
	with, m1, _ := s.ScoreWithScores(parseRelease("The.Matrix.1999.1080p.AMZN.WEB-DL.DDP5.1.H.264-ABBiE", 0), parityScores)
	without, m2, _ := s.ScoreWithScores(parseRelease("The.Matrix.1999.1080p.WEB-DL.DDP5.1.H.264-ABBiE", 0), parityScores)
	if with != without || len(m1) != len(m2)+1 {
		t.Fatalf("with %d (%d matches) without %d (%d matches)", with, len(m1), without, len(m2))
	}
}

func TestParityMinScoreResolution(t *testing.T) {
	five, nine := 5, 9
	if got := minFormatScore(config.Resolver{MinCustomFormatScore: &nine}, core.QualityProfile{MinCustomFormatScore: &five}); *got != 5 {
		t.Fatalf("profile min must win, got %d", *got)
	}
	if got := minFormatScore(config.Resolver{MinCustomFormatScore: &nine}, core.QualityProfile{}); *got != 9 {
		t.Fatalf("global fallback, got %d", *got)
	}
}

// The built-in Proper/Repack bonus (+6) is skipped when the profile scores
// a Repack/Proper custom format, so the boost equals the profile's score
// (as in Servarr); without such a format the built-in bonus still applies.
func TestParityRevisionBonusNotDoubleCounted(t *testing.T) {
	cfg := testCfg()
	svc := New(&fakeIndexer{releases: movieReleases()}, nil, nil, cfg, Options{})
	defer svc.Close()
	s, _ := NewFormatScorer(parityFormats(t), nil)
	svc.scorer = s
	title := matrixTitle()
	now := time.Now()
	proper := parseRelease("The.Matrix.1999.PROPER.1080p.BluRay.x264-NOGRP", 0)
	plain := parseRelease("The.Matrix.1999.1080p.BluRay.x264-NOGRP", 0)
	delta := func(scores map[string]int) float64 {
		p := core.QualityProfile{Name: "p", MaxHeight: 1080, MinHeight: 720, CustomFormatScores: scores,
			PreferredCodecs: []string{"h264", "hevc"}, PreferredSources: []core.Source{core.SourceBluRay}}
		a, err := svc.scoreRelease(proper, p, appleTVCaps(), title, now)
		if err != nil {
			t.Fatal(err)
		}
		b, err := svc.scoreRelease(plain, p, appleTVCaps(), title, now)
		if err != nil {
			t.Fatal(err)
		}
		return a.total - b.total
	}
	if d := delta(parityScores); d != 5 {
		t.Fatalf("profile scoring Repack/Proper: delta %.1f, want 5", d)
	}
	zero := map[string]int{"repack": 0}
	if d := delta(zero); d != 6 {
		t.Fatalf("Repack/Proper scored 0: built-in bonus should remain, delta %.1f want 6", d)
	}
	if d := delta(map[string]int{"repack": 5, "hd-t1": 0}); d != 5 {
		t.Fatalf("delta %.1f want 5", d)
	}
	// Unrelated formats do not suppress the bonus.
	if d := delta(map[string]int{"hd-t1": 1800}); d != 6 {
		t.Fatalf("no revision format scored: delta %.1f want 6", d)
	}
}
