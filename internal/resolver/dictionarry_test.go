package resolver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func buildDictionarryTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		data := []byte(files[name])
		hdr := &tar.Header{
			Name:     "database-v2/" + name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dictionarryTestFiles() map[string]string {
	return map[string]string{
		"pcd.json": `{
			"name": "Dictionarry Database",
			"version": "2.0.0",
			"dependencies": {
				"https://github.com/Dictionarry-Hub/schema": "1.1.0"
			}
		}`,
		"ops/0.rosettarr.sql": `
			INSERT INTO tags (name) VALUES ('2160p'), ('1080p'), ('HDR'), ('Lossless Audio'), ('Remux Focused'), ('Efficient Focused'), ('h265'), ('x265');

			INSERT INTO regular_expressions (name, pattern, description) VALUES
				('Remux', '\bRemux\b', 'Remux token'),
				('x265', '[xh][ ._-]?265|\bHEVC\b', 'HEVC/x265'),
				('Dolby Vision', '\b(dv|dovi|dolby[ .]?vision)\b', 'DV'),
				('HDR', '\bHDR(10)?\b', 'HDR'),
				('FraMeSToR', '(?<=^|[\s.-])FraMeSToR\b', 'Tier 1 remux group'),
				('QxR', '(?<=^|[\s.-])QxR\b', 'Efficient group');

			INSERT INTO custom_formats (name, description) VALUES
				('2160p Remux', 'Matches 2160p Remuxes'),
				('Remux Tier 1', 'Top remux groups'),
				('1080p Bluray', 'Matches 1080p Bluray non-remux'),
				('x265', 'Matches x265/HEVC'),
				('HDR', 'Matches HDR'),
				('Dolby Vision', 'Matches Dolby Vision'),
				('Original Language', 'Matches Original language'),
				('Freeleech 100%', 'Matches 100% freeleech'),
				('Season Pack', 'Matches season packs');

			-- 2160p Remux conditions
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('2160p Remux', '2160p', 'resolution', 'all', 0, 1),
				('2160p Remux', 'Remux Title', 'release_title', 'all', 0, 1),
				('2160p Remux', 'Remux Mod', 'quality_modifier', 'radarr', 0, 0),
				('2160p Remux', 'Remux Src', 'source', 'sonarr', 0, 0);
			INSERT INTO condition_resolutions (custom_format_name, condition_name, resolution) VALUES
				('2160p Remux', '2160p', '2160p');
			INSERT INTO condition_patterns (custom_format_name, condition_name, regular_expression_name) VALUES
				('2160p Remux', 'Remux Title', 'Remux');
			INSERT INTO condition_quality_modifiers (custom_format_name, condition_name, quality_modifier) VALUES
				('2160p Remux', 'Remux Mod', 'remux');
			INSERT INTO condition_sources (custom_format_name, condition_name, source) VALUES
				('2160p Remux', 'Remux Src', 'bluray_raw');

			-- Remux Tier 1 conditions
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('Remux Tier 1', 'FraMeSToR', 'release_group', 'all', 0, 1);
			INSERT INTO condition_patterns (custom_format_name, condition_name, regular_expression_name) VALUES
				('Remux Tier 1', 'FraMeSToR', 'FraMeSToR');

			-- 1080p Bluray conditions
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('1080p Bluray', '1080p', 'resolution', 'all', 0, 1),
				('1080p Bluray', 'Bluray', 'source', 'all', 0, 1),
				('1080p Bluray', 'Not Remux', 'release_title', 'all', 1, 1);
			INSERT INTO condition_resolutions (custom_format_name, condition_name, resolution) VALUES
				('1080p Bluray', '1080p', '1080p');
			INSERT INTO condition_sources (custom_format_name, condition_name, source) VALUES
				('1080p Bluray', 'Bluray', 'bluray');
			INSERT INTO condition_patterns (custom_format_name, condition_name, regular_expression_name) VALUES
				('1080p Bluray', 'Not Remux', 'Remux');

			-- x265 conditions
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('x265', 'x265', 'release_title', 'all', 0, 1),
				('x265', 'QxR', 'release_group', 'all', 0, 0);
			INSERT INTO condition_patterns (custom_format_name, condition_name, regular_expression_name) VALUES
				('x265', 'x265', 'x265'),
				('x265', 'QxR', 'QxR');

			-- HDR and Dolby Vision conditions
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('HDR', 'HDR', 'release_title', 'all', 0, 1),
				('Dolby Vision', 'DV', 'release_title', 'all', 0, 1);
			INSERT INTO condition_patterns (custom_format_name, condition_name, regular_expression_name) VALUES
				('HDR', 'HDR', 'HDR'),
				('Dolby Vision', 'DV', 'Dolby Vision');

			-- Original Language condition
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('Original Language', 'Not Original', 'language', 'all', 0, 1);
			INSERT INTO condition_languages (custom_format_name, condition_name, language_name, except_language) VALUES
				('Original Language', 'Not Original', 'Original', 1);

			-- Unsupported conditions (indexer_flag and release_type)
			INSERT INTO custom_format_conditions (custom_format_name, name, type, arr_type, negate, required) VALUES
				('Freeleech 100%', 'FL', 'indexer_flag', 'all', 0, 1),
				('Season Pack', 'SP', 'release_type', 'sonarr', 0, 1);
			INSERT INTO condition_indexer_flags (custom_format_name, condition_name, flag) VALUES
				('Freeleech 100%', 'FL', 'freeleech');
			INSERT INTO condition_release_types (custom_format_name, condition_name, release_type) VALUES
				('Season Pack', 'SP', 'season_pack');

			-- Profiles (initial score in 0.rosettarr.sql is updated by 1.update-scores.sql)
			INSERT INTO quality_profiles (name, description, upgrades_allowed, minimum_custom_format_score, upgrade_until_score, upgrade_score_increment) VALUES
				('2160p Remux', '2160p Remux utilizes **Video / Audio Formats** to prioritise lossless UHD Blurays.', 1, 20000, 888888, 1),
				('1080p Efficient', '1080p Efficient targets high quality [x265](https://dictionarry.dev) Bluray and WEB Encodes.', 1, 20000, 888888, 1);

			INSERT INTO quality_profile_tags (quality_profile_name, tag_name) VALUES
				('2160p Remux', '2160p'), ('2160p Remux', 'HDR'), ('2160p Remux', 'Lossless Audio'), ('2160p Remux', 'Remux Focused'), ('2160p Remux', 'h265'),
				('1080p Efficient', '1080p'), ('1080p Efficient', 'Efficient Focused'), ('1080p Efficient', 'h265'), ('1080p Efficient', 'x265');

			INSERT INTO quality_profile_languages (quality_profile_name, language_name, type) VALUES
				('2160p Remux', 'Any', 'simple'),
				('1080p Efficient', 'Original', 'must');

			INSERT INTO quality_groups (quality_profile_name, name) VALUES
				('2160p Remux', '2160p Remux'),
				('1080p Efficient', '1080p Efficient');

			INSERT INTO quality_group_members (quality_profile_name, quality_group_name, quality_name, position) VALUES
				('2160p Remux', '2160p Remux', 'Remux-2160p', 0),
				('2160p Remux', '2160p Remux', 'Bluray-2160p', 1),
				('2160p Remux', '2160p Remux', 'WEBDL-2160p', 2),
				('2160p Remux', '2160p Remux', 'Remux-1080p', 3),
				('2160p Remux', '2160p Remux', 'SDTV', 4),
				('1080p Efficient', '1080p Efficient', 'Bluray-1080p', 0),
				('1080p Efficient', '1080p Efficient', 'WEBDL-1080p', 1),
				('1080p Efficient', '1080p Efficient', 'WEBRip-1080p', 2),
				('1080p Efficient', '1080p Efficient', 'SDTV', 3);

			INSERT INTO quality_profile_qualities (quality_profile_name, quality_group_name, position, enabled, upgrade_until) VALUES
				('2160p Remux', '2160p Remux', 0, 1, 1),
				('1080p Efficient', '1080p Efficient', 0, 1, 1);

			INSERT INTO quality_profile_custom_formats (quality_profile_name, custom_format_name, arr_type, score) VALUES
				('2160p Remux', '2160p Remux', 'all', 500000),
				('2160p Remux', 'Remux Tier 1', 'radarr', 485000),
				('2160p Remux', 'Remux Tier 1', 'sonarr', 490000),
				('2160p Remux', 'HDR', 'all', 1000),
				('2160p Remux', 'Dolby Vision', 'all', 3000),
				('2160p Remux', 'Original Language', 'all', -999999),
				('2160p Remux', 'Freeleech 100%', 'all', 10),
				('1080p Efficient', '1080p Bluray', 'all', 280000),
				('1080p Efficient', 'x265', 'radarr', 20000),
				('1080p Efficient', 'x265', 'sonarr', 25000),
				('1080p Efficient', 'HDR', 'all', 1000),
				('1080p Efficient', 'Dolby Vision', 'all', 3000),
				('1080p Efficient', 'Season Pack', 'sonarr', 10);
		`,
		"ops/1.update-scores.sql": `
			UPDATE quality_profiles SET minimum_custom_format_score = 200000, upgrade_until_score = 1000000;
			UPDATE quality_profile_custom_formats SET score = 860000 WHERE quality_profile_name = '2160p Remux' AND custom_format_name = '2160p Remux';
		`,
	}
}

func TestDictionarryCompilesOrderedOpsAndResolvesProfiles(t *testing.T) {
	tarBytes := buildDictionarryTarGz(t, dictionarryTestFiles())
	var fetches atomic.Int32
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(tarBytes)
	}))
	t.Cleanup(srv.Close)

	now := time.Now()
	d := NewDictionarry(DictionarryOptions{
		ArchiveURL: srv.URL + "/database-v2.tar.gz",
		TTL:        time.Hour,
		Now:        func() time.Time { return now },
	})

	profiles, err := d.Profiles(context.Background(), AppRadarr)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].ID != "2160p-remux" || profiles[1].ID != "1080p-efficient" {
		t.Fatalf("unexpected profiles: %+v", profiles)
	}
	if profiles[0].Description != "2160p Remux utilizes Video / Audio Formats to prioritise lossless UHD Blurays." {
		t.Fatalf("unexpected description: %q", profiles[0].Description)
	}
	if profiles[1].Description != "1080p Efficient targets high quality x265 Bluray and WEB Encodes." {
		t.Fatalf("unexpected markdown link cleanup: %q", profiles[1].Description)
	}

	// Second call within TTL uses cache.
	if _, err := d.Profiles(context.Background(), AppSonarr); err != nil {
		t.Fatal(err)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("expected 1 archive fetch, got %d", got)
	}

	// Verify 2160p Remux for Radarr reflects op 1's updated score (860000 and minScore 200000).
	imp, found, err := d.Import(context.Background(), AppRadarr, "2160p-remux")
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanQualityImport(imp, AppRadarr, found, config.Config{})
	if plan.Profile.Name != "2160p Remux" || *plan.Profile.MinCustomFormatScore != 200000 {
		t.Fatalf("unexpected 2160p Remux profile: %+v", plan.Profile)
	}
	if !plan.Profile.AllowHDR || !plan.Profile.AllowDolbyVision || plan.Profile.PreferredCodecs[0] != "hevc" || plan.Profile.PreferredAudio[0] != "truehd" {
		t.Fatalf("expected UHD HDR/DV, hevc, and lossless audio on 2160p Remux: %+v", plan.Profile)
	}
	if plan.Profile.CustomFormatScores["dictionarry-radarr-2160p-remux"] != 860000 {
		t.Fatalf("expected op 1 score 860000 on 2160p Remux, got %v", plan.Profile.CustomFormatScores)
	}
	if plan.Profile.CustomFormatScores["dictionarry-radarr-remux-tier-1"] != 485000 {
		t.Fatalf("expected radarr score 485000 on Remux Tier 1, got %v", plan.Profile.CustomFormatScores)
	}
	// Freeleech 100% should be reported as FormatUnsupported.
	var sawFreeleech bool
	for _, fm := range plan.Formats {
		if fm.Name == "Freeleech 100%" {
			sawFreeleech = true
			if fm.Status != FormatUnsupported || !strings.Contains(fm.Reason, "indexer flags") {
				t.Fatalf("expected Freeleech 100%% unsupported due to indexer flags, got %+v", fm)
			}
		}
	}
	if !sawFreeleech {
		t.Fatalf("Freeleech 100%% missing from formats: %+v", plan.Formats)
	}

	// Verify 1080p Efficient for Sonarr gets Sonarr's score (25000 for x265),
	// allows HDR & Dolby Vision, prefers HEVC, and marks Season Pack unsupported.
	impS, foundS, err := d.Import(context.Background(), AppSonarr, "1080p Efficient")
	if err != nil {
		t.Fatal(err)
	}
	planS := PlanQualityImport(impS, AppSonarr, foundS, config.Config{})
	if !planS.Profile.AllowHDR || !planS.Profile.AllowDolbyVision || planS.Profile.PreferredCodecs[0] != "hevc" {
		t.Fatalf("expected 1080p Efficient to allow HDR/DV and prefer HEVC: %+v", planS.Profile)
	}
	if planS.Profile.CustomFormatScores["dictionarry-sonarr-x265"] != 25000 {
		t.Fatalf("expected sonarr score 25000 on x265, got %v", planS.Profile.CustomFormatScores)
	}
	var sawSeasonPack bool
	for _, fm := range planS.Formats {
		if fm.Name == "Season Pack" {
			sawSeasonPack = true
			if fm.Status != FormatUnsupported || !strings.Contains(fm.Reason, "release type") {
				t.Fatalf("expected Season Pack unsupported due to release type, got %+v", fm)
			}
		}
	}
	if !sawSeasonPack {
		t.Fatalf("Season Pack missing from Sonarr formats: %+v", planS.Formats)
	}

	// Verify the compiled custom formats actually score a release in FormatScorer!
	var cfg config.Config
	if err := plan.Apply(&cfg, ""); err != nil {
		t.Fatal(err)
	}
	scorer, err := NewFormatScorer(cfg.Resolver.CustomFormats, cfg.Quality.Profiles[0].CustomFormatScores)
	if err != nil {
		t.Fatal(err)
	}
	score, matches, err := scorer.ScoreForTitle(core.Release{
		Name: "Dune.Part.Two.2024.2160p.UHD.BluRay.Remux.DV.HDR10.TrueHD.7.1.Atmos.HEVC-FraMeSToR",
		Info: core.ReleaseInfo{
			Height:    2160,
			Source:    core.SourceRemux,
			Group:     "FraMeSToR",
			Languages: []string{"english"},
		},
	}, nil, "english")
	if err != nil {
		t.Fatal(err)
	}
	// 2160p Remux (860000) + Remux Tier 1 (485000) + Dolby Vision (3000) + HDR (1000) = 1349000.
	if score != 1349000 || len(matches) != 4 {
		t.Fatalf("unexpected score=%d matches=%+v", score, matches)
	}

	// Stale cache is served when upstream fails after TTL.
	now = now.Add(2 * time.Hour)
	down.Store(true)
	if stale, err := d.Profiles(context.Background(), AppRadarr); err != nil || len(stale) != 2 {
		t.Fatalf("expected stale cache on upstream failure, got %v %v", stale, err)
	}
}

func TestDictionarryRejectsUnsafeSQLAndDuplicateOps(t *testing.T) {
	badSQL := dictionarryTestFiles()
	badSQL["ops/2.evil.sql"] = `ATTACH DATABASE ':memory:' AS evil;`
	if _, err := compileDictionarryArchive(context.Background(), buildDictionarryTarGz(t, badSQL)); !errors.Is(err, ErrDictionarryUnavailable) {
		t.Fatalf("expected unsafe SQL rejection, got %v", err)
	}

	dupSeq := dictionarryTestFiles()
	dupSeq["ops/1.duplicate.sql"] = `INSERT INTO tags (name) VALUES ('dup');`
	if _, err := compileDictionarryArchive(context.Background(), buildDictionarryTarGz(t, dupSeq)); !errors.Is(err, ErrDictionarryUnavailable) {
		t.Fatalf("expected duplicate sequence rejection, got %v", err)
	}
}

func TestDictionarryCompilesBaseSQLDataset(t *testing.T) {
	basePath := "/Users/braedonsaunders/.bb/thread-storage/thr_v375b2ntiq/issue128/base.sql"
	baseBytes, err := os.ReadFile(basePath)
	if err != nil {
		t.Skip("local base.sql fixture not present")
	}
	files := map[string]string{
		"pcd.json":            `{"dependencies":{"https://github.com/Dictionarry-Hub/schema":"1.1.0"}}`,
		"ops/0.rosettarr.sql": string(baseBytes),
	}
	cat, err := compileDictionarryArchive(context.Background(), buildDictionarryTarGz(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.profiles[AppRadarr]) != 11 || len(cat.profiles[AppSonarr]) != 11 {
		t.Fatalf("expected 11 radarr and 11 sonarr profiles from base.sql, got %d / %d", len(cat.profiles[AppRadarr]), len(cat.profiles[AppSonarr]))
	}
	for _, app := range []string{AppRadarr, AppSonarr} {
		for _, want := range []string{"2160p-remux", "1080p-efficient"} {
			imp, ok := cat.imports[app][want]
			if !ok {
				t.Fatalf("missing %s in %s", want, app)
			}
			plan := PlanQualityImport(imp, app, cat.formats[app], config.Config{})
			var cfg config.Config
			if err := plan.Apply(&cfg, ""); err != nil {
				t.Fatalf("Apply %s (%s): %v", want, app, err)
			}
			if _, err := NewFormatScorer(cfg.Resolver.CustomFormats, cfg.Quality.Profiles[0].CustomFormatScores); err != nil {
				t.Fatalf("NewFormatScorer %s (%s): %v", want, app, err)
			}
		}
	}
}
