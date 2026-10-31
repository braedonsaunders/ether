package resolver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// importFixture serves the release console with a fake GitHub and a config
// store whose saves reach the resolver like the app's subscription does.
type importFixture struct {
	consoleFixture
	gh *fakeGitHub

	cfgMu sync.Mutex
	cfg   config.Config
	saves int
}

func newImportFixture(t *testing.T, save bool) *importFixture {
	t.Helper()
	return newImportFixtureTree(t, save, trashTree())
}

func newImportFixtureTree(t *testing.T, save bool, tree map[string]string) *importFixture {
	t.Helper()
	f := &importFixture{gh: newFakeGitHub(t, tree), cfg: testCfg()}
	f.svc = New(&fakeIndexer{}, nil, openTestDB(t), f.cfg, Options{})
	deps := ConsoleDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Guides: f.gh.guides(nil)}
	if save {
		deps.SaveConfig = func(mutate func(*config.Config) error) error {
			f.cfgMu.Lock()
			defer f.cfgMu.Unlock()
			next := f.cfg
			next.Quality.Profiles = append([]core.QualityProfile(nil), f.cfg.Quality.Profiles...)
			next.Resolver.CustomFormats = append([]core.CustomFormat(nil), f.cfg.Resolver.CustomFormats...)
			if err := mutate(&next); err != nil {
				return err
			}
			if _, err := NewFormatScorer(next.Resolver.CustomFormats, next.Resolver.CustomFormatScores); err != nil {
				return fmt.Errorf("%w: %v", ErrImportRejected, err)
			}
			f.cfg = next
			f.saves++
			f.svc.UpdateConfig(next)
			return nil
		}
	}
	f.srv = httptest.NewServer(f.svc.ConsoleHandler(deps))
	t.Cleanup(f.srv.Close)
	return f
}

func body(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestConsoleListsTrashProfiles(t *testing.T) {
	f := newImportFixture(t, true)
	var out struct {
		Items []TrashProfile `json:"items"`
	}
	if code := f.do(t, "GET", "/trash/radarr/profiles", "", &out); code != 200 || len(out.Items) != 2 || out.Items[0].Name != "HD Bluray + WEB" {
		t.Fatalf("list %d %+v", code, out)
	}
	var e apiError
	if code := f.do(t, "GET", "/trash/lidarr/profiles", "", &e); code != 400 || e.Error.Code != "invalid" {
		t.Fatalf("unknown app %d %+v", code, e)
	}
	f.gh.limited.Store(true)
	if code := f.do(t, "GET", "/trash/sonarr/profiles", "", &e); code != 502 || !strings.Contains(e.Error.Message, "paste the profile JSON") {
		t.Fatalf("rate limited %d %+v", code, e)
	}
}

func TestConsolePreviewsAndAppliesTrashProfile(t *testing.T) {
	f := newImportFixture(t, true)
	var plan QualityImportPlan
	if code := f.do(t, "POST", "/quality-import/preview", body(importRequest{App: AppRadarr, Profile: "sqp-1-2160p"}), &plan); code != 200 {
		t.Fatalf("preview %d", code)
	}
	if plan.Profile.Name != "[SQP] SQP-1 (2160p)" || plan.Profile.CustomFormatScores["cf-web1"] != 1800 || plan.Profile.CustomFormatScores["cf-brdisk"] != -10000 || plan.ScoreSet != "sqp-1-2160p" {
		t.Fatalf("plan %+v", plan)
	}
	if len(plan.Formats) != 2 || plan.Formats[0].Status != FormatAdd || f.saves != 0 {
		t.Fatalf("formats %+v (preview must not save)", plan.Formats)
	}
	var applied struct {
		Name         string `json:"name"`
		FormatsAdded int    `json:"formatsAdded"`
	}
	if code := f.do(t, "POST", "/quality-import", body(importRequest{App: AppRadarr, Profile: "sqp-1-2160p", Name: " UHD "}), &applied); code != 200 || applied.Name != "UHD" || applied.FormatsAdded != 2 {
		t.Fatalf("apply %d %+v", code, applied)
	}
	if last := f.cfg.Quality.Profiles[len(f.cfg.Quality.Profiles)-1]; last.Name != "UHD" || *last.MinCustomFormatScore != 1000 || len(f.cfg.Resolver.CustomFormats) != 2 {
		t.Fatalf("saved %+v / %d formats", last, len(f.cfg.Resolver.CustomFormats))
	}
	// The second import finds its formats present and the name taken.
	var e apiError
	if code := f.do(t, "POST", "/quality-import", body(importRequest{App: AppRadarr, Profile: "sqp-1-2160p", Name: "UHD"}), &e); code != 409 || e.Error.Code != "exists" {
		t.Fatalf("clash %d %+v", code, e)
	}
	if code := f.do(t, "POST", "/quality-import/preview", body(importRequest{App: AppRadarr, Profile: "hd-bluray-web"}), &plan); code != 200 {
		t.Fatalf("second preview %d", code)
	}
	statuses := map[string]string{}
	for _, fm := range plan.Formats {
		statuses[fm.Name] = fm.Status
	}
	if statuses["BR-DISK"] != FormatPresent || statuses["FreeLeech"] != FormatUnsupported || len(plan.Warnings) < 3 {
		t.Fatalf("second plan %+v", plan)
	}
}

func TestConsoleImportAddsDefaultGroupFormats(t *testing.T) {
	f := newImportFixtureTree(t, true, trashGroupTree())
	var plan QualityImportPlan
	if code := f.do(t, "POST", "/quality-import/preview", body(importRequest{App: AppRadarr, Profile: "base-profile"}), &plan); code != 200 {
		t.Fatalf("preview %d", code)
	}
	statuses := map[string]string{}
	for _, fm := range plan.Formats {
		statuses[fm.Name] = fm.Status
	}
	for _, want := range []string{"DD+ ATMOS", "TrueHD", "BR-DISK"} {
		if statuses[want] != FormatAdd {
			t.Errorf("%s not added: %+v", want, plan.Formats)
		}
	}
	for _, left := range []string{"MP3", "WEB Tier 01"} {
		if _, ok := statuses[left]; ok {
			t.Errorf("%s should stay out (opt-out member / opt-in group): %+v", left, plan.Formats)
		}
	}
	if plan.Profile.CustomFormatScores["cf-grp1"] != 2500 || plan.Profile.CustomFormatScores["cf-grp3"] != 1750 {
		t.Fatalf("scores %v", plan.Profile.CustomFormatScores)
	}
	var applied struct {
		Name         string `json:"name"`
		FormatsAdded int    `json:"formatsAdded"`
	}
	if code := f.do(t, "POST", "/quality-import", body(importRequest{App: AppRadarr, Profile: "base-profile"}), &applied); code != 200 || applied.FormatsAdded != 3 {
		t.Fatalf("apply %d %+v", code, applied)
	}
	if last := f.cfg.Quality.Profiles[len(f.cfg.Quality.Profiles)-1]; last.Name != "Base Profile" || len(f.cfg.Resolver.CustomFormats) != 3 {
		t.Fatalf("saved %+v / %d formats", last, len(f.cfg.Resolver.CustomFormats))
	}
}

func TestConsoleImportsPastedProfile(t *testing.T) {
	f := newImportFixture(t, true)
	pasted := `{"name":"Remux + WEB","items":[{"name":"Bluray-1080p Remux","allowed":true},{"name":"SDTV","allowed":true}],
		"formatItems":{"Remux Tier 01":"scf-remux1"}}`
	var plan QualityImportPlan
	// The owner left Radarr selected; the Sonarr quality names win.
	if code := f.do(t, "POST", "/quality-import/preview", body(importRequest{App: AppRadarr, JSON: pasted}), &plan); code != 200 {
		t.Fatalf("preview %d", code)
	}
	if plan.App != AppSonarr || len(plan.Formats) != 1 || plan.Formats[0].Status != FormatAdd || plan.Formats[0].Score != 1900 || plan.Skipped[0] != "SDTV" {
		t.Fatalf("plan %+v", plan)
	}
	if code := f.do(t, "POST", "/quality-import", body(importRequest{App: AppRadarr, JSON: pasted}), nil); code != 200 {
		t.Fatalf("apply %d", code)
	}
	if got := f.cfg.Resolver.CustomFormats; len(got) != 1 || got[0].App != AppSonarr {
		t.Fatalf("formats %+v", got)
	}
	// Offline: the format is unavailable, the profile still previews.
	f.gh.down.Store(true)
	offline := newImportFixture(t, true)
	offline.gh.down.Store(true)
	if code := offline.do(t, "POST", "/quality-import/preview", body(importRequest{App: AppSonarr, JSON: pasted}), &plan); code != 200 {
		t.Fatalf("offline preview %d", code)
	}
	if plan.Formats[0].Status != FormatMissing || plan.Formats[0].Reason != "Couldn't reach TRaSH Guides" || !strings.Contains(strings.Join(plan.Warnings, " "), "Couldn't look up every custom format") {
		t.Fatalf("offline plan %+v", plan)
	}
}

func TestConsoleImportRejectsBadRequests(t *testing.T) {
	f := newImportFixture(t, true)
	for name, tc := range map[string]struct {
		body string
		code int
	}{
		"no app":        {body(importRequest{Profile: "x"}), 400},
		"nothing":       {body(importRequest{App: AppRadarr}), 400},
		"both":          {body(importRequest{App: AppRadarr, Profile: "x", JSON: "{}"}), 400},
		"unknown":       {body(importRequest{App: AppRadarr, Profile: "nope"}), 404},
		"bad json":      {body(importRequest{App: AppRadarr, JSON: `{"name":"x"}`}), 400},
		"unknown field": {`{"app":"radarr","profile":"x","extra":1}`, 400},
		"too large":     {`{"app":"radarr","json":"` + strings.Repeat("a", maxImportBody) + `"}`, 413},
	} {
		var e apiError
		if code := f.do(t, "POST", "/quality-import/preview", tc.body, &e); code != tc.code || e.Error.Message == "" {
			t.Errorf("%s: %d %+v, want %d", name, code, e, tc.code)
		}
	}
	readOnly := newImportFixture(t, false)
	if code := readOnly.do(t, "POST", "/quality-import", body(importRequest{App: AppRadarr, Profile: "sqp-1-2160p"}), nil); code != 501 {
		t.Fatalf("without a saver: %d", code)
	}
}
