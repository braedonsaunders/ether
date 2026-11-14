package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type syncFixture struct {
	mu     sync.Mutex
	cfg    config.Config
	gh     *fakeGitHub
	now    time.Time
	saves  int
	syncer *QualitySyncer
}

func newSyncFixture(t *testing.T, tree map[string]string, cfg config.Config) *syncFixture {
	t.Helper()
	gh := newFakeGitHub(t, tree)
	f := &syncFixture{gh: gh, now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), cfg: cfg}
	f.syncer = NewQualitySyncer(QualitySyncDeps{
		Guides: gh.guides(nil),
		Config: func() config.Config {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.cfg
		},
		Save: func(mutate func(*config.Config) error) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			next := f.cfg
			if err := mutate(&next); err != nil {
				return err
			}
			if _, err := NewFormatScorer(next.Resolver.CustomFormats); err != nil {
				return fmt.Errorf("%w: %v", ErrImportRejected, err)
			}
			f.cfg = next
			f.saves++
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return f.now },
	})
	return f
}

func trashImported(name, profileID string) core.QualityProfile {
	return core.QualityProfile{
		Name:      name,
		MediaType: core.MediaMovie,
		Import:    &core.ProfileImport{Source: ImportSourceTrash, App: AppRadarr, Profile: profileID},
	}
}

func TestQualitySyncUpdatesProfileAndDefinitionsExactly(t *testing.T) {
	cfg := config.Config{Quality: config.Quality{
		Profiles: []core.QualityProfile{
			func() core.QualityProfile {
				p := trashImported("My HD", "hd-bluray-web")
				p.Import.TrashID = "p1"
				p.CustomFormatScores = map[string]int{"cf-web1": 1, "cf-gone": 50}
				p.MinSizeMBPerMin = 7
				p.CustomFormatScores[core.FormatAudioTranscode] = -99
				return p
			}(),
			// A backfilled 0 must not keep an unused managed format alive.
			{Name: "Manual", CustomFormatScores: map[string]int{"cf-manual": 5, "cf-old": 0}},
		},
	}, Resolver: config.Resolver{CustomFormats: []core.CustomFormat{
		func() core.CustomFormat {
			f := format("cf-web1", "Old WEB", map[string]int{"default": 1}, titleSpec)
			f.ManagedBy = "trash:radarr"
			return f
		}(),
		func() core.CustomFormat {
			f := format("cf-old", "Old managed", map[string]int{"default": 2}, titleSpec)
			f.ManagedBy = "trash:radarr"
			return f
		}(),
		format("cf-manual", "Manual format", map[string]int{"default": 3}, titleSpec),
	}}}
	f := newSyncFixture(t, trashTree(), cfg)
	report, err := f.syncer.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 || report.ProfilesUpdated != 1 || report.FormatsAdded != 1 || report.FormatsUpdated != 1 || report.FormatsRemoved != 1 {
		t.Fatalf("report %+v", report)
	}
	prof := f.cfg.Quality.Profiles[0]
	// The guide's scores exactly, plus Ether's streaming formats with the
	// owner's own tuning of them kept.
	wantScores := core.StreamingFormatScores()
	wantScores["cf-web1"], wantScores["cf-brdisk"], wantScores[core.FormatAudioTranscode] = 1700, -10000, -99
	if !maps.Equal(prof.CustomFormatScores, wantScores) {
		t.Fatalf("scores %v, want exactly %v", prof.CustomFormatScores, wantScores)
	}
	if prof.MediaType != core.MediaMovie || prof.Import == nil || prof.Import.SyncedAt != f.now || prof.Import.Profile != "hd-bluray-web" {
		t.Fatalf("profile provenance %+v", prof)
	}
	if !slices.Equal(prof.AllowedQualities, []string{"bluray-1080", "webrip-1080", "webdl-1080"}) {
		t.Fatalf("qualities %v", prof.AllowedQualities)
	}
	if prof.MinSizeMBPerMin != 7 || prof.CustomFormatScores[core.FormatAudioTranscode] != -99 {
		t.Fatalf("owner tuning lost: %+v", prof)
	}
	defs := map[string]core.CustomFormat{}
	for _, d := range f.cfg.Resolver.CustomFormats {
		defs[d.ID] = d
	}
	if len(defs) != 3 {
		t.Fatalf("definitions %+v", f.cfg.Resolver.CustomFormats)
	}
	if d := defs["cf-web1"]; d.Name != "WEB Tier 01" || d.App != AppRadarr || d.ManagedBy != "trash:radarr" || d.Scores["sqp-1-2160p"] != 1800 {
		t.Fatalf("upserted definition %+v", d)
	}
	if d := defs["cf-brdisk"]; d.ManagedBy != "trash:radarr" {
		t.Fatalf("added definition %+v", d)
	}
	if d := defs["cf-manual"]; d.ManagedBy != "" || d.Name != "Manual format" {
		t.Fatalf("manual definition touched: %+v", d)
	}
	if _, ok := defs["cf-old"]; ok {
		t.Fatal("unreferenced managed definition kept")
	}
	if manual := f.cfg.Quality.Profiles[1].CustomFormatScores; len(manual) != 1 || manual["cf-manual"] != 5 {
		t.Fatalf("dangling 0 score for a removed definition kept: %v", manual)
	}
	if f.cfg.Quality.Sync.LastRun != f.now || f.cfg.Quality.Sync.LastError != "" {
		t.Fatalf("sync state %+v", f.cfg.Quality.Sync)
	}
	if _, err := NewFormatScorer(f.cfg.Resolver.CustomFormats); err != nil {
		t.Fatalf("sync broke scoring: %v", err)
	}

	// A second run changes nothing at the source.
	report, err = f.syncer.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ProfilesUpdated != 0 || report.FormatsAdded != 0 || report.FormatsUpdated != 0 || report.FormatsRemoved != 0 || len(report.Warnings) != 0 {
		t.Fatalf("second report %+v", report)
	}
}

func TestQualitySyncSkipsFailedAndUnusableProfiles(t *testing.T) {
	tree := trashTree()
	tree["radarr/quality-profiles/impossible.json"] = `{"trash_id":"p9","name":"Impossible","minFormatScore":999999,
		"items":[{"name":"WEBDL-1080p","allowed":true}],"formatItems":{"WEB Tier 01":"cf-web1"}}`
	tree["radarr/quality-profiles/unusable.json"] = `{"trash_id":"p10","name":"Unusable","items":[{"name":"WEBDL-1080p","allowed":false}]}`
	unusable := trashImported("Unusable", "unusable")
	unusable.CustomFormatScores = map[string]int{"cf-web1": 9}
	bad := trashImported("Bad", "missing")
	bad.CustomFormatScores = map[string]int{"cf-web1": 4}
	impossible := trashImported("Impossible", "impossible")
	impossible.CustomFormatScores = map[string]int{"cf-web1": 8}
	cfg := config.Config{Quality: config.Quality{Profiles: []core.QualityProfile{
		trashImported("Good", "hd-bluray-web"),
		bad,
		impossible,
		unusable,
	}}}
	f := newSyncFixture(t, tree, cfg)
	report, err := f.syncer.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ProfilesUpdated != 2 || len(report.Warnings) != 3 {
		t.Fatalf("report %+v", report)
	}
	joined := strings.Join(report.Warnings, "\n")
	if !strings.Contains(joined, "Bad") || !strings.Contains(joined, "no such profile") && !strings.Contains(joined, "no longer exists") {
		t.Fatalf("warnings %v", report.Warnings)
	}
	if !strings.Contains(joined, "Impossible") || !strings.Contains(joined, "at most 1700") {
		t.Fatalf("minimum warning missing: %v", report.Warnings)
	}
	if got := f.cfg.Quality.Profiles[1]; got.CustomFormatScores["cf-web1"] != 4 || got.Import.SyncedAt.IsZero() != true {
		t.Fatalf("failed profile overwritten: %+v", got)
	}
	if got := f.cfg.Quality.Profiles[2]; got.CustomFormatScores["cf-web1"] != 1700 || got.Import.SyncedAt.IsZero() {
		t.Fatalf("unreachable minimum profile was not updated: %+v", got)
	}
	if got := f.cfg.Quality.Profiles[3]; got.CustomFormatScores["cf-web1"] != 9 || !got.Import.SyncedAt.IsZero() {
		t.Fatalf("unusable profile overwritten: %+v", got)
	}
	if f.cfg.Quality.Sync.LastError == "" {
		t.Fatal("LastError should report the skipped profiles")
	}
}

func TestQualitySyncSonarrSetsTVMedia(t *testing.T) {
	cfg := config.Config{Quality: config.Quality{
		Sync: config.QualitySync{Enabled: true},
		Profiles: []core.QualityProfile{
			{Name: "Shows", Import: &core.ProfileImport{Source: ImportSourceTrash, App: AppSonarr, Profile: "web-1080p", TrashID: "s1"}},
		},
	}}
	f := newSyncFixture(t, trashTree(), cfg)
	report, err := f.syncer.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 || report.FormatsAdded != 1 {
		t.Fatalf("report %+v", report)
	}
	prof := f.cfg.Quality.Profiles[0]
	if prof.MediaType != core.MediaTV || prof.Import.App != AppSonarr {
		t.Fatalf("profile %+v", prof)
	}
	if f.cfg.Quality.Sync.Interval.D() != 24*time.Hour {
		t.Fatalf("missing interval not defaulted: %v", f.cfg.Quality.Sync.Interval)
	}
	for _, d := range f.cfg.Resolver.CustomFormats {
		if d.ID == "scf-remux1" && (d.App != AppSonarr || d.ManagedBy != "trash:sonarr") {
			t.Fatalf("definition %+v", d)
		}
	}
}

func TestQualitySyncDictionarry(t *testing.T) {
	dictBytes := buildDictionarryTarGz(t, dictionarryTestFiles())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(dictBytes)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Config{Quality: config.Quality{Profiles: []core.QualityProfile{
		{Name: "Profilarr", Import: &core.ProfileImport{Source: ImportSourceDictionarry, App: AppRadarr, Profile: "2160p-remux"}},
	}}}
	f := newSyncFixture(t, trashTree(), cfg)
	f.syncer = NewQualitySyncer(QualitySyncDeps{
		Dictionarry: NewDictionarry(DictionarryOptions{ArchiveURL: srv.URL + "/database-v2.tar.gz", UserAgent: "Ether/test"}),
		Config:      func() config.Config { return f.cfg },
		Save: func(mutate func(*config.Config) error) error {
			next := f.cfg
			if err := mutate(&next); err != nil {
				return err
			}
			if _, err := NewFormatScorer(next.Resolver.CustomFormats); err != nil {
				return fmt.Errorf("%w: %v", ErrImportRejected, err)
			}
			f.cfg = next
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return f.now },
	})
	report, err := f.syncer.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 || report.ProfilesUpdated != 1 || report.FormatsAdded == 0 {
		t.Fatalf("report %+v", report)
	}
	prof := f.cfg.Quality.Profiles[0]
	if prof.MediaType != core.MediaMovie || prof.CustomFormatScores["dictionarry-radarr-2160p-remux"] != 860000 {
		t.Fatalf("profile %+v", prof)
	}
	for _, d := range f.cfg.Resolver.CustomFormats {
		if d.ManagedBy != "dictionarry:radarr" {
			t.Fatalf("definition %+v", d)
		}
	}
}

func TestSyncDue(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if SyncDue(config.QualitySync{Enabled: false, LastRun: now.Add(-100 * time.Hour)}, now) {
		t.Fatal("disabled sync is due")
	}
	if !SyncDue(config.QualitySync{Enabled: true}, now) {
		t.Fatal("a sync with no start time should be due")
	}
	due := config.QualitySync{Enabled: true, Interval: config.Duration(time.Hour), LastRun: now.Add(-30 * time.Minute)}
	if SyncDue(due, now) {
		t.Fatal("sync due before its interval")
	}
	if !SyncDue(due, now.Add(30*time.Minute)) {
		t.Fatal("sync not due at its interval")
	}
	due.Interval = 0
	if SyncDue(config.QualitySync{Enabled: true, LastRun: now.Add(-23 * time.Hour)}, now) {
		t.Fatal("missing interval should default to 24h")
	}
	if !SyncDue(config.QualitySync{Enabled: true, LastRun: now.Add(-24 * time.Hour)}, now) {
		t.Fatal("default interval not applied")
	}
}

func TestQualitySyncSkipsPartialGroupFailureWithoutOverwrite(t *testing.T) {
	cfg := config.Config{Quality: config.Quality{Profiles: []core.QualityProfile{
		func() core.QualityProfile {
			p := trashImported("Base", "base-profile")
			p.Import.TrashID = "p3"
			p.CustomFormatScores = map[string]int{"kept": 9}
			return p
		}(),
	}}}
	f := newSyncFixture(t, trashGroupTree(), cfg)
	f.gh.fail = map[string]bool{"radarr/cf-groups/release-groups.json": true}
	report, err := f.syncer.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ProfilesUpdated != 0 || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "default format groups") {
		t.Fatalf("report %+v", report)
	}
	if got := f.cfg.Quality.Profiles[0]; got.CustomFormatScores["kept"] != 9 || len(got.CustomFormatScores) != 1 {
		t.Fatalf("profile overwritten from a partial read: %+v", got)
	}
}

func TestSameFormatDefinitionIgnoresRawJSONWhitespace(t *testing.T) {
	spec := func(raw string) core.CustomFormat {
		return core.CustomFormat{
			ID: "same", Name: "Same", App: AppRadarr, ManagedBy: "trash:radarr",
			Scores: map[string]int{"default": 10},
			Specifications: []core.FormatSpecification{{
				Name: "Rule", Implementation: "ReleaseTitleSpecification", Required: true,
				Fields: map[string]json.RawMessage{"value": json.RawMessage(raw)},
			}},
		}
	}
	a := spec(`"foo"`)
	b := spec(` "foo" `)
	if !sameFormatDefinition(a, b) {
		t.Fatal("raw JSON whitespace must not count as a definition change")
	}
	b.Specifications[0].Fields["value"] = json.RawMessage(`"bar"`)
	if sameFormatDefinition(a, b) {
		t.Fatal("a different regex must count as a definition change")
	}
}

// Shared manual definitions and same-name replacements remain owner-owned.
func TestQualitySyncRespectsOwnership(t *testing.T) {
	manual := format("shared", "Owner definition", map[string]int{"default": 42}, titleSpec)
	c := config.Config{Quality: config.Quality{Profiles: []core.QualityProfile{trashImported("Imported", "hd")}}, Resolver: config.Resolver{CustomFormats: []core.CustomFormat{manual}}}
	saved := *c.Quality.Profiles[0].Import
	plan := QualityImportPlan{App: AppRadarr, Source: ImportSourceTrash, ProfileID: "hd", Definitions: []core.CustomFormat{format("shared", "Guide definition", nil, titleSpec)}}
	s := NewQualitySyncer(QualitySyncDeps{})
	report := QualitySyncReport{}
	s.apply(&c, "Imported", core.MediaMovie, saved, plan, time.Now(), &report)
	if c.Resolver.CustomFormats[0].Name != manual.Name || c.Resolver.CustomFormats[0].ManagedBy != "" {
		t.Fatal("manual format overwritten")
	}
	c.Quality.Profiles[0] = core.QualityProfile{Name: "Imported", MediaType: core.MediaMovie, MaxHeight: 720}
	s.apply(&c, "Imported", core.MediaMovie, saved, plan, time.Now(), &report)
	if c.Quality.Profiles[0].Import != nil || c.Quality.Profiles[0].MaxHeight != 720 {
		t.Fatal("manual replacement overwritten")
	}
}
