//go:build plexparity && conformlab

package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/conformance"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sessions"
	tmdbpkg "github.com/braedonsaunders/ether/internal/tmdb"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Run: LAB_ENV=/path/to/lab.env LAB_JELLYFIN_URL=http://host:8096 LAB_EMBY_URL=http://host:8097/emby go test -tags plexparity,conformlab ./internal/app -run TestMediaBrowserLabConformance -count=1 -v
// LAB_ENV=env reads the four adult credential variables from the process environment;
// this permits a private --env-file /dev/stdin in a disposable worker container.
// LAB_REPORT_DIR optionally retains redacted JSON reports; otherwise only counts
// are printed. LAB_SUMMARY_PATH optionally writes release evidence (native
// clients are never certified by this harness). Credentials and generated
// media stay in a private temporary dir.
func TestMediaBrowserLabConformance(t *testing.T) {
	path := os.Getenv("LAB_ENV")
	if path == "" {
		t.Skip("LAB_ENV is not set")
	}
	settings, err := readLabSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"LAB_JELLYFIN_URL", "LAB_EMBY_URL"} {
		if v := os.Getenv(key); v != "" {
			settings[key] = v
		}
		parsed, e := url.Parse(settings[key])
		if e != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
			t.Fatalf("%s must be an HTTP(S) lab URL without credentials", key)
		}
	}
	jellyfinRef := labReference(t, settings, "JELLYFIN", settings["LAB_JELLYFIN_URL"])
	defer jellyfinRef.logout(t)
	embyRef := labReference(t, settings, "EMBY", settings["LAB_EMBY_URL"])
	defer embyRef.logout(t)
	ctx := context.Background()
	a, viewer, cleanup := newAlignedLabFixture(t, jellyfinRef, labFixtureOptions{PublicViewerPicker: map[string]bool{"emby": true}})
	defer cleanup()
	summary := conformance.Manifest{Schema: 1, Protocols: map[string]conformance.Summary{}}
	for _, spec := range []struct {
		proto, client string
		reference     labReferenceData
	}{{"jellyfin", "findroid,jellycon,jellyfin-androidtv,streamyfin,swiftfin,infuse-jellyfin", jellyfinRef}, {"emby", "emby-sdk", embyRef}} {
		t.Run(spec.proto, func(t *testing.T) {
			inst, e := a.frontends.registry.Mount(ctx, core.Protocol(spec.proto))
			if e != nil {
				t.Fatal(e)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(spec.proto))
			combined := map[string]bool{}
			validEvidence := true
			var seq atomic.Uint32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := seq.Add(1)
				r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", n%200+1)
				inst.Handler().ServeHTTP(w, r)
			}))
			defer srv.Close()
			r, _ := http.NewRequest("POST", srv.URL+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"fixture-viewer","Pw":"fixture-password"}`))
			r.Header.Set("Authorization", `MediaBrowser Client="Conform", DeviceId="conform-device"`)
			response, e := srv.Client().Do(r)
			if e != nil {
				t.Fatal(e)
			}
			var login struct{ AccessToken string }
			e = json.NewDecoder(response.Body).Decode(&login)
			response.Body.Close()
			if e != nil || login.AccessToken == "" {
				t.Fatal("target login failed", e)
			}
			// Each simulated client uses the grant issued for its own DeviceId.
			// Reusing the preflight token for a different client would violate
			// the production grant/device binding.
			clientTokens := map[string]string{}
			defer func() {
				for clientName, token := range clientTokens {
					(labReferenceData{base: srv.URL, token: token, device: "conform-" + clientName}).logout(t)
				}
				(labReferenceData{base: srv.URL, token: login.AccessToken, device: "conform-device"}).logout(t)
			}()
			for _, clientName := range strings.Split(spec.client, ",") {
				deviceID := "conform-" + clientName
				clientReq, e := http.NewRequest("POST", srv.URL+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"fixture-viewer","Pw":"fixture-password"}`))
				if e != nil {
					t.Fatal("target client login request failed")
				}
				clientReq.Header.Set("Authorization", `MediaBrowser Client="Conform", DeviceId="`+deviceID+`"`)
				clientResp, e := srv.Client().Do(clientReq)
				if e != nil {
					t.Fatal("target client login failed")
				}
				var clientLogin struct{ AccessToken string }
				e = json.NewDecoder(clientResp.Body).Decode(&clientLogin)
				clientResp.Body.Close()
				if e != nil || clientResp.StatusCode != http.StatusOK || clientLogin.AccessToken == "" {
					t.Fatal("target client login response invalid")
				}
				clientTokens[clientName] = clientLogin.AccessToken
			}
			userID, _ := ids.GUID(ids.User, viewer.ID)
			viewID, _ := ids.GUID(ids.View, 1)
			itemID := ids.RatingKeyGUID(core.MovieKey(10378))
			itemReq, _ := http.NewRequest("GET", srv.URL+"/Items/"+itemID, nil)
			itemReq.Header.Set("X-Emby-Token", login.AccessToken)
			itemResp, err := srv.Client().Do(itemReq)
			if err != nil {
				t.Fatal(err)
			}
			var item struct{ ImageTags map[string]string }
			err = json.NewDecoder(itemResp.Body).Decode(&item)
			itemResp.Body.Close()
			if err != nil || itemResp.StatusCode != 200 || item.ImageTags["Primary"] == "" {
				t.Fatal("item artwork not issued", itemResp.StatusCode, err)
			}
			labTraceMediaDetail(t, a, viewer, spec.proto, "initial", spec.reference.vars["item_id"], srv, login.AccessToken, itemID, "")
			firstClient := strings.Split(spec.client, ",")[0]
			labTraceMediaDetail(t, a, viewer, spec.proto, "initial", spec.reference.vars["item_id"], srv, clientTokens[firstClient], itemID, "conform-"+firstClient)
			imgResp, err := srv.Client().Get(srv.URL + "/Items/" + itemID + "/Images/Primary?tag=" + url.QueryEscape(item.ImageTags["Primary"]))
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, imgResp.Body)
			imgResp.Body.Close()
			if err != nil || imgResp.StatusCode != 200 || !strings.HasPrefix(imgResp.Header.Get("Content-Type"), "image/") {
				t.Fatal("fixture artwork fetch failed", imgResp.StatusCode, err)
			}
			gridReq, _ := http.NewRequest("GET", srv.URL+"/Items?Recursive=true&Limit=50", nil)
			gridReq.Header.Set("X-Emby-Token", login.AccessToken)
			gridResp, err := srv.Client().Do(gridReq)
			if err != nil {
				t.Fatal(err)
			}
			var grid struct {
				Items []struct {
					Id        string
					ImageTags map[string]string
				}
			}
			err = json.NewDecoder(gridResp.Body).Decode(&grid)
			gridResp.Body.Close()
			if err != nil || gridResp.StatusCode != 200 {
				t.Fatal("grid failed", gridResp.StatusCode, err)
			}
			foundImage := false
			for _, entry := range grid.Items {
				if entry.Id == itemID && entry.ImageTags["Primary"] != "" {
					foundImage = true
				}
			}
			if !foundImage {
				t.Fatal("grid did not issue scoped artwork for seeded title")
			}
			similarReq, _ := http.NewRequest("GET", srv.URL+"/Items/"+itemID+"/Similar", nil)
			similarReq.Header.Set("X-Emby-Token", login.AccessToken)
			similarResp, err := srv.Client().Do(similarReq)
			if err != nil {
				t.Fatal(err)
			}
			var similar struct{ Items []struct{ Name string } }
			err = json.NewDecoder(similarResp.Body).Decode(&similar)
			similarResp.Body.Close()
			if err != nil || similarResp.StatusCode != http.StatusOK {
				t.Fatal(similarResp.StatusCode, err)
			}
			for _, item := range similar.Items {
				if item.Name == "Future hidden title" {
					t.Fatal("unreleased candidate was exposed")
				}
			}
			values := map[string]string{"username": "fixture-viewer", "password": "fixture-password", "device_id": "conform-device", "access_token": login.AccessToken, "user_id": userID, "item_id": itemID, "itemId": itemID, "media_source_id": itemID, "parent_id": viewID, "play_session_id": "initial", "position_ticks": "0"}
			targetFile := filepath.Join(t.TempDir(), "target.json")
			b, _ := json.Marshal(values)
			if err := os.WriteFile(targetFile, b, 0600); err != nil {
				t.Fatal(err)
			}
			refFile := filepath.Join(t.TempDir(), "reference.json")
			refJSON, err := json.Marshal(spec.reference.vars)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(refFile, refJSON, 0600); err != nil {
				t.Fatal(err)
			}
			labProxy, stopProxy := labProbeProxy(t, spec.reference)
			defer stopProxy()
			for _, mode := range []string{"compare", "live"} {
				report := conform.Report{}
				for _, clientName := range strings.Split(spec.client, ",") {
					values["device_id"] = "conform-" + clientName
					values["access_token"] = clientTokens[clientName]
					b, _ = json.Marshal(values)
					if err := os.WriteFile(targetFile, b, 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"run", "./cmd/conformdiff", mode, "-protocol", spec.proto, "-client", clientName, "-target", srv.URL, "-target-vars", targetFile, "-reference-vars", refFile, "-format", "json", "-timeout", "15s"}
					if mode == "live" {
						args = append(args, "-reference-server", labProxy)
					}
					cmd := exec.Command("go", args...)
					cmd.Dir = "../.."
					cmd.Env = append(os.Environ(), "CONFORM_TARGET_TOKEN="+clientTokens[clientName], "CONFORM_REFERENCE_TOKEN="+spec.reference.token)
					out, err := cmd.Output()
					var partial conform.Report
					if e = json.Unmarshal(out, &partial); e != nil {
						validEvidence = false
						t.Errorf("%s %s %s invalid report: %v", spec.proto, mode, clientName, err)
						continue
					}
					for _, diff := range partial.Differences {
						if diff.Kind == conform.KindRequestError {
							validEvidence = false
						}
						if mode == "live" && diff.Kind == conform.KindStatus && strings.HasSuffix(diff.Endpoint, "/Users/AuthenticateByName") && diff.Reference != float64(http.StatusOK) {
							validEvidence = false
							t.Errorf("%s %s reference login did not succeed; comparison invalid", spec.proto, clientName)
						}
						if mode == "live" && diff.Kind == conform.KindStatus && diff.Reference == float64(http.StatusUnauthorized) {
							validEvidence = false
							t.Errorf("%s %s reference authorization expired; comparison is invalid", spec.proto, clientName)
							break
						}
					}
					report.Differences = append(report.Differences, partial.Differences...)
					report.ReferenceDefects = append(report.ReferenceDefects, partial.ReferenceDefects...)
					for _, session := range (ownerPlayback{Playback: a.playback, app: a}).Sessions() {
						t.Logf("lab_target_stop protocol=%s mode=%s client=%s session=%s", spec.proto, mode, clientName, session.ID)
						if err := a.stopOwnerSession(session.ID); err != nil {
							validEvidence = false
							t.Errorf("%s %s target playback cleanup failed", spec.proto, clientName)
						}
					}
					if len((ownerPlayback{Playback: a.playback, app: a}).Sessions()) != 0 {
						validEvidence = false
						t.Errorf("%s %s target playback cleanup left active sessions", spec.proto, clientName)
					}
					t.Logf("lab_target_remaining protocol=%s mode=%s client=%s count=%d", spec.proto, mode, clientName, len((ownerPlayback{Playback: a.playback, app: a}).Sessions()))
				}
				labTraceMediaDetail(t, a, viewer, spec.proto, "after-"+mode, spec.reference.vars["item_id"], srv, login.AccessToken, itemID, "")
				labTraceMediaDetail(t, a, viewer, spec.proto, "after-"+mode, spec.reference.vars["item_id"], srv, clientTokens[firstClient], itemID, "conform-"+firstClient)
				if reportDir := os.Getenv("LAB_REPORT_DIR"); reportDir != "" {
					if err := os.MkdirAll(reportDir, 0700); err != nil {
						t.Fatal(err)
					}
					output, err := json.MarshalIndent(report, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(reportDir, spec.proto+"-"+mode+".json"), output, 0600); err != nil {
						t.Fatal(err)
					}
				}
				unique := map[string]bool{}
				for _, d := range report.Differences {
					if d.Blocking {
						path := d.Path
						if d.Kind == "request_error" {
							path = "$request"
						}
						unique[d.Endpoint+"|"+path+"|"+d.Kind] = true
						combined[d.Endpoint+"|"+path+"|"+d.Kind] = true
					}
				}
				keys := make([]string, 0, len(unique))
				for k := range unique {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				t.Logf("%s %s blocking=%d unique=%d", spec.proto, mode, report.Blocking(), len(keys))
				for _, k := range keys {
					t.Log(k)
				}
				if len(keys) > 0 {
					t.Errorf("%s %s has %d unique blocking differences", spec.proto, mode, len(keys))
				}
			}
			if validEvidence {
				count := len(combined)
				summary.Protocols[spec.proto] = conformance.Summary{ProtocolVersion: inst.Status(ctx).TargetVersion, Date: time.Now().UTC().Format(time.RFC3339), Blocking: &count, CertifiedClients: []string{}}
			}

		})
	}
	if path := os.Getenv("LAB_SUMMARY_PATH"); path != "" {
		output, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(output, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

type labFixtureOptions struct {
	Duration  time.Duration
	FrameRate int
	// AudioTitle seeds a real title in the generated stream for protocols
	// that exercise track metadata. Empty preserves the shared default.
	AudioTitle string
	// ConfigureTitle may add reference-derived metadata to the isolated seed.
	ConfigureTitle func(*core.Title)
	// PublicViewerPicker enables only explicitly named protocol pickers.
	// Omitted entries retain the default-off security behavior.
	PublicViewerPicker map[string]bool
}

func configureLabPickers(c *config.Config, pickers map[string]bool) error {
	if len(pickers) == 0 {
		return nil
	}
	if c.Frontends == nil {
		c.Frontends = map[string]config.Frontend{}
	}
	for protocol, enabled := range pickers {
		if protocol != "jellyfin" && protocol != "emby" {
			return fmt.Errorf("invalid picker protocol %q", protocol)
		}
		surface := c.Frontends[protocol]
		if surface.Settings == nil {
			surface.Settings = map[string]any{}
		}
		surface.Settings["publicViewerPicker"] = enabled
		c.Frontends[protocol] = surface
	}
	return nil
}

func TestLabPickerSettingsArePerProtocol(t *testing.T) {
	cfg := &config.Config{}
	if err := configureLabPickers(cfg, nil); err != nil || len(cfg.Frontends) != 0 {
		t.Fatal("default picker must remain off")
	}
	if err := configureLabPickers(cfg, map[string]bool{"emby": true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Frontends["emby"].Settings["publicViewerPicker"] != true || cfg.Frontends["jellyfin"].Settings["publicViewerPicker"] != nil {
		t.Fatal("picker leaked across protocols")
	}
	if err := configureLabPickers(cfg, map[string]bool{"silo": true}); err == nil {
		t.Fatal("accepted unsupported picker")
	}
}

// newAlignedLabFixture supplies an isolated real app with fake playback and artwork for opt-in lab tests.
// Options permit other protocols to match a reference duration without changing
// the MediaBrowser fixture; values are bounded to keep generated media small.
func newAlignedLabFixture(t *testing.T, jellyfinRef labReferenceData, options ...labFixtureOptions) (*App, *core.User, func()) {
	t.Helper()
	duration, frameRate := 15*time.Second, 24
	if len(options) > 1 {
		t.Fatal("only one lab fixture options value is allowed")
	}
	if len(options) == 1 {
		if options[0].Duration != 0 {
			duration = options[0].Duration
		}
		if options[0].FrameRate != 0 {
			frameRate = options[0].FrameRate
		}
	}
	if duration <= 0 || duration > 30*time.Minute || frameRate < 1 || frameRate > 60 {
		t.Fatal("invalid lab fixture duration or frame rate")
	}
	ctx := context.Background()
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x + y), 255})
		}
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/3/images/") && strings.HasSuffix(r.URL.Path, "/fixture.jpg") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBytes.Bytes())
			return
		}
		if (strings.HasPrefix(r.URL.Path, "/3/movie/") || strings.HasPrefix(r.URL.Path, "/3/tv/")) && (strings.HasSuffix(r.URL.Path, "/recommendations") || strings.HasSuffix(r.URL.Path, "/similar")) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"page":1,"total_pages":1,"total_results":1,"results":[{"id":987654321,"title":"Future hidden title","name":"Future hidden title","release_date":"2099-01-01","first_air_date":"2099-01-01"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Setenv("ETHER_PARITY_TMDB_URL", tmdb.URL+"/3")
	dir := t.TempDir()
	var a *App
	cleanup := sync.OnceFunc(func() {
		if a != nil {
			a.Close()
		}
		tmdb.Close()
	})
	t.Cleanup(cleanup)
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Fatal("ffmpeg is required for the opt-in lab harness")
	}
	mediaArgs := []string{"-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x180:rate=%d", frameRate), "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", strconv.FormatFloat(duration.Seconds(), 'f', 3, 64), "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac"}
	if len(options) == 1 && options[0].AudioTitle != "" {
		if len(options[0].AudioTitle) > 256 || strings.ContainsRune(options[0].AudioTitle, '\x00') {
			t.Fatal("invalid fixture audio title")
		}
		mediaArgs = append(mediaArgs, "-metadata:s:a:0", "title="+options[0].AudioTitle)
	}
	mediaArgs = append(mediaArgs, "-movflags", "+faststart", filepath.Join(dir, "parity.mp4"))
	makeMedia := exec.Command(ffmpeg, mediaArgs...)
	if output, e := makeMedia.CombinedOutput(); e != nil {
		t.Fatalf("generate fixture media: %v: %s", e, output)
	}
	mediaBytes, hashErr := os.ReadFile(filepath.Join(dir, "parity.mp4"))
	if hashErr != nil {
		t.Fatal("fixture hash unavailable")
	}
	t.Logf("lab_fixture sha256=%x bytes=%d", sha256.Sum256(mediaBytes), len(mediaBytes))
	cfg, e := config.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Catalog.TMDBKey = "fake-tmdb-key"
		c.Plex.Token = "fixture-owner-token"
		c.Transcode.FFmpegPath = ffmpeg
		c.Server.MaxStreams = 30
		if len(options) == 1 {
			if err := configureLabPickers(c, options[0].PublicViewerPicker); err != nil {
				return err
			}
		}
		for i := range c.Quality.Profiles {
			c.Quality.Profiles[i].MinSizeMBPerMin = 0
			c.Quality.Profiles[i].MaxSizeMBPerMin = 0
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	a, e = New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		t.Fatal(e)
	}
	imageConfig := &tmdbpkg.Configuration{}
	imageConfig.Images.BaseURL = tmdb.URL + "/3/images/"
	imageConfig.Images.SecureBaseURL = imageConfig.Images.BaseURL
	a.tmdb.SetConfiguration(imageConfig)
	if e = a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "fixture-owner-token"); e != nil {
		t.Fatal(e)
	}
	viewer, e := a.users.Owner(ctx)
	if e != nil {
		t.Fatal(e)
	}
	local, _ := identity.NewLocal(a.db)
	if e = local.Create(ctx, viewer.ID, "fixture-viewer", "fixture-password"); e != nil {
		t.Fatal(e)
	}
	request, _ := http.NewRequest("GET", jellyfinRef.base+"/Items?Recursive=true&Fields=ProviderIds,Overview,OriginalTitle,Genres,Studios,ProductionLocations,Taglines,People", nil)
	request.Header.Set("X-Emby-Token", jellyfinRef.token)
	request.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+jellyfinRef.device+`", Version="1", Token="`+jellyfinRef.token+`"`)
	resp, e := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if e != nil {
		t.Fatal("lab listing unavailable")
	}
	var listing struct {
		Items []struct {
			Name, Type, Overview, OriginalTitle, OriginalLanguage, OfficialRating string
			ProviderIds                                                           map[string]string
			IndexNumber, ParentIndexNumber, ProductionYear                        int
			RunTimeTicks                                                          int64
			CommunityRating                                                       float64
			Genres, ProductionLocations, Taglines                                 []string
			Studios                                                               []struct{ Name string }
			People                                                                []struct{ Name, Role, Type string }
		}
	}
	e = json.NewDecoder(resp.Body).Decode(&listing)
	resp.Body.Close()
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range listing.Items {
		tmdbID, _ := strconv.ParseInt(x.ProviderIds["Tmdb"], 10, 64)
		tvdb, _ := strconv.ParseInt(x.ProviderIds["Tvdb"], 10, 64)
		title := core.Title{Title: x.Name, Summary: x.Overview, IDs: core.IDs{TMDB: tmdbID, IMDB: x.ProviderIds["Imdb"], TVDB: tvdb}, Year: x.ProductionYear, Runtime: time.Duration(x.RunTimeTicks) * 100 * time.Nanosecond, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), PosterPath: "/fixture.jpg", BackdropPath: "/fixture.jpg", StillPath: "/fixture.jpg"}
		title.OriginalTitle, title.Language, title.ContentRating, title.Rating = x.OriginalTitle, x.OriginalLanguage, x.OfficialRating, x.CommunityRating
		title.Genres, title.Countries = x.Genres, x.ProductionLocations
		if len(x.Taglines) > 0 {
			title.Tagline = x.Taglines[0]
		}
		for _, studio := range x.Studios {
			title.Studios = append(title.Studios, studio.Name)
		}
		for _, person := range x.People {
			p := core.Person{Name: person.Name, Role: person.Role}
			switch person.Type {
			case "Actor":
				title.Cast = append(title.Cast, p)
			case "Director":
				title.Directors = append(title.Directors, p)
			case "Writer":
				title.Writers = append(title.Writers, p)
			}
		}
		switch x.Type {
		case "Movie":
			if tmdbID == 0 {
				continue
			}
			title.Key = core.MovieKey(tmdbID)
			title.Type = core.TypeMovie
		case "Series":
			if tmdbID != 91461 {
				continue
			}
			title.Key = core.ShowKey(tmdbID)
			title.Type = core.TypeShow
		case "Season":
			title.Key = core.SeasonKey(91461, x.IndexNumber)
			title.Type = core.TypeSeason
			title.ShowKey = core.ShowKey(91461)
			title.Season = x.IndexNumber
		case "Episode":
			title.Key = core.EpisodeKey(91461, x.ParentIndexNumber, x.IndexNumber)
			title.Type = core.TypeEpisode
			title.ShowKey = core.ShowKey(91461)
			title.ShowIDs = core.IDs{TMDB: 91461}
			title.Season = x.ParentIndexNumber
			title.Episode = x.IndexNumber
		default:
			continue
		}
		if len(options) == 1 && options[0].ConfigureTitle != nil {
			options[0].ConfigureTitle(&title)
		}
		if e = a.catalog.UpsertTitle(ctx, title, true); e != nil {
			t.Fatal(e)
		}
	}
	if _, err := a.catalog.Similar(ctx, core.MovieKey(10378), 5); err != nil {
		t.Fatal(err)
	}
	return a, viewer, cleanup
}

type labReferenceData struct {
	base, token, device string
	session             string
	vars                map[string]string
}

func (ref labReferenceData) logout(t *testing.T) {
	t.Helper()
	req, err := http.NewRequest("POST", ref.base+"/Sessions/Logout", nil)
	if err != nil {
		t.Error("could not construct lab sign-out")
		return
	}
	req.Header.Set("X-Emby-Token", ref.token)
	req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+ref.device+`", Version="1", Token="`+ref.token+`"`)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Error("lab sign-out failed")
		return
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Errorf("lab sign-out returned HTTP %d", response.StatusCode)
	}
	t.Logf("lab_logout device=%s session=%s status=%d", ref.device, ref.session, response.StatusCode)
}

// labProbeProxy retains newly issued probe tokens only in memory, so every
// live sign-in can be revoked after the test without writing credentials to a
// transcript or report. Other requests pass through unchanged.
// labRequestIdentity accepts the auth forms emitted by conformdiff. Conflicting
// token/device assertions are ambiguous; never guess a credential for cleanup.
func labRequestIdentity(req *http.Request) (token, device string, err error) {
	merge := func(dst *string, value string) bool {
		if value == "" {
			return true
		}
		if *dst != "" && *dst != value {
			return false
		}
		*dst = value
		return true
	}
	for _, value := range req.Header.Values("X-Emby-Token") {
		if !merge(&token, value) {
			return "", "", fmt.Errorf("ambiguous token")
		}
	}
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		for _, value := range req.Header.Values(header) {
			scheme, params, ok := strings.Cut(value, " ")
			if !ok || !strings.EqualFold(scheme, "MediaBrowser") {
				return "", "", fmt.Errorf("unsupported auth form")
			}
			_, parsed, parseErr := mime.ParseMediaType("application/x;" + strings.ReplaceAll(params, ",", ";"))
			if parseErr != nil || !merge(&token, parsed["token"]) || !merge(&device, parsed["deviceid"]) {
				return "", "", fmt.Errorf("ambiguous auth parameters")
			}
		}
	}
	return token, device, nil
}

type labPlaybackReceipt struct {
	Token, Device, Session, Item string
	Body                         []byte
}
type labPlaybackContextKey struct{}
type labLoginContextKey struct{}

func labProbeProxy(t *testing.T, ref labReferenceData) (string, func()) {
	t.Helper()
	target, err := url.Parse(ref.base)
	if err != nil {
		t.Fatal("invalid lab proxy target")
	}
	var mu sync.Mutex
	issued := map[string]labReferenceData{}
	// Quarantine is cleanup-only; malformed metadata never makes a token usable.
	quarantine := map[string]labReferenceData{}
	pending := map[[3]string]labPlaybackReceipt{}
	proxy := httputil.NewSingleHostReverseProxy(target)
	direct := proxy.Director
	proxy.Director = func(req *http.Request) { direct(req); req.Host = target.Host }
	proxy.ModifyResponse = func(response *http.Response) error {
		if receipt, ok := response.Request.Context().Value(labPlaybackContextKey{}).(labPlaybackReceipt); ok {
			key := [3]string{receipt.Token, receipt.Session, receipt.Item}
			mu.Lock()
			if strings.HasSuffix(response.Request.URL.Path, "/Sessions/Playing/Stopped") {
				t.Logf("lab_reference_profile_stop device=%s session=%s item=%s status=%d", receipt.Device, receipt.Session, receipt.Item, response.StatusCode)
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					delete(pending, key)
				}
			}
			mu.Unlock()
		}
		if response.StatusCode != http.StatusOK || response.Request.Method != http.MethodPost || !strings.HasSuffix(response.Request.URL.Path, "/Users/AuthenticateByName") {
			return nil
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if readErr != nil || len(body) > 1<<20 {
			return fmt.Errorf("lab login response unreadable or oversized")
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		decoded := body
		switch strings.ToLower(response.Header.Get("Content-Encoding")) {
		case "", "identity":
		case "gzip":
			reader, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("lab login gzip invalid")
			}
			decoded, err = io.ReadAll(io.LimitReader(reader, (1<<20)+1))
			reader.Close()
			if err != nil || len(decoded) > 1<<20 {
				return fmt.Errorf("lab login gzip unreadable or oversized")
			}
		default:
			return fmt.Errorf("lab login encoding unsupported")
		}
		// Provenance is established before forwarding this own-device login.
		device, owned := response.Request.Context().Value(labLoginContextKey{}).(string)
		if !owned || device == "" {
			return fmt.Errorf("lab login request provenance missing")
		}
		var minted struct{ AccessToken string }
		if json.Unmarshal(decoded, &minted) != nil || minted.AccessToken == "" {
			return fmt.Errorf("lab login token unavailable")
		}
		owner := labReferenceData{base: ref.base, token: minted.AccessToken, device: device}
		mu.Lock()
		quarantine[minted.AccessToken] = owner
		mu.Unlock()
		t.Logf("lab_quarantined_probe device=%s", device)
		var metadata struct{ SessionInfo struct{ Id, DeviceId string } }
		if json.Unmarshal(decoded, &metadata) != nil {
			return fmt.Errorf("lab login metadata invalid; credential quarantined")
		}
		if metadata.SessionInfo.DeviceId != "" && metadata.SessionInfo.DeviceId != device {
			return fmt.Errorf("lab login device mismatch; credential quarantined")
		}
		owner.session = metadata.SessionInfo.Id
		mu.Lock()
		quarantine[minted.AccessToken] = owner
		issued[minted.AccessToken] = owner
		mu.Unlock()
		t.Logf("lab_owned_probe device=%s session=%s", device, owner.session)
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		// Error messages originate above or in transport: never print the raw error.
		t.Logf("lab_proxy_failure method=%s auth_endpoint=%t", req.Method, strings.HasSuffix(req.URL.Path, "/Users/AuthenticateByName"))
		http.Error(w, "lab proxy response failed", http.StatusBadGateway)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/Users/AuthenticateByName") {
			_, device, err := labRequestIdentity(req)
			if err != nil || device == "" || (device != ref.device && device != ref.vars["device_id"]) {
				http.Error(w, "unowned or ambiguous login device", http.StatusForbidden)
				return
			}
			req = req.WithContext(context.WithValue(req.Context(), labLoginContextKey{}, device))
		}
		playback := req.Method == http.MethodPost && (strings.HasSuffix(req.URL.Path, "/Sessions/Playing") || strings.HasSuffix(req.URL.Path, "/Sessions/Playing/Progress") || strings.HasSuffix(req.URL.Path, "/Sessions/Playing/Stopped"))
		if !playback {
			proxy.ServeHTTP(w, req)
			return
		}
		token, device, identityErr := labRequestIdentity(req)
		mu.Lock()
		owner, owned := issued[token]
		mu.Unlock()
		if token != "" && token == ref.token {
			owner = ref
			owned = true
		}
		if identityErr != nil || token == "" || !owned {
			http.Error(w, "unowned or ambiguous playback identity", http.StatusForbidden)
			return
		}
		if device == "" {
			device = owner.device
		}
		if device == "" || (device != owner.device && device != ref.vars["device_id"]) {
			http.Error(w, "unowned playback device", http.StatusForbidden)
			return
		}
		if req.Body == nil {
			http.Error(w, "missing playback body", http.StatusBadRequest)
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(req.Body, (1<<20)+1))
		req.Body.Close()
		if readErr != nil || len(body) > 1<<20 {
			http.Error(w, "unreadable or oversized playback body", http.StatusBadRequest)
			return
		}
		var ids struct{ ItemId, PlaySessionId string }
		if json.Unmarshal(body, &ids) != nil || ids.ItemId == "" || ids.PlaySessionId == "" {
			http.Error(w, "missing playback identity", http.StatusBadRequest)
			return
		}
		receipt := labPlaybackReceipt{Token: token, Device: device, Session: ids.PlaySessionId, Item: ids.ItemId, Body: body}
		key := [3]string{token, ids.PlaySessionId, ids.ItemId}
		mu.Lock()
		if !strings.HasSuffix(req.URL.Path, "/Sessions/Playing/Stopped") {
			pending[key] = receipt
		}
		mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(body))
		proxy.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), labPlaybackContextKey{}, receipt)))
	})
	server := httptest.NewServer(handler)
	return server.URL, func() {
		server.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, receipt := range pending {
			req, err := http.NewRequest("POST", ref.base+"/Sessions/Playing/Stopped", bytes.NewReader(receipt.Body))
			if err != nil {
				t.Error("reference stop construction failed")
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Emby-Token", receipt.Token)
			req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+receipt.Device+`", Version="1"`)
			response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
			if err != nil {
				t.Error("reference stop failed")
				continue
			}
			response.Body.Close()
			t.Logf("lab_reference_stop device=%s session=%s item=%s status=%d", receipt.Device, receipt.Session, receipt.Item, response.StatusCode)
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				t.Error("reference stop not accepted")
			}
		}
		for _, owner := range quarantine {
			owner.logout(t)
		}
	}
}

func readLabSettings(path string) (map[string]string, error) {
	if path == "env" {
		values := map[string]string{}
		for _, key := range []string{"JELLYFIN_ADULT_USER", "JELLYFIN_ADULT_PASSWORD", "EMBY_ADULT_USER", "EMBY_ADULT_PASSWORD"} {
			if value, ok := os.LookupEnv(key); ok {
				values[key] = value
			}
		}
		return values, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open LAB_ENV: %w", err)
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid LAB_ENV entry")
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			if unquoted, e := strconv.Unquote(value); e == nil {
				value = unquoted
			} else {
				return nil, fmt.Errorf("invalid quoted LAB_ENV entry")
			}
		}
		values[key] = value
	}
	return values, scanner.Err()
}

func labReference(t *testing.T, values map[string]string, prefix, base string) labReferenceData {
	t.Helper()
	username, password := values[prefix+"_ADULT_USER"], values[prefix+"_ADULT_PASSWORD"]
	if username == "" || password == "" {
		t.Fatalf("%s adult credentials are missing from LAB_ENV", prefix)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal("cannot allocate unique lab device")
	}
	suffix := hex.EncodeToString(random[:])
	loginDevice, probeDevice := "ether-login-"+suffix, "ether-probe-"+suffix
	body, _ := json.Marshal(map[string]string{"Username": username, "Pw": password})
	req, err := http.NewRequest("POST", strings.TrimRight(base, "/")+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		t.Fatal("invalid lab URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+loginDevice+`", Version="1"`)
	req.Header.Set("X-Emby-Authorization", req.Header.Get("Authorization"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("lab authentication request failed")
	}
	var login struct {
		AccessToken, ServerId string
		SessionInfo           struct{ Id string }
		User                  struct{ Id string }
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&login)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || login.AccessToken == "" || login.User.Id == "" {
		t.Fatalf("lab authentication failed (HTTP %d)", resp.StatusCode)
	}
	ref := labReferenceData{base: strings.TrimRight(base, "/"), token: login.AccessToken, device: loginDevice, session: login.SessionInfo.Id}
	t.Logf("lab_owned_reference protocol=%s device=%s probe_device=%s session=%s", prefix, loginDevice, probeDevice, login.SessionInfo.Id)
	completed := false
	defer func() {
		if !completed {
			ref.logout(t)
		}
	}()
	get := func(path string, out any) {
		t.Helper()
		req, err := http.NewRequest("GET", strings.TrimRight(base, "/")+path, nil)
		if err != nil {
			t.Fatal("invalid lab request")
		}
		req.Header.Set("X-Emby-Token", login.AccessToken)
		req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+loginDevice+`", Version="1", Token="`+login.AccessToken+`"`)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("lab discovery request failed")
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("lab discovery failed (HTTP %d)", response.StatusCode)
		}
	}
	var listing struct {
		Items []struct {
			Id          string
			ProviderIds map[string]string
		}
	}
	get("/Items?Recursive=true&Fields=ProviderIds", &listing)
	itemID := ""
	for _, item := range listing.Items {
		if item.ProviderIds["Tmdb"] == "10378" {
			itemID = item.Id
			break
		}
	}
	if itemID == "" {
		t.Fatal("lab Big Buck Bunny TMDB item is missing")
	}
	var views struct {
		Items []struct{ Id, CollectionType string }
	}
	get("/Users/"+login.User.Id+"/Views", &views)
	viewID := ""
	for _, view := range views.Items {
		if strings.EqualFold(view.CollectionType, "movies") {
			viewID = view.Id
			break
		}
	}
	if viewID == "" {
		t.Fatal("lab movie view is missing")
	}
	var baseline json.RawMessage
	get("/Users/"+login.User.Id+"/Items/"+itemID, &baseline)
	var itemState struct {
		UserData     json.RawMessage
		MediaSources []struct{ Id string }
	}
	if json.Unmarshal(baseline, &itemState) != nil {
		t.Fatal("reference state snapshot invalid")
	}
	state, _ := json.Marshal(itemState)
	t.Logf("lab_reference_baseline protocol=%s item=%s state_source_metadata_sha256=%x", prefix, itemID, sha256.Sum256(state))
	completed = true
	return labReferenceData{base: strings.TrimRight(base, "/"), token: login.AccessToken, device: loginDevice, session: login.SessionInfo.Id, vars: map[string]string{
		"username": username, "password": password, "user_id": login.User.Id, "access_token": login.AccessToken,
		"item_id": itemID, "itemId": itemID, "parent_id": viewID, "media_source_id": itemID,
		"play_session_id": "initial", "position_ticks": "0", "device_id": probeDevice, "server_id": login.ServerId,
		"provider:tmdb:10378": itemID,
	}}
}

func TestReadLabSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.env")
	if err := os.WriteFile(path, []byte("# fixture\nexport USER=viewer\nPASSWORD='literal # value'\nURL=\"http://127.0.0.1:8096\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := readLabSettings(path)
	if err != nil || settings["USER"] != "viewer" || settings["PASSWORD"] != "literal # value" || settings["URL"] != "http://127.0.0.1:8096" {
		t.Fatal("settings parsing failed", err)
	}
}

func TestLabProbeProxyRevokesIssuedCredentials(t *testing.T) {
	var logouts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/AuthenticateByName":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AccessToken":"probe-only-token","SessionInfo":{"DeviceId":"owned-device"}}`)
		case "/emby/Sessions/Logout":
			if r.Header.Get("X-Emby-Token") != "probe-only-token" {
				t.Error("wrong sign-out credential")
			}
			logouts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	proxy, stop := labProbeProxy(t, labReferenceData{base: backend.URL + "/emby", vars: map[string]string{"device_id": "owned-device"}})
	req, _ := http.NewRequest("POST", proxy+"/Users/AuthenticateByName", strings.NewReader(`{}`))
	req.Header.Set("Authorization", `MediaBrowser DeviceId="owned-device"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	stop()
	if resp.StatusCode != 200 || logouts.Load() != 1 {
		t.Fatal(resp.StatusCode, logouts.Load())
	}
}

func TestReadLabSettingsFromEnvironment(t *testing.T) {
	t.Setenv("JELLYFIN_ADULT_USER", "fixture-user")
	t.Setenv("JELLYFIN_ADULT_PASSWORD", "fixture-password")
	t.Setenv("EMBY_ADULT_USER", "emby-user")
	t.Setenv("EMBY_ADULT_PASSWORD", "emby-password")
	settings, err := readLabSettings("env")
	if err != nil || settings["JELLYFIN_ADULT_USER"] != "fixture-user" || settings["JELLYFIN_ADULT_PASSWORD"] != "fixture-password" || settings["EMBY_ADULT_USER"] != "emby-user" || settings["EMBY_ADULT_PASSWORD"] != "emby-password" {
		t.Fatal("environment settings were not loaded")
	}
}

// labTraceMediaDetail records only policy-authorized source counts and wire
// field presence. It never logs credentials, media paths or source names.
func labTraceMediaDetail(t *testing.T, a *App, viewer *core.User, protocol, stage, referenceItem string, srv *httptest.Server, token, itemID, deviceID string) {
	if os.Getenv("LAB_TRACE_DETAIL") != "1" {
		return
	}
	t.Helper()
	client, name, version := "", "", ""
	if deviceID != "" {
		client, name, version = "Findroid", "Android", "0.15.0"
		if protocol == "emby" {
			client, name, version = "Emby.SDK v2 reference client", "Unknown", "4.10"
		}
	}
	experience, err := a.newSurfaceExperience(core.Protocol(protocol))
	if err != nil {
		t.Error("detail trace experience unavailable")
		return
	}
	dev := frontend.Device{ID: deviceID, Client: client, Product: client, DeviceName: name, Version: version}
	detail, err := experience.Item(context.Background(), frontend.ItemRequest{Viewer: viewer, Device: dev, Key: core.MovieKey(10378)})
	if err != nil {
		t.Error("detail trace item unavailable")
		return
	}
	issueOutcome := "no_versions"
	if len(detail.Versions) != 0 {
		issueOutcome = "host_unavailable"
		ctx := frontend.WithClientIP(context.Background(), netip.MustParseAddr("198.51.100.1"))
		capability, capErr := (surfaceCapabilities{app: a, surface: core.Protocol(protocol)}).Build(ctx, frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: core.Protocol(protocol), ClientID: deviceID, Product: client, DeviceName: name, Version: version}})
		if capErr != nil {
			issueOutcome = "scope_error"
		} else if handles, handleErr := sessions.NewVersionHandles(a.db, a.frontends.broker, core.Protocol(protocol)); handleErr == nil {
			_, issueErr := handles.Issue(ctx, frontend.VersionScope{Viewer: viewer, Device: dev, Protocol: core.Protocol(protocol), Caps: capability.Client}, core.MovieKey(10378), detail.Versions[0].ID)
			switch {
			case issueErr == nil:
				issueOutcome = "issued"
			case errors.Is(issueErr, core.ErrNotFound):
				issueOutcome = "not_found"
			case errors.Is(issueErr, core.ErrUnauthorized):
				issueOutcome = "unauthorized"
			default:
				issueOutcome = "other_error"
			}
		}
	}
	req, err := http.NewRequest("GET", srv.URL+"/Items/"+itemID, nil)
	if err != nil {
		t.Error("detail trace request invalid")
		return
	}
	req.Header.Set("X-Emby-Token", token)
	if deviceID != "" {
		req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="`+client+`", Device="`+name+`", DeviceId="`+deviceID+`", Version="`+version+`"`)
	}
	response, err := srv.Client().Do(req)
	if err != nil {
		t.Error("detail trace request failed")
		return
	}
	defer response.Body.Close()
	var body map[string]any
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&body) != nil {
		t.Error("detail trace response unavailable")
		return
	}
	sources, _ := body["MediaSources"].([]any)
	streams, _ := body["MediaStreams"].([]any)
	fields := []string{}
	for _, key := range []string{"MediaSources", "MediaStreams", "Container", "Bitrate", "Size", "Width", "Height", "UserData"} {
		if _, ok := body[key]; ok {
			fields = append(fields, key)
		}
	}
	versionHashes := make([]string, 0, len(detail.Versions))
	for _, version := range detail.Versions {
		if version.ID != "" {
			sum := sha256.Sum256([]byte(version.ID))
			versionHashes = append(versionHashes, hex.EncodeToString(sum[:8]))
		}
	}
	sourceHashes := make([]string, 0, len(sources))
	for _, value := range sources {
		if source, ok := value.(map[string]any); ok {
			if id, ok := source["Id"].(string); ok && id != "" {
				sum := sha256.Sum256([]byte(id))
				sourceHashes = append(sourceHashes, hex.EncodeToString(sum[:8]))
			}
		}
	}
	t.Logf("detail_trace protocol=%s stage=%s device_present=%t issue=%s reference_item=%s target_item=%s authorized_versions=%d version_hashes=%v wire_sources=%d source_hashes=%v wire_streams=%d fields=%v", protocol, stage, deviceID != "", issueOutcome, referenceItem, itemID, len(detail.Versions), versionHashes, len(sources), sourceHashes, len(streams), fields)
}

func TestLabProbeProxyStopsOnlyOwnedPendingPlayback(t *testing.T) {
	var stopped, loggedOut atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_, _ = io.WriteString(w, `{"AccessToken":"owned-probe-token","SessionInfo":{"Id":"owned-session","DeviceId":"owned-device"}}`)
		case "/Sessions/Playing":
			w.WriteHeader(http.StatusNoContent)
		case "/Sessions/Playing/Stopped":
			if r.Header.Get("X-Emby-Token") != "owned-probe-token" {
				t.Error("unexpected cleanup credential")
			}
			var body struct{ ItemId, PlaySessionId string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.ItemId != "owned-item" || body.PlaySessionId != "owned-play" {
				t.Error("cleanup lost exact playback IDs")
			}
			stopped.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/Sessions/Logout":
			if stopped.Load() != 1 {
				t.Error("logout preceded playback stop")
			}
			loggedOut.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	proxy, cleanup := labProbeProxy(t, labReferenceData{base: backend.URL, vars: map[string]string{"device_id": "owned-device"}})
	loginReq, _ := http.NewRequest("POST", proxy+"/Users/AuthenticateByName", strings.NewReader(`{}`))
	loginReq.Header.Set("Authorization", `MediaBrowser DeviceId="owned-device"`)
	login, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	req, _ := http.NewRequest("POST", proxy+"/Sessions/Playing", strings.NewReader(`{"ItemId":"owned-item","PlaySessionId":"owned-play"}`))
	req.Header.Set("X-Emby-Token", "owned-probe-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	cleanup()
	if stopped.Load() != 1 || loggedOut.Load() != 1 {
		t.Fatal("owned cleanup incomplete")
	}
}

func TestLabProbeProxyConcurrentPlaybackAndRejectedIdentity(t *testing.T) {
	var mu sync.Mutex
	stops := map[string]int{}
	forwarded := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		forwarded++
		if strings.HasSuffix(r.URL.Path, "/Stopped") {
			var ids struct{ PlaySessionId string }
			_ = json.NewDecoder(r.Body).Decode(&ids)
			stops[ids.PlaySessionId]++
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	proxy, cleanup := labProbeProxy(t, labReferenceData{base: backend.URL, token: "owned", device: "owned-device"})
	send := func(path, body, token, auth string) int {
		req, _ := http.NewRequest("POST", proxy+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("X-Emby-Token", token)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	one := `{"ItemId":"item","PlaySessionId":"one"}`
	two := `{"ItemId":"item","PlaySessionId":"two"}`
	for _, body := range []string{one, two} {
		if send("/Sessions/Playing", body, "owned", "") != 204 {
			t.Fatal("owned play rejected")
		}
	}
	if send("/Sessions/Playing/Stopped", one, "owned", "") != 204 {
		t.Fatal("partial stop rejected")
	}
	mu.Lock()
	before := forwarded
	mu.Unlock()
	for _, args := range [][3]string{
		{one, "foreign", ""}, {one, "", ""}, {one, "owned", `MediaBrowser Token="foreign"`},
		{one, "owned", `MediaBrowser DeviceId="foreign-device"`},
		{"", "owned", ""}, {strings.Repeat("x", (1<<20)+1), "owned", ""},
	} {
		if code := send("/Sessions/Playing", args[0], args[1], args[2]); code != 403 && code != 400 {
			t.Fatalf("unsafe input forwarded: %d", code)
		}
	}
	mu.Lock()
	after := forwarded
	mu.Unlock()
	if before != after {
		t.Fatal("rejected request reached reference")
	}
	cleanup()
	mu.Lock()
	defer mu.Unlock()
	if stops["one"] != 1 || stops["two"] != 1 || len(stops) != 2 {
		t.Fatalf("partial stop lost or duplicated cleanup: %v", stops)
	}
}

func TestLabRequestIdentityAuthForms(t *testing.T) {
	for _, header := range []string{"Authorization", "X-Emby-Authorization"} {
		req, _ := http.NewRequest("POST", "http://localhost", nil)
		req.Header.Set(header, `MediaBrowser Client="conformdiff", DeviceId="owned-device", Version="1", Token="owned-token"`)
		req.Header.Set("X-Emby-Token", "owned-token")
		token, device, err := labRequestIdentity(req)
		if err != nil || token != "owned-token" || device != "owned-device" {
			t.Fatal("conformdiff auth form was not recognized")
		}
	}
}

func TestLabProbeProxyQuarantinesMintedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, authHeader, authValue, response string
		compressed                            bool
		status                                int
	}{
		{"findroid-gzip", "Authorization", `MediaBrowser Client="Findroid", Device="Android", DeviceId="owned-device", Version="0.15.0", Token="primary-token"`, `{"AccessToken":"minted-token","SessionInfo":{"Id":"minted-session","DeviceId":"owned-device"}}`, true, 200},
		{"emby-gzip", "X-Emby-Authorization", `MediaBrowser Client="Emby.SDK v2 reference client", Device="Unknown", DeviceId="owned-device", Version="4.10"`, `{"AccessToken":"minted-token","SessionInfo":{"Id":"minted-session","DeviceId":"owned-device"}}`, true, 200},
		{"malformed-metadata-after-mint", "Authorization", `MediaBrowser DeviceId="owned-device"`, `{"AccessToken":"minted-token","SessionInfo":{"Id":42}}`, false, 502},
		{"wrong-device-after-mint", "Authorization", `MediaBrowser DeviceId="owned-device"`, `{"AccessToken":"minted-token","SessionInfo":{"Id":"minted-session","DeviceId":"foreign-device"}}`, false, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logouts, plays atomic.Int32
			var wire bytes.Buffer
			if tc.compressed {
				g := gzip.NewWriter(&wire)
				_, _ = g.Write([]byte(tc.response))
				_ = g.Close()
			} else {
				wire.WriteString(tc.response)
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/Users/AuthenticateByName":
					w.Header().Set("Content-Type", "application/json")
					if tc.compressed {
						w.Header().Set("Content-Encoding", "gzip")
					}
					_, _ = w.Write(wire.Bytes())
				case "/Sessions/Playing":
					plays.Add(1)
					w.WriteHeader(204)
				case "/Sessions/Logout":
					token, device, err := labRequestIdentity(r)
					if err != nil || token != "minted-token" || device != "owned-device" {
						t.Error("quarantined cleanup lost verified request ownership")
					}
					logouts.Add(1)
					w.WriteHeader(204)
				default:
					http.NotFound(w, r)
				}
			}))
			defer backend.Close()
			proxy, cleanup := labProbeProxy(t, labReferenceData{base: backend.URL, token: "primary-token", device: "primary-device", vars: map[string]string{"device_id": "owned-device"}})
			req, _ := http.NewRequest("POST", proxy+"/Users/AuthenticateByName", strings.NewReader(`{"Username":"fixture","Pw":"fixture"}`))
			req.Header.Set(tc.authHeader, tc.authValue)
			if tc.authHeader == "X-Emby-Authorization" {
				req.Header.Set("X-Emby-Token", "primary-token")
			}
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != tc.status {
				t.Fatalf("login status=%d expected=%d", resp.StatusCode, tc.status)
			}
			if tc.status == 200 && (!bytes.Equal(body, wire.Bytes()) || (tc.compressed && resp.Header.Get("Content-Encoding") != "gzip")) {
				t.Error("response wire bytes or encoding changed")
			}
			if tc.status == 502 {
				play, _ := http.NewRequest("POST", proxy+"/Sessions/Playing", strings.NewReader(`{"ItemId":"item","PlaySessionId":"play"}`))
				play.Header.Set("X-Emby-Token", "minted-token")
				rejected, err := http.DefaultClient.Do(play)
				if err != nil {
					t.Fatal(err)
				}
				rejected.Body.Close()
				if rejected.StatusCode != 403 || plays.Load() != 0 {
					t.Error("quarantined token became usable")
				}
			}
			cleanup()
			if logouts.Load() != 1 {
				t.Fatal("credential minted before metadata failure was not revoked")
			}
		})
	}
}

func TestLabProbeProxyRejectsForeignLoginBeforeMint(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer backend.Close()
	proxy, cleanup := labProbeProxy(t, labReferenceData{base: backend.URL, vars: map[string]string{"device_id": "owned-device"}})
	for _, auth := range []string{`MediaBrowser DeviceId="foreign-device"`, `MediaBrowser DeviceId="owned-device", Token="conflict"`, ""} {
		req, _ := http.NewRequest("POST", proxy+"/Users/AuthenticateByName", strings.NewReader(`{}`))
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Emby-Token", "primary")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatal("ambiguous/foreign login reached upstream")
		}
	}
	cleanup()
	if calls.Load() != 0 {
		t.Fatal("unowned login minted or revoked credentials")
	}
}
