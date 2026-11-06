package resolver

// Evaluation mode: one profile setting that switches BOTH scoring and
// ordering between Ether's own evaluation and Radarr/Sonarr's. Each gap is
// tested in both modes; the Ether-mode cases pin today's behaviour.

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

func TestEvaluationModeDefaults(t *testing.T) {
	order := [][]string{{"bluray-1080"}}
	imported := &core.ProfileImport{Source: "trash", App: AppRadarr}
	for _, tt := range []struct {
		name string
		p    core.QualityProfile
		want bool
	}{
		{"manual profile", core.QualityProfile{}, false},
		{"manual, order only", core.QualityProfile{QualityOrder: order}, false},
		{"imported with order", core.QualityProfile{Import: imported, QualityOrder: order}, true},
		{"imported before the setting existed (no order)", core.QualityProfile{Import: imported}, false},
		{"imported, owner chose ether", core.QualityProfile{Import: imported, QualityOrder: order, EvaluationMode: core.EvaluationModeEther}, false},
		{"manual, owner chose servarr", core.QualityProfile{EvaluationMode: core.EvaluationModeServarr}, true},
	} {
		if got := tt.p.ServarrMode(); got != tt.want {
			t.Errorf("%s: ServarrMode %v want %v", tt.name, got, tt.want)
		}
	}
	// The selection key does not move for a profile whose behaviour did not.
	a := core.QualityProfile{Name: "p", MaxHeight: 1080}
	b := a
	b.EvaluationMode = core.EvaluationModeEther
	if a.ComputeSelectionKey() != b.ComputeSelectionKey() {
		t.Error("explicit ether mode changed the selection key of an ether-mode profile")
	}
	c := a
	c.EvaluationMode = core.EvaluationModeServarr
	if a.ComputeSelectionKey() == c.ComputeSelectionKey() {
		t.Error("servarr mode did not change the selection key")
	}
}

func TestImportQualityProfileKeepsQualityOrder(t *testing.T) {
	imp, err := ImportQualityProfile([]byte(`{"name":"P","minFormatScore":0,"items":[
		{"name":"Remux-1080p","allowed":true},
		{"name":"Bluray|WEB 1080p","allowed":true,"items":["WEBRip-1080p","Bluray-1080p","WEBDL-1080p"]},
		{"name":"Remux-2160p","allowed":false},{"name":"WEBDL-720p","allowed":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"remux-1080"}, {"webrip-1080", "bluray-1080", "webdl-1080"}, {"webdl-720"}}
	if len(imp.Profile.QualityOrder) != len(want) {
		t.Fatalf("order %v", imp.Profile.QualityOrder)
	}
	for i := range want {
		if !slices.Equal(imp.Profile.QualityOrder[i], want[i]) {
			t.Fatalf("order %v want %v", imp.Profile.QualityOrder, want)
		}
	}
	imp.Profile.Import = &core.ProfileImport{Source: "trash"}
	if !imp.Profile.ServarrMode() {
		t.Fatal("an imported profile with an order should default to servarr mode")
	}
}

func sparseFormats(t *testing.T) *FormatScorer {
	t.Helper()
	defs := []core.CustomFormat{
		format("a", "A", map[string]int{"default": 10}, titleSpec),
		format("b", "B", map[string]int{"default": 20}, titleSpec),
	}
	s, err := NewFormatScorer(defs, map[string]int{"a": 5, "b": 7})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// G2: unlisted formats.
func TestEvaluationModeUnlistedFormats(t *testing.T) {
	s := sparseFormats(t)
	rel := core.Release{Name: "x"}
	sparse := map[string]int{"b": 7}
	if got, _, _ := s.scoreFor(rel, nil, sparse, false); got != 12 {
		t.Fatalf("ether mode: sparse map must inherit the global score, got %d", got)
	}
	if got, _, _ := s.scoreFor(rel, nil, sparse, true); got != 7 {
		t.Fatalf("servarr mode: unlisted format must score 0, got %d", got)
	}
}

// G1: the language list a rule sees.
func TestEvaluationModeLanguages(t *testing.T) {
	english := &core.Title{Title: "Obsession", Language: "en"}
	spec := func(name string) core.FormatSpecification {
		return core.FormatSpecification{Name: name, Implementation: "LanguageSpecification", Fields: map[string]json.RawMessage{"value": []byte("1")}}
	}
	defs := []core.CustomFormat{{ID: "en", Name: "English", Scores: map[string]int{"default": 10}, App: AppRadarr, Specifications: []core.FormatSpecification{spec("english")}}}
	s, err := NewFormatScorer(defs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name         string
		ether, serv  int
		servLanguage []string
	}{
		{"Obsession.2026.MULTi.1080p.WEB-DL.H.264-GRP", 0, 10, []string{"english"}},
		{"Obsession.2026.German.DL.1080p.BluRay.x264-GRP", 0, 10, []string{"german", "english"}},
		{"Obsession.2026.GERMAN.ML.1080p.WEB.x264-GRP", 0, 10, []string{"german", "english"}},
		{"Obsession.2026.GERMAN.WEB.DL.1080p.x264-GRP", 0, 0, []string{"german"}}, // WEB-DL is not a dual-language tag
		{"Obsession.2026.1080p.BluRay.x264-GRP", 10, 10, []string{"english"}},
		{"Obsession.2026.FRENCH.1080p.BluRay.x264-GRP", 0, 0, []string{"french"}},
	} {
		rel := parseRelease(tt.name, 0)
		if got := formatLanguages(rel, english); !slices.Equal(got, tt.servLanguage) {
			t.Errorf("%s: formatLanguages %v want %v", tt.name, got, tt.servLanguage)
		}
		if got, _, _ := s.scoreFor(rel, english, map[string]int{"en": 10}, false); got != tt.ether {
			t.Errorf("%s: ether mode %d want %d", tt.name, got, tt.ether)
		}
		if got, _, _ := s.scoreFor(rel, english, map[string]int{"en": 10}, true); got != tt.serv {
			t.Errorf("%s: servarr mode %d want %d", tt.name, got, tt.serv)
		}
	}
	// The original language, not English, fills in for an untagged release.
	japanese := &core.Title{Title: "Obsession", Language: "ja"}
	if got := formatLanguages(parseRelease("Obsession.2026.1080p.BluRay.x264-GRP", 0), japanese); !slices.Equal(got, []string{"japanese"}) {
		t.Errorf("untagged release of a Japanese title: %v", got)
	}
	if got := formatLanguages(parseRelease("Obsession.2026.German.DL.1080p.BluRay.x264-GRP", 0), japanese); !slices.Equal(got, []string{"german", "japanese"}) {
		t.Errorf("German.DL of a Japanese title: %v", got)
	}
}

// G4: the release title a rule matches.
func TestEvaluationModeTitleInput(t *testing.T) {
	rel := parseRelease("The.Matrix.1999.1080p.BluRay.x264-DON.mkv", 0)
	if got := radarrSimpleReleaseTitle(rel.Name, rel.Info.Title); got != "A.Movie.1999.1080p.BluRay.x264-DON" {
		t.Errorf("radarr simple title %q", got)
	}
	if got := radarrSimpleReleaseTitle("The Matrix (1999) 1080p [x264]?-", "The Matrix"); got != "A Movie (1999) 1080p [x264]" {
		t.Errorf("radarr simple title with spaces %q", got)
	}
	if got := servarrReleaseTitle("Show.S01E01.1080p.WEB-DL-GRP.mkv"); got != "Show.S01E01.1080p.WEB-DL-GRP" {
		t.Errorf("sonarr release title %q", got)
	}
	titleRule := func(app string) *FormatScorer {
		def := core.CustomFormat{ID: "m", Name: "M", App: app, Scores: map[string]int{"default": 1},
			Specifications: []core.FormatSpecification{{Name: "m", Implementation: "ReleaseTitleSpecification", Fields: map[string]json.RawMessage{"value": []byte(`"matrix"`)}}}}
		s, err := NewFormatScorer([]core.CustomFormat{def}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	radarr, sonarr := titleRule(AppRadarr), titleRule(AppSonarr)
	for _, tt := range []struct {
		app     string
		s       *FormatScorer
		ether   int
		servarr int
	}{
		{AppRadarr, radarr, 1, 0}, // Radarr hides the movie's own title behind "A.Movie"
		{AppSonarr, sonarr, 1, 1}, // Sonarr matches the release title
	} {
		if got, _, _ := tt.s.scoreFor(rel, nil, map[string]int{"m": 1}, false); got != tt.ether {
			t.Errorf("%s ether mode %d want %d", tt.app, got, tt.ether)
		}
		if got, _, _ := tt.s.scoreFor(rel, nil, map[string]int{"m": 1}, true); got != tt.servarr {
			t.Errorf("%s servarr mode %d want %d", tt.app, got, tt.servarr)
		}
	}
}

// G10: the minimum score.
func TestEvaluationModeMinimumScore(t *testing.T) {
	servarr := core.QualityProfile{EvaluationMode: core.EvaluationModeServarr}
	if m := minFormatScore(config.Resolver{}, servarr); m == nil || *m != 0 {
		t.Fatalf("servarr mode default minimum: %v", m)
	}
	if m := minFormatScore(config.Resolver{}, core.QualityProfile{}); m != nil {
		t.Fatalf("ether mode must keep no minimum, got %d", *m)
	}
	five := 5
	if m := minFormatScore(config.Resolver{}, core.QualityProfile{EvaluationMode: core.EvaluationModeServarr, MinCustomFormatScore: &five}); *m != 5 {
		t.Fatalf("profile minimum must win, got %d", *m)
	}
}

// G8/G9: ordering.
func TestEvaluationModeOrdering(t *testing.T) {
	order := [][]string{{"remux-2160"}, {"bluray-1080"}, {"webdl-2160", "webrip-2160"}, {"webdl-1080"}}
	base := core.QualityProfile{Name: "p", MaxHeight: 2160, QualityOrder: order, Import: &core.ProfileImport{Source: "trash", App: AppRadarr}}
	servarr, ether := base, base
	servarr.EvaluationMode = core.EvaluationModeServarr
	ether.EvaluationMode = core.EvaluationModeEther
	cand := func(src core.Source, height, cf int, score float64) candidate {
		return candidate{rel: core.Release{Score: score, Info: core.ReleaseInfo{Source: src, Height: height}}, quality: qualityRank{height: height}, langOK: true, cf: cf}
	}
	web2160 := cand(core.SourceWEBDL, 2160, 100, 120)
	bd1080 := cand(core.SourceBluRay, 1080, 1700, 900)
	// Servarr: the profile ranks Bluray-1080p above WEB-2160p, whatever the score.
	if !compareCandidates(bd1080, web2160, servarr) || compareCandidates(web2160, bd1080, servarr) {
		t.Error("servarr mode: Bluray-1080p must outrank WEB-2160p by profile position")
	}
	// Ether: resolution first.
	if !compareCandidates(web2160, bd1080, ether) {
		t.Error("ether mode: 2160p must outrank 1080p")
	}
	// Same quality: pure CF score decides, not the soft score.
	lowCF, highCF := cand(core.SourceBluRay, 1080, 10, 999), cand(core.SourceBluRay, 1080, 50, 1)
	if !compareCandidates(highCF, lowCF, servarr) {
		t.Error("servarr mode: higher CF score must win over a higher soft score")
	}
	// Revision comes before CF score.
	proper := cand(core.SourceBluRay, 1080, 0, 0)
	proper.rel.Info.Proper = true
	if !compareCandidates(proper, highCF, servarr) {
		t.Error("servarr mode: a Proper outranks the original at equal quality")
	}
	// Group members tie on quality: CF decides between WEBDL and WEBRip 2160p.
	webrip := cand(core.SourceWEBRip, 2160, 300, 0)
	if !compareCandidates(webrip, web2160, servarr) {
		t.Error("servarr mode: grouped qualities tie, so the CF score decides")
	}
	// Soft score is the final tiebreak.
	x, y := cand(core.SourceBluRay, 1080, 5, 10), cand(core.SourceBluRay, 1080, 5, 20)
	if !compareCandidates(y, x, servarr) {
		t.Error("servarr mode: soft score breaks an exact tie")
	}
	// Ether safeguards stay: an unacceptable audio language is a last resort
	// and a connection that does not fit loses, even with better quality.
	wrongLang := cand(core.SourceBluRay, 1080, 5000, 0)
	wrongLang.langOK = false
	if !compareCandidates(web2160, wrongLang, servarr) {
		t.Error("servarr mode: language gate must still apply")
	}
	big := bd1080
	big.bandwidth = -1
	if !compareCandidates(web2160, big, servarr) {
		t.Error("servarr mode: connection gate must still apply")
	}
	// Torrent peers, usenet age and size follow the CF score.
	t1, t2 := cand(core.SourceBluRay, 1080, 5, 0), cand(core.SourceBluRay, 1080, 5, 0)
	t1.rel.Protocol, t2.rel.Protocol = core.ProtocolTorrent, core.ProtocolTorrent
	t1.rel.Seeders, t2.rel.Seeders = 1000, 10
	if !compareCandidates(t1, t2, servarr) {
		t.Error("servarr mode: more seeders (log10) wins among torrents")
	}
	u1, u2 := cand(core.SourceBluRay, 1080, 5, 0), cand(core.SourceBluRay, 1080, 5, 0)
	u1.hasAge, u2.hasAge = true, true
	now := time.Now()
	u1.age, u2.age = usenetAgeScore(now.Add(-2*time.Hour), now), usenetAgeScore(now.Add(-400*24*time.Hour), now)
	if !compareCandidates(u1, u2, servarr) {
		t.Error("servarr mode: newer usenet release wins")
	}
	s1, s2 := cand(core.SourceBluRay, 1080, 5, 0), cand(core.SourceBluRay, 1080, 5, 0)
	s1.rel.Size, s2.rel.Size = 20<<30, 10<<30
	if !compareCandidates(s1, s2, servarr) {
		t.Error("servarr mode: larger release wins when no preferred size is known")
	}
	// Unused import guard for releasepolicy in case of future gate tests.
	_ = releasepolicy.Result{}
}

func TestUsenetAgeBuckets(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		age  time.Duration
		want float64
	}{{30 * time.Minute, 1000}, {5 * time.Hour, 100}, {3 * 24 * time.Hour, 10}, {100 * 24 * time.Hour, -2}, {1000 * 24 * time.Hour, -3}} {
		if got := usenetAgeScore(now.Add(-tt.age), now); got != tt.want {
			t.Errorf("age %s: %v want %v", tt.age, got, tt.want)
		}
	}
}
