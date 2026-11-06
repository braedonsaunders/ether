package resolver

// Live Servarr parity check. Skipped unless ETHER_SERVARR_LIVE=1.
//
// Read-only: GETs /api/v3/customformat, /qualityprofile, /movie|/series and
// /parse against a running Radarr/Sonarr, then scores the same titles with
// Ether's FormatScorer using the instance's own custom formats and the
// matched item's quality profile scores, and writes a table.
//
// Environment (never printed):
//
//	RADARR_URL / RADARR_API_KEY, SONARR_URL / SONARR_API_KEY
//	ETHER_SERVARR_OUT  output markdown path (default $BB_THREAD_STORAGE/servarr-parity.md)
//
// Run: set -a; . .cache/arr.env; set +a
//
//	ETHER_SERVARR_LIVE=1 RADARR_URL=http://127.0.0.1:17878 SONARR_URL=http://127.0.0.1:17989 \
//	  go test ./internal/resolver -run TestLiveServarrParity -count=1 -v

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

type servarrField struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}
type servarrSpec struct {
	Name           string         `json:"name"`
	Implementation string         `json:"implementation"`
	Negate         bool           `json:"negate"`
	Required       bool           `json:"required"`
	Fields         []servarrField `json:"fields"`
}
type servarrCF struct {
	ID             int           `json:"id"`
	Name           string        `json:"name"`
	Specifications []servarrSpec `json:"specifications"`
}
type servarrProfile struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	MinFormatScore  int    `json:"minFormatScore"`
	CutoffFormatScr int    `json:"cutoffFormatScore"`
	FormatItems     []struct {
		Format int `json:"format"`
		Score  int `json:"score"`
	} `json:"formatItems"`
}

func servarrGet(t *testing.T, base, key, path string, out any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	req.Header.Set("X-Api-Key", key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: %v", strings.SplitN(path, "?", 2)[0], err)
	}
}

func TestLiveServarrParity(t *testing.T) {
	if os.Getenv("ETHER_SERVARR_LIVE") != "1" {
		t.Skip("set ETHER_SERVARR_LIVE=1 to compare against a live Radarr/Sonarr")
	}
	out := os.Getenv("ETHER_SERVARR_OUT")
	if out == "" {
		out = filepath.Join(os.Getenv("BB_THREAD_STORAGE"), "servarr-parity.md")
	}
	var report strings.Builder
	report.WriteString("# Ether vs Servarr custom-format scoring (live)\n\n")
	mismatches, rows := 0, 0

	radarrSuffix := []string{
		"2160p.UHD.BluRay.REMUX.HDR.HEVC.TrueHD.7.1.Atmos-FraMeSToR", "1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-BiZKiT",
		"2160p.UHD.BluRay.x265.HDR-TERMiNAL", "1080p.BluRay.x264-DON", "REPACK.1080p.BluRay.x264-DON",
		"PROPER.1080p.BluRay.x264-NOGRP", "2160p.WEB-DL.DDP5.1.HDR.HEVC-FLUX", "1080p.AMZN.WEB-DL.DDP5.1.H.264-ABBiE",
		"1080p.NF.WEB-DL.DDP5.1.H.264-NOGRP", "1080p.WEB-DL.DDP5.1.H.264-CMRG", "1080p.WEB-DL.DDP5.1.H.264-Cinefile",
		"1080p.BluRay.x264-NOGRP", "720p.HDTV.x264-GRP", "1080p.BluRay.x264.Hybrid-GRP", "MULTi.1080p.WEB-DL.H.264-GRP",
		"German.DL.1080p.BluRay.x264-GRP", "2160p.BluRay.x265.DV.HDR10-GRP", "1080p.WEBRip.x265-GRP",
		"GERMAN.ML.1080p.WEB.x264-GRP", "MULTi.FRENCH.1080p.BluRay.x264-GRP", "VOSTFR.1080p.WEB-DL.H.264-GRP",
		"TRUEFRENCH.1080p.BluRay.x264-GRP", "1080p.BluRay.x264-DON.mkv", "2160p.WEB-DL.DV.HDR10Plus.DDP5.1.Atmos-FLUX",
		"Directors.Cut.1080p.BluRay.x264-GRP", "IMAX.2160p.UHD.BluRay.x265-GRP", "German.1080p.WEB-DL.H.264-GRP",
		"Japanese.1080p.BluRay.x264-GRP", "DUAL.1080p.WEB-DL.H.264-GRP",
	}
	for _, app := range []string{AppRadarr, AppSonarr} {
		base, key := os.Getenv(strings.ToUpper(app)+"_URL"), os.Getenv(strings.ToUpper(app)+"_API_KEY")
		if base == "" || key == "" {
			report.WriteString(fmt.Sprintf("## %s\n\nskipped: URL or API key env not set\n\n", app))
			continue
		}
		var cfs []servarrCF
		var profiles []servarrProfile
		servarrGet(t, base, key, "/api/v3/customformat", &cfs)
		servarrGet(t, base, key, "/api/v3/qualityprofile", &profiles)
		var items []struct {
			Title            string `json:"title"`
			Year             int    `json:"year"`
			QualityProfileID int    `json:"qualityProfileId"`
			OriginalLanguage struct {
				Name string `json:"name"`
			} `json:"originalLanguage"`
		}
		if app == AppRadarr {
			servarrGet(t, base, key, "/api/v3/movie", &items)
		} else {
			servarrGet(t, base, key, "/api/v3/series", &items)
		}
		if len(items) == 0 || len(cfs) == 0 {
			report.WriteString(fmt.Sprintf("## %s\n\nskipped: %d library items, %d custom formats\n\n", app, len(items), len(cfs)))
			continue
		}
		// Prefer an item whose profile scores something.
		pick := items[0]
		byID := map[int]servarrProfile{}
		for _, p := range profiles {
			byID[p.ID] = p
		}
		for _, it := range items {
			for _, fi := range byID[it.QualityProfileID].FormatItems {
				if fi.Score != 0 {
					pick = it
					goto picked
				}
			}
		}
	picked:
		prof := byID[pick.QualityProfileID]
		scores := map[string]int{}
		for _, fi := range prof.FormatItems {
			scores[strconv.Itoa(fi.Format)] = fi.Score
		}

		// Convert each Servarr format; unsupported ones are reported, not fatal.
		var defs []core.CustomFormat
		unsupported := map[string]string{}
		for _, cf := range cfs {
			d := core.CustomFormat{ID: strconv.Itoa(cf.ID), Name: cf.Name, App: app}
			for _, sp := range cf.Specifications {
				fm := map[string]json.RawMessage{}
				for _, f := range sp.Fields {
					fm[f.Name] = f.Value
				}
				d.Specifications = append(d.Specifications, core.FormatSpecification{Name: sp.Name, Implementation: sp.Implementation, Negate: sp.Negate, Required: sp.Required, Fields: fm})
			}
			if _, err := NewFormatScorer([]core.CustomFormat{d}, nil); err != nil {
				unsupported[cf.Name] = err.Error()
				continue
			}
			defs = append(defs, d)
		}
		scorer, err := NewFormatScorer(defs, nil)
		if err != nil {
			t.Fatalf("%s: %v", app, err)
		}

		slug := strings.NewReplacer(" ", ".", ":", "", "'", "").Replace(pick.Title)
		var titles []string
		for _, s := range radarrSuffix {
			if app == AppRadarr {
				titles = append(titles, fmt.Sprintf("%s.%d.%s", slug, pick.Year, s))
			} else {
				titles = append(titles, fmt.Sprintf("%s.S01E01.%s", slug, s))
			}
		}
		report.WriteString(fmt.Sprintf("## %s\n\nProfile `%s` (%d CFs defined, %d scored in profile, %d unsupported by Ether, minFormatScore %d).\n\n", app, prof.Name, len(cfs), len(prof.FormatItems), len(unsupported), prof.MinFormatScore))
		if len(unsupported) > 0 {
			names := make([]string, 0, len(unsupported))
			for n := range unsupported {
				names = append(names, n)
			}
			sort.Strings(names)
			report.WriteString("Unsupported by Ether: " + strings.Join(names, "; ") + "\n\n")
		}
		report.WriteString("| Title | Ether mode | Servarr mode | Servarr (live) | Servarr mode = live | Ether mode = live | Servarr-mode diff |\n|---|---|---|---|---|---|---|\n")
		etherMismatches, servarrMismatches := 0, 0
		titleRow := &core.Title{Title: pick.Title, Language: strings.ToLower(pick.OriginalLanguage.Name)}
		for _, title := range titles {
			var parsed struct {
				CustomFormats []struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				} `json:"customFormats"`
				CustomFormatScore int `json:"customFormatScore"`
			}
			servarrGet(t, base, key, "/api/v3/parse?title="+url.QueryEscape(title), &parsed)
			rel := core.Release{Name: title, Info: releaseparse.Parse(title)}
			etherTotal, _, err := scorer.scoreFor(rel, titleRow, scores, false)
			if err != nil {
				t.Fatal(err)
			}
			total, matches, err := scorer.scoreFor(rel, titleRow, scores, true)
			if err != nil {
				t.Fatal(err)
			}
			eth, srv := map[string]bool{}, map[string]bool{}
			for _, m := range matches {
				eth[m.Name] = true
			}
			for _, m := range parsed.CustomFormats {
				srv[m.Name] = true
			}
			var onlyE, onlyS []string
			for n := range eth {
				if !srv[n] {
					onlyE = append(onlyE, n)
				}
			}
			for n := range srv {
				if unsupported[n] != "" {
					onlyS = append(onlyS, n+" (unsupported)")
				} else if !eth[n] {
					onlyS = append(onlyS, n)
				}
			}
			sort.Strings(onlyE)
			sort.Strings(onlyS)
			ok := total == parsed.CustomFormatScore && len(onlyE) == 0 && len(onlyS) == 0
			rows++
			if !ok {
				mismatches++
				servarrMismatches++
			}
			if etherTotal != parsed.CustomFormatScore {
				etherMismatches++
			}
			yn := func(b bool) string {
				if b {
					return "yes"
				}
				return "NO"
			}
			report.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d | %s | %s | %s |\n", title, etherTotal, total, parsed.CustomFormatScore, yn(ok), yn(etherTotal == parsed.CustomFormatScore),
				strings.TrimSpace("only Ether: "+strings.Join(onlyE, ", ")+" / only Servarr: "+strings.Join(onlyS, ", "))))
		}
		report.WriteString(fmt.Sprintf("\n%s: Servarr mode %d/%d mismatches; Ether mode %d/%d differ from live Servarr.\n", app, servarrMismatches, len(titles), etherMismatches, len(titles)))
		report.WriteString("\n")
	}
	report.WriteString(fmt.Sprintf("%d rows, %d mismatches.\n", rows, mismatches))
	if err := os.WriteFile(out, []byte(report.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d rows, %d mismatches", out, rows, mismatches)
}
