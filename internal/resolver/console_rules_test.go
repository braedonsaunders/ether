package resolver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPreferencePreviewAndPlaybackAgreeWithoutSaving(t *testing.T) {
	f := newConsoleFixture(t)
	defer f.svc.Close()
	p, _, _ := f.svc.consoleProfile("")
	p.ReleaseRules = []core.ReleaseRule{{ID: "group", Name: "Prefer 720", Action: "prefer", Conditions: []core.ReleaseCondition{{Field: "height", Operator: "range", Min: 720, Max: 720}}}}
	body, _ := json.Marshal(map[string]any{"profile": p, "key": matrixTitle().Key.String()})
	var preview struct {
		Releases []ConsoleRelease `json:"releases"`
		Warning  string           `json:"warning"`
	}
	if status := f.do(t, "POST", "/rules/preview", string(body), &preview); status != 200 {
		t.Fatalf("status %d", status)
	}
	rows := preview.Releases
	if len(rows) == 0 || rows[0].Release.Info.Height != 720 || rows[0].Rank != 1 {
		t.Fatalf("preview %+v", rows)
	}
	cfg := testCfg()
	cfg.Quality.Profiles = []core.QualityProfile{p}
	cfg.Quality.Default = p.Name
	svc := New(&fakeIndexer{releases: movieReleases()}, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	actual, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: plainCaps()})
	if err != nil {
		t.Fatal(err)
	}
	if actual.Release.ID != rows[0].Release.ID {
		t.Fatalf("preview %s playback %s", rows[0].Release.ID, actual.Release.ID)
	}
	if n := len(f.svc.Pins()); n != 0 {
		t.Fatalf("preview changed pins: %d", n)
	}
	saved, _, _ := f.svc.consoleProfile("")
	if len(saved.ReleaseRules) > 0 {
		t.Fatal("preview saved the draft")
	}
	for _, row := range rows {
		if row.Release.NZBURL != "" || row.Release.MagnetURL != "" || row.Release.StreamURL != "" || row.Release.StreamHeaders != nil {
			t.Fatal("secret-bearing link in preview")
		}
	}
}
func TestPreferencePreviewRejectsInvalidDraftAndMissingLanguage(t *testing.T) {
	f := newConsoleFixture(t)
	defer f.svc.Close()
	p, _, _ := f.svc.consoleProfile("")
	p.ReleaseRules = []core.ReleaseRule{{ID: "lang", Name: "Norwegian", Action: "require", Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "no"}}}}
	body, _ := json.Marshal(map[string]any{"profile": p, "names": []string{"Film.2026.1080p.NORDIC.WEB-DL"}})
	var preview struct {
		Releases []ConsoleRelease `json:"releases"`
	}
	if f.do(t, "POST", "/rules/preview", string(body), &preview) != 200 || len(preview.Releases) != 1 || !preview.Releases[0].Rejected || !preview.Releases[0].Preferences.Unknown {
		t.Fatalf("%+v", preview)
	}
	p.ReleaseRules[0].Conditions[0] = core.ReleaseCondition{Field: "name", Operator: "regex", Value: "(?=x)"}
	body, _ = json.Marshal(map[string]any{"profile": p, "names": []string{"x"}})
	if f.do(t, "POST", "/rules/preview", string(body), nil) != 400 {
		t.Fatal("invalid regex accepted")
	}
}

func TestFilenamePreviewDoesNotInventUsenet(t *testing.T) {
	f := newConsoleFixture(t)
	defer f.svc.Close()
	p, _, _ := f.svc.consoleProfile("")
	p.ReleaseRules = []core.ReleaseRule{{ID: "proto", Name: "Usenet", Action: "require", Conditions: []core.ReleaseCondition{{Field: "protocol", Operator: "equals", Value: "usenet"}}}}
	body, _ := json.Marshal(map[string]any{"profile": p, "names": []string{"Film.2026.1080p.WEB-DL"}})
	var preview struct {
		Releases []ConsoleRelease `json:"releases"`
	}
	if f.do(t, "POST", "/rules/preview", string(body), &preview) != 200 || len(preview.Releases) != 1 {
		t.Fatalf("%+v", preview)
	}
	row := preview.Releases[0]
	if !row.Rejected || row.Preferences == nil || !row.Preferences.Unknown {
		t.Fatalf("filename protocol %+v", row)
	}
	if row.Release.Protocol != "" {
		t.Fatalf("preview protocol %q", row.Release.Protocol)
	}
	if len(row.Preferences.Explanations) != 1 || row.Preferences.Explanations[0].Outcome != "unknown" {
		t.Fatalf("explanations %+v", row.Preferences.Explanations)
	}
}

func corfaceRules() []core.ReleaseRule {
	return []core.ReleaseRule{
		{
			ID: "audio-or-tag", Name: "Norwegian audio or Nordic name", Action: "prefer", Match: "any",
			Conditions: []core.ReleaseCondition{
				{Field: "language", Operator: "equals", Value: "no"},
				{Field: "name", Operator: "regex", Value: `\bNORDIC\b`},
			},
		},
		{
			ID: "subs-and-original", Name: "Nordic subs with original audio", Action: "prefer", Match: "all",
			Conditions: []core.ReleaseCondition{
				{Field: "subtitles", Operator: "equals", Value: "no"},
				{Field: "audio_matches_original", Operator: "equals"},
			},
		},
	}
}

func signalReleaseInfo(height int, audio, subs []string) core.ReleaseInfo {
	return core.ReleaseInfo{
		Title: "The Signal", Year: 1999, Height: height, Source: core.SourceWEBDL,
		VideoCodec: "h264", Languages: audio, SubtitleLanguages: subs,
	}
}

func TestOriginalAudioPreviewMatchesResolve(t *testing.T) {
	title := matrixTitle()
	title.Language = "ja"
	nordic := rel("nordic", "The.Signal.1999.1080p.WEB-DL.NORDIC-GRP", 8000)
	nordic.Info = signalReleaseInfo(1080, nil, nil)
	original := rel("original", "The.Signal.1999.720p.WEB-DL-GRP", 4000)
	original.Info = signalReleaseInfo(720, []string{"japanese"}, []string{"norwegian"})
	other := rel("other", "The.Signal.1999.1080p.WEB-DL.ENG-GRP", 9000)
	other.Info = signalReleaseInfo(1080, []string{"english"}, nil)
	rules := corfaceRules()
	cfg := testCfg()
	cfg.Resolver.MaxCandidates = 5
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].ReleaseRules = rules
	}
	svc := New(&fakeIndexer{releases: []core.Release{other, original, nordic}}, fakeHealth{}, openTestDB(t), cfg, Options{})
	t.Cleanup(func() { svc.Close() })
	h := svc.ConsoleHandler(ConsoleDeps{
		Catalog: fakeCatalog{titles: map[core.RatingKey]*core.Title{title.Key: title}},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p, _, _ := svc.consoleProfile("")
	p.ReleaseRules = rules
	body, _ := json.Marshal(map[string]any{"profile": p, "key": title.Key.String()})
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/rules/preview", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var preview struct {
		Releases []ConsoleRelease `json:"releases"`
		Note     string           `json:"note"`
	}
	if err := json.NewDecoder(res.Body).Decode(&preview); err != nil || res.StatusCode != 200 {
		t.Fatalf("preview %d %v", res.StatusCode, err)
	}
	if preview.Note != "" || len(preview.Releases) != 3 {
		t.Fatalf("title preview %+v", preview)
	}
	got := []string{preview.Releases[0].Release.ID, preview.Releases[1].Release.ID, preview.Releases[2].Release.ID}
	if got[0] != "nordic" || got[1] != "original" || got[2] != "other" {
		t.Fatalf("preview order %v", got)
	}
	actual, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: title, Client: plainCaps()})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{actual.Release.ID}
	for _, alt := range actual.Alternates {
		ids = append(ids, alt.ID)
	}
	if len(ids) < 3 || ids[0] != got[0] || ids[1] != got[1] || ids[2] != got[2] {
		t.Fatalf("resolve %v preview %v", ids, got)
	}
	names, _ := json.Marshal(map[string]any{"profile": p, "names": []string{nordic.Name, original.Name, other.Name}})
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/rules/preview", strings.NewReader(string(names)))
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	preview.Releases = nil
	preview.Note = ""
	if err := json.NewDecoder(res.Body).Decode(&preview); err != nil || preview.Note != "original_language_unavailable" {
		t.Fatalf("filename note %q err %v", preview.Note, err)
	}
	for _, row := range preview.Releases {
		if row.Release.ID == "1" || strings.Contains(row.Release.Name, "720p") {
			if row.Preferences == nil || len(row.Preferences.Preferences) < 2 || row.Preferences.Preferences[1] != 0 {
				t.Fatalf("filename preview treated original audio as known: %+v", row.Preferences)
			}
		}
	}
}
