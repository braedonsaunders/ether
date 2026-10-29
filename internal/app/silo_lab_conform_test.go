//go:build plexparity && conformlab

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	app, viewer, cleanup := newAlignedLabFixture(t, jf, labFixtureOptions{Duration: 634 * time.Second, FrameRate: 1})
	defer cleanup()
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
	inst, err := app.frontends.registry.Mount(context.Background(), core.Protocol("silo"))
	if err != nil {
		t.Fatal(err)
	}
	defer app.frontends.registry.Unmount(context.Background(), core.Protocol("silo"))
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
			t.Logf("%s %s blocking=%d", mode, profile, report.Blocking())
			for _, d := range report.Differences {
				if d.Blocking {
					t.Logf("%s %s %s", d.Endpoint, d.Path, d.Kind)
				}
			}
			if report.Blocking() > 0 {
				t.Errorf("%s %s has blocking differences", mode, profile)
			}
			for _, session := range (ownerPlayback{Playback: app.playback, app: app}).Sessions() {
				_ = app.stopOwnerSession(session.ID)
			}
		}
	}
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
		login := r.StatusCode == 200 && r.Request.Method == "POST" && r.Request.URL.Path == "/api/v2/auth/login"
		start := r.StatusCode == 201 && r.Request.Method == "POST" && r.Request.URL.Path == "/api/v2/playback/start"
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
			AccessToken string `json:"access_token"`
			SessionID   string `json:"session_id"`
		}
		if json.Unmarshal(body, &data) != nil {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if login && data.AccessToken != "" {
			issued[data.AccessToken] = true
		}
		if start && data.SessionID != "" {
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
			if res.StatusCode != 404 && res.StatusCode != 401 && (res.StatusCode < 200 || res.StatusCode >= 300) {
				t.Errorf("lab session cleanup returned %d", res.StatusCode)
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
			if res.StatusCode != 401 && (res.StatusCode < 200 || res.StatusCode >= 300) {
				t.Errorf("lab probe sign-out returned %d", res.StatusCode)
			}
		}
	}
}
