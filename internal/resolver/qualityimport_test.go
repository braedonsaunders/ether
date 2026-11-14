package resolver

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestImportQualityProfileSkipsQualitiesEtherCannotMatch(t *testing.T) {
	imp, err := ImportQualityProfile([]byte(`{"name":"Anime","minFormatScore":100,"upgradeAllowed":true,"cutoff":"Remux-1080p","language":"Original",
		"items":[{"name":"Remux-1080p","allowed":true},{"name":"Raw-HD","allowed":true},
		{"name":"Bluray|WEB 1080p","allowed":true,"items":["WEBRip-1080p","Bluray-1080p","WEBDL-1080p","HDTV-1080p"]},
		{"name":"SDTV","allowed":true},{"name":"DVD","allowed":true},{"name":"Remux-2160p","allowed":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := imp.Profile
	want := []string{"remux-1080", "webrip-1080", "bluray-1080", "webdl-1080", "hdtv-1080", "dvd-480"}
	if !slices.Equal(p.AllowedQualities, want) {
		t.Fatalf("qualities %v, want %v", p.AllowedQualities, want)
	}
	// The quality order keeps the profile's items; grouped qualities rank
	// equally.
	if len(p.QualityOrder) != 3 || p.QualityOrder[1].Name != "Bluray|WEB 1080p" || !slices.Equal(p.QualityOrder[1].Qualities, []string{"webrip-1080", "bluray-1080", "webdl-1080", "hdtv-1080"}) {
		t.Fatalf("quality order %+v", p.QualityOrder)
	}
	if p.MinHeight != 480 || p.MaxHeight != 1080 || p.AllowHDR || *p.MinCustomFormatScore != 100 || p.MaxSizeMBPerMin != 0 || p.MinSizeMBPerMin != 5 {
		t.Fatalf("profile %+v", p)
	}
	if !slices.Equal(imp.Skipped, []string{"Raw-HD", "SDTV"}) || imp.App != AppRadarr {
		t.Fatalf("skipped %v app %q", imp.Skipped, imp.App)
	}
	joined := strings.Join(imp.Warnings, "\n")
	// An original-language profile maps onto the profile Language field
	// (Radarr-style) instead of becoming a warning.
	if p.Language != "original" {
		t.Errorf("language %q, want original", p.Language)
	}
	for _, want := range []string{"Left out Raw-HD, SDTV", "Upgrade and cutoff"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings lack %q:\n%s", want, joined)
		}
	}
}

func TestImportQualityProfileRejectsNothingPlayable(t *testing.T) {
	for name, body := range map[string]string{
		"only unsupported": `{"name":"Raw","items":[{"name":"Raw-HD","allowed":true},{"name":"BR-DISK","allowed":true}]}`,
		"none enabled":     `{"name":"Off","items":[{"name":"WEBDL-1080p","allowed":false}]}`,
		"api export":       `{"name":"Mine","items":[{"name":"WEBDL-1080p","allowed":true}],"formatItems":[{"format":3,"name":"x","score":1}]}`,
		"list":             `[{"name":"Mine"}]`,
		"no name":          `{"items":[{"name":"WEBDL-1080p","allowed":true}]}`,
		"not json":         `{`,
	} {
		if _, err := ImportQualityProfile([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := ImportQualityProfile([]byte(`{"name":"Raw","items":[{"name":"Raw-HD","allowed":true}]}`))
	if err == nil || !strings.Contains(err.Error(), "Raw-HD") {
		t.Fatalf("error should name the skipped quality: %v", err)
	}
}

func TestImportQualityProfileUHDAndLanguage(t *testing.T) {
	imp, err := ImportQualityProfile([]byte(`{"name":"[French] UHD","language":"French","trash_score_set":"french-multi-vf",
		"items":[{"name":"Bluray-2160p","allowed":true},{"name":"WEB 2160p","allowed":true,"items":["WEBDL-2160p","WEBRip-2160p"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := imp.Profile
	if !p.AllowHDR || !p.AllowDolbyVision || p.MinHeight != 2160 {
		t.Fatalf("UHD profile %+v", p)
	}
	if p.Language != "french" || len(p.PreferredLanguages) != 0 || imp.ScoreSet != "french-multi-vf" {
		t.Fatalf("language %q preferred %v set %q", p.Language, p.PreferredLanguages, imp.ScoreSet)
	}
	for lang, want := range map[string]string{"Original": "original", "Any": "any"} {
		imp, err := ImportQualityProfile([]byte(`{"name":"L","language":"` + lang + `","items":[{"name":"WEBDL-1080p","allowed":true}]}`))
		if err != nil || imp.Profile.Language != want || len(imp.Warnings) != 0 {
			t.Fatalf("language %s: %+v %v", lang, imp, err)
		}
	}
	sonarr, err := ImportQualityProfile([]byte(`{"name":"Remux","items":[{"name":"Bluray-1080p Remux","allowed":true}]}`))
	if err != nil || sonarr.App != AppSonarr || len(sonarr.Warnings) != 0 {
		t.Fatalf("sonarr profile %+v %v", sonarr, err)
	}
}

func format(id, name string, scores map[string]int, specs string) core.CustomFormat {
	f := core.CustomFormat{ID: id, Name: name, Scores: scores}
	if err := json.Unmarshal([]byte(specs), &f.Specifications); err != nil {
		panic(err)
	}
	return f
}

const titleSpec = `[{"name":"t","implementation":"ReleaseTitleSpecification","fields":{"value":"\\bx\\b"}}]`

func planFixture() (QualityImport, map[string]core.CustomFormat, config.Config) {
	imp := QualityImport{
		Profile:   core.QualityProfile{Name: "SQP", AllowedQualities: []string{"webdl-1080"}},
		ScoreSet:  "sqp",
		FormatIDs: map[string]string{"Have": "have", "New": "new", "Flags": "flags", "Gone": "gone", "Plain": "plain"},
	}
	found := map[string]core.CustomFormat{
		"have":  format("have", "Have", map[string]int{"default": 10, "sqp": 20}, titleSpec),
		"new":   format("new", "New", map[string]int{"default": 5, "sqp": 50}, titleSpec),
		"plain": format("plain", "Plain", map[string]int{"default": -100}, titleSpec),
		"flags": format("flags", "Flags", map[string]int{"default": 1}, `[{"name":"f","implementation":"IndexerFlagSpecification","fields":{"value":1}}]`),
	}
	cfg := config.Config{}
	cfg.Resolver.CustomFormats = []core.CustomFormat{
		format("have", "Have (mine)", map[string]int{"default": 1}, titleSpec),
		format("other", "Other", map[string]int{"default": 7}, titleSpec),
	}
	cfg.Quality.Profiles = []core.QualityProfile{
		{Name: "SQP", MediaType: core.MediaMovie},
		{Name: "SQP (2)", MediaType: core.MediaMovie},
		{Name: "Mine", MediaType: core.MediaMovie, CustomFormatScores: map[string]int{"new": 3}},
	}
	return imp, found, cfg
}

func TestPlanQualityImportResolvesFormats(t *testing.T) {
	imp, found, cfg := planFixture()
	plan := PlanQualityImport(imp, AppRadarr, found, cfg)
	if plan.Profile.Name != "SQP (3)" {
		t.Fatalf("name %q", plan.Profile.Name)
	}
	got := map[string]ImportFormat{}
	for _, f := range plan.Formats {
		got[f.ID] = f
	}
	check := func(id, status string, score int) {
		t.Helper()
		if f := got[id]; f.Status != status || f.Score != score {
			t.Errorf("%s: %+v, want %s %d", id, f, status, score)
		}
	}
	check("have", FormatPresent, 20) // TRaSH's current score for the set
	check("new", FormatAdd, 50)
	check("plain", FormatAdd, -100) // no score in the set: its default
	check("flags", FormatUnsupported, 1)
	check("gone", FormatMissing, 0)
	if got["flags"].Reason != "Uses indexer flags, which Ether doesn't read" || got["gone"].Reason == "" || got["have"].Name != "Have (mine)" {
		t.Fatalf("reasons/names %+v", got)
	}
	if plan.Formats[0].ID != "new" || plan.Formats[len(plan.Formats)-1].ID != "plain" {
		t.Fatalf("formats not ordered by score: %+v", plan.Formats)
	}
	want := map[string]int{"have": 20, "new": 50, "plain": -100}
	if len(plan.Profile.CustomFormatScores) != len(want) {
		t.Fatalf("profile scores %v", plan.Profile.CustomFormatScores)
	}
	for id, v := range want {
		if plan.Profile.CustomFormatScores[id] != v {
			t.Fatalf("profile scores %v", plan.Profile.CustomFormatScores)
		}
	}
	joined := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(joined, "2 custom formats can't be imported") || !strings.Contains(joined, "1 custom format already saved isn't part") {
		t.Fatalf("warnings:\n%s", joined)
	}
}

func TestQualityImportApplyKeepsOtherProfilesRanking(t *testing.T) {
	imp, found, cfg := planFixture()
	plan := PlanQualityImport(imp, AppRadarr, found, cfg)
	if err := plan.Apply(&cfg, "  "); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Quality.Profiles) != 4 {
		t.Fatalf("profiles %+v", cfg.Quality.Profiles)
	}
	added := cfg.Quality.Profiles[3]
	if added.Name != "SQP (3)" || added.CustomFormatScores["other"] != 0 || added.CustomFormatScores["new"] != 50 {
		t.Fatalf("imported profile %+v", added)
	}
	var ids []string
	for _, f := range cfg.Resolver.CustomFormats {
		ids = append(ids, f.ID)
	}
	if !slices.Equal(ids, []string{"have", "other", "new", "plain"}) || cfg.Resolver.CustomFormats[0].Name != "Have (mine)" {
		t.Fatalf("formats %v (existing must stay untouched)", cfg.Resolver.CustomFormats)
	}
	// Other profiles don't list the added formats, so they score 0 there.
	mine := cfg.Quality.Profiles[2].CustomFormatScores
	if mine["new"] != 3 || mine["plain"] != 0 || len(mine) != 1 {
		t.Fatalf("other profile scores %v: own scores kept, nothing added", mine)
	}
	if _, err := NewFormatScorer(cfg.Resolver.CustomFormats); err != nil {
		t.Fatalf("import broke scoring: %v", err)
	}
	if err := plan.Apply(&cfg, "Mine"); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("name clash: %v", err)
	}
	// Applying again under a new name adds no duplicate formats.
	if err := plan.Apply(&cfg, "Again"); err != nil || len(cfg.Resolver.CustomFormats) != 4 {
		t.Fatalf("re-apply: %v, %d formats", err, len(cfg.Resolver.CustomFormats))
	}
}

func TestPlanMarksSonarrFormats(t *testing.T) {
	imp := QualityImport{Profile: core.QualityProfile{Name: "WEB"}, FormatIDs: map[string]string{"Remux Tier 01": "r1"}}
	found := map[string]core.CustomFormat{"r1": format("r1", "Remux Tier 01", map[string]int{"default": 1900},
		`[{"name":"Remux","implementation":"SourceSpecification","required":true,"fields":{"value":7}}]`)}
	plan := PlanQualityImport(imp, AppSonarr, found, config.Config{})
	if len(plan.Definitions) != 1 || plan.Definitions[0].App != AppSonarr || plan.Formats[0].Status != FormatAdd {
		t.Fatalf("plan %+v", plan)
	}
}

// Sonarr numbers sources differently: its 7 is a remux, Radarr's 7 a WEB-DL.
func TestSonarrSourceEnum(t *testing.T) {
	spec := `[{"name":"s","implementation":"SourceSpecification","required":true,"fields":{"value":7}}]`
	radarr := format("r", "Radarr 7", map[string]int{"default": 1}, spec)
	sonarr := format("s", "Sonarr 7", map[string]int{"default": 1}, spec)
	sonarr.App = AppSonarr
	s, err := NewFormatScorer([]core.CustomFormat{radarr, sonarr})
	if err != nil {
		t.Fatal(err)
	}
	match := func(src core.Source) []string {
		_, ms, err := s.Score(core.Release{Info: core.ReleaseInfo{Source: src}})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		return ids
	}
	if got := match(core.SourceRemux); !slices.Equal(got, []string{"s"}) {
		t.Fatalf("remux matched %v", got)
	}
	if got := match(core.SourceWEBDL); !slices.Equal(got, []string{"r"}) {
		t.Fatalf("web-dl matched %v", got)
	}
	bluray := format("b", "Sonarr Bluray", nil, `[{"name":"s","implementation":"SourceSpecification","fields":{"value":6}}]`)
	bluray.App = AppSonarr
	s, _ = NewFormatScorer([]core.CustomFormat{bluray})
	if _, ms, _ := s.Score(core.Release{Info: core.ReleaseInfo{Source: core.SourceRemux}}); len(ms) != 0 {
		t.Fatal("Sonarr's Bluray matched a remux")
	}
	for _, bad := range []core.CustomFormat{
		func() core.CustomFormat {
			f := format("x", "X", nil, `[{"name":"s","implementation":"SourceSpecification","fields":{"value":9}}]`)
			f.App = AppSonarr
			return f
		}(),
		func() core.CustomFormat { f := format("y", "Y", nil, titleSpec); f.App = "lidarr"; return f }(),
	} {
		if _, err := NewFormatScorer([]core.CustomFormat{bad}); err == nil {
			t.Errorf("%s accepted", bad.Name)
		}
	}
}

func TestImportQualityProfileDetectsApp(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"language names radarr": {
			`{"name":"Radarr","language":"Original","items":[{"name":"WEBDL-1080p","allowed":true}]}`, AppRadarr,
		},
		"quality definition names sonarr": {
			`{"name":"Sonarr","trash_quality_definition":{"series":"s1"},"items":[{"name":"WEBDL-1080p","allowed":true}]}`, AppSonarr,
		},
		"sonarr marker wins over language": {
			`{"name":"Sonarr","trash_quality_definition":{"series":"s1"},"language":"Original","items":[{"name":"WEBDL-1080p","allowed":true}]}`, AppSonarr,
		},
		"remux prefix falls back to radarr": {
			`{"name":"Radarr remux","items":[{"name":"Remux-1080p","allowed":true}]}`, AppRadarr,
		},
		"remux suffix falls back to sonarr": {
			`{"name":"Sonarr remux","items":[{"name":"Bluray-1080p Remux","allowed":true}]}`, AppSonarr,
		},
		"plain names leave it unknown": {
			`{"name":"Plain","items":[{"name":"WEBDL-1080p","allowed":true}]}`, "",
		},
	} {
		imp, err := ImportQualityProfile([]byte(tc.body))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if imp.App != tc.want {
			t.Errorf("%s: app %q, want %q", name, imp.App, tc.want)
		}
	}
}

func TestPlanWarnsWhenMinimumScoreIsImpossible(t *testing.T) {
	min := 5000
	imp := QualityImport{
		Source: ImportSourceTrash,
		Profile: core.QualityProfile{
			Name:                 "Strict",
			AllowedQualities:     []string{"webdl-1080"},
			MinCustomFormatScore: &min,
		},
		FormatIDs: map[string]string{"Low": "low"},
	}
	found := map[string]core.CustomFormat{"low": format("low", "Low", map[string]int{"default": 100}, titleSpec)}
	cfg := config.Config{Quality: config.Quality{Profiles: []core.QualityProfile{{Name: "Other"}}}}
	plan := PlanQualityImport(imp, AppRadarr, found, cfg)
	if plan.Unusable != "" || !strings.Contains(strings.Join(plan.Warnings, " "), "at most 100") {
		t.Fatalf("unusable %q", plan.Unusable)
	}
	if plan.Profile.CustomFormatScores["low"] != 100 {
		t.Fatalf("scores %v", plan.Profile.CustomFormatScores)
	}
	if err := plan.Apply(&cfg, "Strict"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(cfg.Quality.Profiles) != 2 || len(cfg.Resolver.CustomFormats) != 1 {
		t.Fatalf("unusable import was saved: %+v", cfg)
	}
	// Exactly the best possible score is still reachable.
	min = 100
	plan = PlanQualityImport(imp, AppRadarr, found, cfg)
	if plan.Unusable != "" {
		t.Fatalf("minimum equal to the best score marked unusable: %q", plan.Unusable)
	}
}

func TestPlanExposesFetchedDefinitionsForSync(t *testing.T) {
	imp, found, cfg := planFixture()
	plan := PlanQualityImport(imp, AppRadarr, found, cfg)
	got := map[string]core.CustomFormat{}
	for _, f := range plan.Definitions {
		got[f.ID] = f
	}
	if len(got) != 3 || got["have"].Name != "Have" || got["new"].Name != "New" || got["plain"].Name != "Plain" {
		t.Fatalf("definitions %+v", plan.Definitions)
	}
	if got["have"].App != AppRadarr {
		t.Fatalf("definition app %q", got["have"].App)
	}
}

func TestQualityGateMatchesUnknownHeightBySource(t *testing.T) {
	profile := core.QualityProfile{AllowedQualities: []string{"webdl-1080"}}
	if got := rejectPolicy(core.Release{Info: core.ReleaseInfo{Source: core.SourceWEBDL}}, nil, profile, core.ClientCaps{}, false, false); got != "" {
		t.Fatalf("unknown-height WEB-DL rejected: %q", got)
	}
	profile.MinHeight = 1080
	if got := rejectPolicy(core.Release{Info: core.ReleaseInfo{Source: core.SourceWEBDL}}, nil, profile, core.ClientCaps{}, false, false); got != "" {
		t.Fatalf("min resolution not applied: %q", got)
	}
	profile.MinHeight = 0
	if got := rejectPolicy(core.Release{Info: core.ReleaseInfo{Source: core.SourceBluRay}}, nil, profile, core.ClientCaps{}, false, false); got != "" {
		t.Fatalf("other source allowed: %q", got)
	}
	profile.AllowedQualities = []string{"webdl-2160"}
	if got := rejectPolicy(core.Release{Info: core.ReleaseInfo{Source: core.SourceWEBDL}}, nil, profile, core.ClientCaps{}, false, false); got != "" {
		t.Fatalf("unknown height should match any height of its source: %q", got)
	}
	if got := rejectPolicy(core.Release{Info: core.ReleaseInfo{Source: core.SourceWEBDL, Height: 720}}, nil, profile, core.ClientCaps{}, false, false); got != "" {
		t.Fatalf("known wrong height allowed: %q", got)
	}
}
