//go:build plexparity && conformlab

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// Run with a private LAB_ENV file and LAB_JELLYFIN_URL/LAB_SILO_URL. The
// fixture creates and closes all media, fake TMDB, frontend, and HTTP servers.
func TestSiloLabConformance(t *testing.T) {
	env := os.Getenv("LAB_ENV")
	if env == "" {
		t.Skip("LAB_ENV is not set")
	}
	settings, err := readLabSettings(env)
	if err != nil {
		t.Fatal(err)
	}
	address := func(key string) string {
		value := os.Getenv(key)
		if value == "" {
			value = settings[key]
		}
		u, e := url.Parse(value)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("invalid %s", key)
		}
		return strings.TrimRight(value, "/")
	}
	jellyfinURL, siloURL := address("LAB_JELLYFIN_URL"), address("LAB_SILO_URL")
	jf := labReference(t, settings, "JELLYFIN", jellyfinURL)
	defer jf.logout(t)
	client := &http.Client{Timeout: 30 * time.Second}
	profilesByBase := map[string]string{}
	call := func(base, method, path, token, device string, body any) map[string]any {
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, e := http.NewRequest(method, base+path, reader)
		if e != nil {
			t.Fatal("invalid request")
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Silo-Device-Id", device)
		if profile := profilesByBase[base]; profile != "" {
			req.Header.Set("X-Profile-Id", profile)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, e := client.Do(req)
		if e != nil {
			t.Fatal("lab request failed")
		}
		defer res.Body.Close()
		if res.StatusCode/100 != 2 {
			t.Fatalf("%s %s returned %d", method, path, res.StatusCode)
		}
		var value map[string]any
		if e = json.NewDecoder(res.Body).Decode(&value); e != nil {
			t.Fatalf("decode %s", path)
		}
		return value
	}
	refDevice := "lab-silo-reference"
	refLogin := call(siloURL, "POST", "/api/v2/auth/login", "", refDevice, map[string]any{"username": settings["SILO_ADULT_USER"], "password": settings["SILO_ADULT_PASSWORD"]})
	refToken, _ := refLogin["access_token"].(string)
	if refToken == "" {
		t.Fatal("lab sign-in failed")
	}
	defer func() {
		req, _ := http.NewRequest("POST", siloURL+"/api/v2/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer "+refToken)
		req.Header.Set("X-Silo-Device-Id", refDevice)
		res, e := client.Do(req)
		if e == nil {
			res.Body.Close()
			if err := siloLabCleanupStatus("lab sign-out", res.StatusCode); err != nil {
				t.Error(err)
			}
		} else {
			t.Error("lab sign-out failed")
		}
	}()
	refProfiles := call(siloURL, "GET", "/api/v2/profiles", refToken, refDevice, nil)
	profiles, _ := refProfiles["items"].([]any)
	if len(profiles) == 0 {
		t.Fatal("lab profiles unavailable")
	}
	refProfile, _ := profiles[0].(map[string]any)["id"].(string)
	profilesByBase[siloURL] = refProfile
	refLibs := call(siloURL, "GET", "/api/v2/user/libraries", refToken, refDevice, nil)
	libraries, _ := refLibs["items"].([]any)
	refLibrary := ""
	for _, v := range libraries {
		m := v.(map[string]any)
		if m["type"] == "movies" {
			refLibrary = fmt.Sprint(m["id"])
			break
		}
	}
	if refLibrary == "" {
		t.Fatal("lab movie library unavailable")
	}
	refSections := call(siloURL, "GET", "/api/v2/library/"+refLibrary+"/sections", refToken, refDevice, nil)
	rows, _ := refSections["sections"].([]any)
	refItem := ""
	for _, row := range rows {
		xs, _ := row.(map[string]any)["items"].([]any)
		for _, x := range xs {
			m := x.(map[string]any)
			if m["content_id"] == "movie-tmdb-10378" {
				refItem = "movie-tmdb-10378"
				break
			}
		}
	}
	if refItem == "" {
		t.Fatal("lab fixture title unavailable")
	}
	refVersions := call(siloURL, "GET", "/api/v2/catalog/items/"+refItem+"/versions", refToken, refDevice, nil)
	refItems, _ := refVersions["items"].([]any)
	if len(refItems) == 0 {
		t.Fatal("lab version unavailable")
	}
	refFile := fmt.Sprint(refItems[0].(map[string]any)["file_id"])
	refCaps := call(siloURL, "GET", "/api/v2/playback/capabilities", refToken, refDevice, nil)
	refInstall, _ := refCaps["installation_id"].(string)
	refDetail := call(siloURL, "GET", "/api/v2/catalog/items/"+refItem, refToken, refDevice, nil)
	refHome := call(siloURL, "GET", "/api/v2/home/sections", refToken, refDevice, nil)
	progress := siloLabHomeProgress(refHome)
	nextUp := siloLabNextUp(refHome)
	var watched []core.RatingKey
	for _, candidate := range nextUp {
		// Read the reference's actual state; next-up alone does not prove which
		// earlier episodes were watched, and no reference mutation is needed.
		for episode := 1; episode < candidate.episode; episode++ {
			id := fmt.Sprintf("episode-tvdb-78927-%d-%d", candidate.season, episode)
			detail := call(siloURL, "GET", "/api/v2/catalog/items/"+id, refToken, refDevice, nil)
			state, _ := detail["user_state"].(map[string]any)
			if state["played"] == true {
				watched = append(watched, core.EpisodeKey(91461, candidate.season, episode))
			}
		}
	}
	seedTitle := func(title *core.Title) {
		if title.Type == core.TypeEpisode && title.ShowKey == core.ShowKey(91461) {
			for _, p := range progress {
				if p.key == title.Key && p.seriesTitle != "" {
					title.ShowTitle = p.seriesTitle
				}
			}
			for _, p := range nextUp {
				if p.key == title.Key && p.seriesTitle != "" {
					title.ShowTitle = p.seriesTitle
				}
			}
		}
		if title.Key != core.MovieKey(10378) {
			return
		}
		if raw, ok := refDetail["rating_sources"].([]any); ok {
			for _, value := range raw {
				row, _ := value.(map[string]any)
				if row["source"] == "tmdb" {
					if votes, ok := row["votes"].(float64); ok && votes >= 0 {
						title.VoteCount = int(votes)
					}
				}
			}
		}
		if logo, _ := refDetail["logo_url"].(string); logo != "" {
			title.LogoPath = "/fixture.jpg"
		}
		if raw, ok := refDetail["keywords"].([]any); ok {
			title.Keywords = nil
			for _, value := range raw {
				if keyword, ok := value.(string); ok {
					title.Keywords = append(title.Keywords, keyword)
				}
			}
		}
		if raw, ok := refDetail["crew"].([]any); ok {
			title.Crew, title.Directors, title.Writers = nil, nil, nil
			for _, value := range raw {
				row, _ := value.(map[string]any)
				name, _ := row["name"].(string)
				role, _ := row["job"].(string)
				tmdb, _ := strconv.ParseInt(fmt.Sprint(row["tmdb_id"]), 10, 64)
				person := core.Person{Name: name, Role: role, TMDB: tmdb}
				if photo, _ := row["photo_url"].(string); photo != "" {
					person.ThumbPath = "/fixture.jpg"
				}
				if name != "" {
					title.Crew = append(title.Crew, person)
				}
			}
		}
	}
	app, viewer, cleanup := newAlignedLabFixture(t, jf, labFixtureOptions{Duration: 634 * time.Second, FrameRate: 1, AudioTitle: "Fixture audio", ConfigureTitle: seedTitle})
	defer cleanup()
	for _, key := range watched {
		if title, err := app.catalog.Get(context.Background(), key); err != nil || title == nil || title.Key != key {
			continue
		}
		if err := app.state.Scrobble(context.Background(), viewer.ID, key); err != nil {
			t.Fatal("failed to seed observed watched episode")
		}
	}
	for _, p := range progress {
		// The shared fixture may not contain every reference title. Only mirror
		// observed progress when the matching fake catalogue item exists.
		if title, err := app.catalog.Get(context.Background(), p.key); err != nil || title == nil || title.Key != p.key {
			continue
		}
		if err := app.state.Progress(context.Background(), viewer.ID, p.key, p.position, p.duration, "paused"); err != nil {
			t.Fatal("failed to seed reference-matched progress")
		}
	}
	inst, err := app.frontends.registry.Mount(context.Background(), core.Protocol("silo"))
	if err != nil {
		t.Fatal(err)
	}
	defer app.frontends.registry.Unmount(context.Background(), core.Protocol("silo"))
	// The owner view excludes prepared plans. The isolated broker owns those
	// reservations, so close it before checking the raw local playback manager.
	defer func() {
		app.frontends.broker.Close(context.Background())
		active, prepared := siloLabPartitionSessions(app.playback.Sessions())
		for _, session := range append(active, prepared...) {
			t.Errorf("target playback remained after broker close: id=%s prepared=%t", session.ID, session.PreparedOnly)
		}
	}()
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	targetDevice := "silo-fixture-device"
	targetLogin := call(srv.URL, "POST", "/api/v2/auth/login", "", targetDevice, map[string]any{"username": "fixture-viewer", "password": "fixture-password"})
	targetToken, _ := targetLogin["access_token"].(string)
	if targetToken == "" {
		t.Fatal("target sign-in failed")
	}
	profilesByBase[srv.URL] = strconv.FormatInt(viewer.ID, 10)
	targetItem := ids.RatingKeyGUID(core.MovieKey(10378))
	targetVersions := call(srv.URL, "GET", "/api/v2/catalog/items/"+targetItem+"/versions", targetToken, targetDevice, nil)
	targetItems, _ := targetVersions["items"].([]any)
	if len(targetItems) == 0 {
		t.Fatal("target version unavailable")
	}
	targetFile := fmt.Sprint(targetItems[0].(map[string]any)["file_id"])
	targetCaps := call(srv.URL, "GET", "/api/v2/playback/capabilities", targetToken, targetDevice, nil)
	targetInstall, _ := targetCaps["installation_id"].(string)
	values := func(username, password, token, profile, library, item, file, installation, device string) map[string]string {
		return map[string]string{"username": username, "password": password, "access_token": token, "profile_id": profile, "library_id": library, "item_id": item, "file_id": file, "installation_id": installation, "device_id": device, "duration_seconds": "634", "position_seconds": "10", "sequence": "1", "quality_preference": "auto", "app_version": "1.0.0", "app_build": "lab", "playback_attempt_id_direct": "direct", "playback_attempt_id_transcode": "transcode", "stop_id": "stop"}
	}
	refProxy, stopProxy := siloLabProbeProxy(t, siloURL, refDevice, refInstall)
	defer stopProxy()
	t.Logf("lab run devices reference=%s installation=%s target=%s installation=%s", refDevice, refInstall, targetDevice, targetInstall)
	refVars := values(settings["SILO_ADULT_USER"], settings["SILO_ADULT_PASSWORD"], refToken, refProfile, refLibrary, refItem, refFile, refInstall, refDevice)
	targetVars := values("fixture-viewer", "fixture-password", targetToken, strconv.FormatInt(viewer.ID, 10), "1", targetItem, targetFile, targetInstall, targetDevice)
	write := func(name string, v map[string]string) string {
		path := filepath.Join(t.TempDir(), name)
		raw, _ := json.Marshal(v)
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	refPath, targetPath := write("reference.json", refVars), write("target.json", targetVars)
	for _, mode := range []string{"compare", "live"} {
		var combined conform.Report
		for _, profile := range []string{"silo-apple", "silo-android"} {
			args := []string{"run", "./cmd/conformdiff", mode, "-protocol", "silo", "-client", profile, "-target", srv.URL, "-target-vars", targetPath, "-reference-vars", refPath, "-format", "json", "-timeout", "45s"}
			if mode == "live" {
				args = append(args, "-reference-server", refProxy)
			}
			cmd := exec.Command("go", args...)
			cmd.Dir = "../.."
			cmd.Env = append(os.Environ(), "CONFORM_TARGET_TOKEN="+targetToken, "CONFORM_REFERENCE_TOKEN="+refToken)
			output, runErr := cmd.Output()
			var report conform.Report
			if err = json.Unmarshal(output, &report); err != nil {
				t.Errorf("%s %s invalid report: %v", mode, profile, runErr)
				continue
			}
			combined.Differences = append(combined.Differences, report.Differences...)
			combined.Endpoints = append(combined.Endpoints, report.Endpoints...)
			combined.ReferenceDefects = append(combined.ReferenceDefects, report.ReferenceDefects...)
			if dir := os.Getenv("LAB_REPORT_DIR"); dir != "" {
				writeSiloLabReport(t, filepath.Join(dir, "profiles", profile, mode+".json"), report)
			}
			t.Logf("%s %s blocking=%d", mode, profile, report.Blocking())
			for _, d := range report.Differences {
				if d.Blocking {
					t.Logf("%s %s %s", d.Endpoint, d.Path, d.Kind)
				}
			}
			if report.Blocking() > 0 {
				t.Errorf("%s %s has blocking differences", mode, profile)
			}
			owner := ownerPlayback{Playback: app.playback, app: app}
			_, prepared := siloLabPartitionSessions(app.playback.Sessions())
			for _, session := range prepared {
				t.Logf("target prepared reservation retained for broker cleanup id=%s", session.ID)
			}
			active := owner.Sessions()
			for _, session := range active {
				t.Logf("target session cleanup id=%s", session.ID)
			}
			if stopErr := siloLabStopActive(active, app.stopOwnerSession); stopErr != nil {
				t.Errorf("target session cleanup failed: %v", stopErr)
			}
			if remaining := owner.Sessions(); len(remaining) != 0 {
				t.Errorf("target session cleanup left %d sessions", len(remaining))
			}
		}
		if dir := os.Getenv("LAB_REPORT_DIR"); dir != "" {
			writeSiloLabReport(t, filepath.Join(dir, "silo-"+mode+".json"), combined)
		}
	}
}

func writeSiloLabReport(t *testing.T, path string, report conform.Report) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func siloLabCleanupStatus(operation string, status int) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s returned %d; cleanup not verified", operation, status)
	}
	return nil
}

func siloLabPartitionSessions(sessions []*core.PlaySession) (active, prepared []*core.PlaySession) {
	for _, session := range sessions {
		if session.PreparedOnly {
			prepared = append(prepared, session)
		} else {
			active = append(active, session)
		}
	}
	return active, prepared
}

func siloLabStopActive(sessions []*core.PlaySession, stop func(string) error) error {
	var result error
	for _, session := range sessions {
		if err := stop(session.ID); err != nil {
			result = errors.Join(result, fmt.Errorf("session %s: %w", session.ID, err))
		}
	}
	return result
}

func nonNilLabItems(value any) []any {
	items, _ := value.([]any)
	return items
}

type siloLabProgressEntry struct {
	key                core.RatingKey
	position, duration time.Duration
	seriesTitle        string
}

type siloLabNextUpEntry struct {
	key             core.RatingKey
	season, episode int
	seriesTitle     string
}

func siloLabNextUp(home map[string]any) []siloLabNextUpEntry {
	var entries []siloLabNextUpEntry
	for _, rawRow := range nonNilLabItems(home["sections"]) {
		row, _ := rawRow.(map[string]any)
		for _, raw := range nonNilLabItems(row["items"]) {
			item, _ := raw.(map[string]any)
			if item["type"] != "episode" || item["item_source"] != "next_up" || item["series_id"] != "series-tvdb-78927" {
				continue
			}
			season, sok := item["season_number"].(float64)
			episode, eok := item["episode_number"].(float64)
			if !sok || !eok || season < 0 || season > 1000 || episode < 1 || episode > 50 || math.Trunc(season) != season || math.Trunc(episode) != episode {
				continue
			}
			seriesTitle, _ := item["series_title"].(string)
			entries = append(entries, siloLabNextUpEntry{key: core.EpisodeKey(91461, int(season), int(episode)), season: int(season), episode: int(episode), seriesTitle: seriesTitle})
		}
	}
	return entries
}

// Match only the reference titles whose identities the shared fixture seeds.
// The reference is read-only: progress is written exclusively to the fake app.
func siloLabHomeProgress(home map[string]any) []siloLabProgressEntry {
	var entries []siloLabProgressEntry
	rows, _ := home["sections"].([]any)
	for _, rawRow := range rows {
		row, _ := rawRow.(map[string]any)
		items, _ := row["items"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item["item_source"] != "in_progress" {
				continue
			}
			position, ok := item["position_seconds"].(float64)
			if !ok || position <= 0 || math.IsNaN(position) || math.IsInf(position, 0) || position > float64((1<<63-1)/int64(time.Second)) {
				continue
			}
			duration, ok := item["duration_seconds"].(float64)
			if !ok || duration <= position || math.IsNaN(duration) || math.IsInf(duration, 0) || duration > float64((1<<63-1)/int64(time.Second)) {
				continue
			}
			var key core.RatingKey
			switch item["type"] {
			case "movie":
				id, _ := item["content_id"].(string)
				if strings.HasPrefix(id, "movie-tmdb-") {
					if n, err := strconv.ParseInt(strings.TrimPrefix(id, "movie-tmdb-"), 10, 64); err == nil && n > 0 {
						key = core.MovieKey(n)
					}
				}
			case "episode":
				if item["series_id"] != "series-tvdb-78927" {
					continue
				}
				season, sok := item["season_number"].(float64)
				episode, eok := item["episode_number"].(float64)
				if sok && eok && season >= 0 && season <= 1000 && episode > 0 && episode <= 10000 && math.Trunc(season) == season && math.Trunc(episode) == episode {
					key = core.EpisodeKey(91461, int(season), int(episode))
				}
			}
			if key == 0 {
				continue
			}
			seriesTitle, _ := item["series_title"].(string)
			entries = append(entries, siloLabProgressEntry{key: key, position: time.Duration(position * float64(time.Second)), duration: time.Duration(duration * float64(time.Second)), seriesTitle: seriesTitle})
		}
	}
	return entries
}

func siloLabProbeProxy(t *testing.T, base, device, installation string) (string, func()) {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid lab proxy")
	}
	var mu sync.Mutex
	issued := map[string]bool{}
	sessions := map[string]string{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	orig := proxy.Director
	proxy.Director = func(r *http.Request) { orig(r); r.Host = target.Host }
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			mu.Lock()
			if r.Request.Method == "DELETE" && strings.HasPrefix(r.Request.URL.Path, "/api/v2/playback/") {
				delete(sessions, strings.TrimPrefix(r.Request.URL.Path, "/api/v2/playback/"))
			}
			if r.Request.Method == "POST" && r.Request.URL.Path == "/api/v2/auth/logout" {
				delete(issued, strings.TrimPrefix(r.Request.Header.Get("Authorization"), "Bearer "))
			}
			mu.Unlock()
		}
		login := r.StatusCode == 200 && r.Request.Method == "POST" && r.Request.URL.Path == "/api/v2/auth/login"
		start := r.StatusCode >= 200 && r.StatusCode < 300 && r.Request.Method == "POST" && r.Request.URL.Path == "/api/v2/playback/start"
		if !login && !start {
			return nil
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		r.Body.Close()
		if e != nil {
			return e
		}
		if len(body) > 1<<20 {
			return fmt.Errorf("probe response too large")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var data struct {
			AccessToken  string `json:"access_token"`
			SessionID    string `json:"session_id"`
			PlaybackPlan struct {
				SessionID string `json:"session_id"`
			} `json:"playback_plan"`
		}
		if json.Unmarshal(body, &data) != nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if login && data.AccessToken != "" {
			issued[data.AccessToken] = true
		}
		if start && data.SessionID == "" {
			data.SessionID = data.PlaybackPlan.SessionID
		}
		if start {
			t.Logf("reference playback response status=%d session_observed=%t", r.StatusCode, data.SessionID != "")
		}
		if start && data.SessionID != "" {
			t.Logf("reference session observed id=%s", data.SessionID)
			sessions[data.SessionID] = r.Request.Header.Get("Authorization")
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	return server.URL, func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		client := &http.Client{Timeout: 15 * time.Second}
		for id, auth := range sessions {
			var n [16]byte
			if _, e := rand.Read(n[:]); e != nil {
				t.Error("unable to create cleanup id")
				continue
			}
			raw, _ := json.Marshal(map[string]string{"installation_id": installation, "stop_id": hex.EncodeToString(n[:])})
			req, _ := http.NewRequest("DELETE", base+"/api/v2/playback/"+url.PathEscape(id), bytes.NewReader(raw))
			req.Header.Set("Authorization", auth)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Silo-Device-Id", device)
			res, e := client.Do(req)
			if e != nil {
				t.Error("lab session cleanup failed")
				continue
			}
			res.Body.Close()
			if err := siloLabCleanupStatus("lab session cleanup", res.StatusCode); err != nil {
				t.Error(err)
			}
		}
		for token := range issued {
			req, _ := http.NewRequest("POST", base+"/api/v2/auth/logout", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Silo-Device-Id", device)
			res, e := client.Do(req)
			if e != nil {
				t.Error("lab probe sign-out failed")
				continue
			}
			res.Body.Close()
			if err := siloLabCleanupStatus("lab probe sign-out", res.StatusCode); err != nil {
				t.Error(err)
			}
		}
	}
}
