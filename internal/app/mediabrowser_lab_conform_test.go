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
	mbconform "github.com/braedonsaunders/ether/internal/conform/mediabrowser"
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
// media stay in a private temporary dir. Credential variables use anonymous pipes.
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
	conformBinary := filepath.Join(t.TempDir(), "conformdiff")
	build := exec.Command("go", "build", "-o", conformBinary, "./cmd/conformdiff")
	build.Dir = "../.."
	if err := build.Run(); err != nil {
		t.Fatal("build lab comparison tool failed")
	}
	t.Log("lab_baseline=isolated-profile-phase-v1; old counts and performance are not comparable")
	summary := conformance.Manifest{Schema: 1, Protocols: map[string]conformance.Summary{}}
	for _, spec := range []struct {
		proto, client string
		reference     labReferenceData
	}{{"jellyfin", "findroid,jellycon,jellyfin-androidtv,streamyfin,swiftfin,infuse-jellyfin", jellyfinRef}, {"emby", "emby-sdk", embyRef}} {
		t.Run(spec.proto, func(t *testing.T) {
			combined := map[string]bool{}
			validEvidence := true
			protocolVersion := ""
			probeDevices := map[string]string{}
			allowedProbeDevices := []string{}
			for _, clientName := range strings.Split(spec.client, ",") {
				probeDevices[clientName] = spec.reference.vars["device_id"] + "-" + clientName
				allowedProbeDevices = append(allowedProbeDevices, probeDevices[clientName])
			}
			labProxy, stopProxy := labProbeProxy(t, spec.reference, allowedProbeDevices...)
			defer stopProxy()
			for _, mode := range []string{"compare", "live"} {
				report := conform.Report{}
				for _, clientName := range strings.Split(spec.client, ",") {
					passed := t.Run(mode+"/"+clientName, func(t *testing.T) {
						target := newLabProfileTarget(t, jellyfinRef, spec.proto, clientName)
						defer target.close()
						protocolVersion = target.version
						referenceValues := map[string]string{}
						for key, value := range spec.reference.vars {
							referenceValues[key] = value
						}
						referenceValues["device_id"] = probeDevices[clientName]
						t.Logf("lab_profile_reference_device protocol=%s mode=%s client=%s device=%s", spec.proto, mode, clientName, probeDevices[clientName])
						args := []string{mode, "-protocol", spec.proto, "-client", clientName, "-target", target.server.URL, "-target-vars", "/dev/fd/3", "-reference-vars", "/dev/fd/4", "-format", "json", "-timeout", "15s"}
						if mode == "live" {
							args = append(args, "-reference-server", labProxy)
						}
						cmd := exec.Command(conformBinary, args...)
						cmd.Dir = "../.."
						cmd.Env = append(os.Environ(), "CONFORM_TARGET_TOKEN="+target.values["access_token"], "CONFORM_REFERENCE_TOKEN="+spec.reference.token)
						out, err := labCommandVariables(cmd, target.values, referenceValues)
						var partial conform.Report
						if errJSON := json.Unmarshal(out, &partial); errJSON != nil {
							validEvidence = false
							t.Errorf("%s %s %s invalid report (command failed=%t)", spec.proto, mode, clientName, err != nil)
							return
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
					})
					if !passed {
						validEvidence = false
					}
				}
				if reportDir := os.Getenv("LAB_REPORT_DIR"); reportDir != "" {
					reportDir = filepath.Join(reportDir, "isolated-profile-phase-v1")
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
				summary.Protocols[spec.proto] = conformance.Summary{ProtocolVersion: protocolVersion, Date: time.Now().UTC().Format(time.RFC3339), Blocking: &count, CertifiedClients: []string{}}
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

// labCredentialCleanup never contains a token or response body. A separate,
// still-live credential proves the protected endpoint healthy on both sides
// of the exact owned token's denial check, including an already-invalid token.
type labCredentialCleanup struct {
	LogoutStatus, ProtectedStatus, ControlBefore, ControlAfter int
	Verified                                                   bool
}

func (ref labReferenceData) logoutWithProtectedDenial(control labReferenceData) labCredentialCleanup {
	result := labCredentialCleanup{}
	if ref.token == "" || ref.device == "" || control.token == "" || control.device == "" || ref.token == control.token || ref.base != control.base {
		return result
	}
	client := &http.Client{Timeout: 15 * time.Second}
	request := func(owner labReferenceData, path, method string) int {
		req, err := http.NewRequest(method, owner.base+path, nil)
		if err != nil {
			return 0
		}
		req.Header.Set("X-Emby-Token", owner.token)
		req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="`+owner.device+`", Version="1", Token="`+owner.token+`"`)
		response, err := client.Do(req)
		if err != nil {
			return 0
		}
		response.Body.Close()
		return response.StatusCode
	}
	result.ControlBefore = request(control, "/Users/Me", "GET")
	result.LogoutStatus = request(ref, "/Sessions/Logout", "POST")
	result.ProtectedStatus = request(ref, "/Users/Me", "GET")
	result.ControlAfter = request(control, "/Users/Me", "GET")
	logoutAccepted := (result.LogoutStatus >= 200 && result.LogoutStatus < 300) || result.LogoutStatus == 401
	result.Verified = logoutAccepted && result.ProtectedStatus == 401 && result.ControlBefore == 200 && result.ControlAfter == 200
	return result
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

func labProbeProxy(t *testing.T, ref labReferenceData, profileDevices ...string) (string, func()) {
	t.Helper()
	target, err := url.Parse(ref.base)
	if err != nil {
		t.Fatal("invalid lab proxy target")
	}
	var mu sync.Mutex
	ownedDevices := map[string]bool{}
	for _, device := range append(profileDevices, ref.device, ref.vars["device_id"]) {
		if device != "" {
			ownedDevices[device] = true
		}
	}
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
			if err != nil || device == "" || !ownedDevices[device] {
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
		if device == "" || (device != owner.device && !(token == ref.token && ownedDevices[device])) {
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
			receipt := owner.logoutWithProtectedDenial(ref)
			t.Logf("lab_probe_cleanup device=%s session=%s logout_status=%d protected_status=%d control_before=%d control_after=%d verified=%t", owner.device, owner.session, receipt.LogoutStatus, receipt.ProtectedStatus, receipt.ControlBefore, receipt.ControlAfter, receipt.Verified)
			if !receipt.Verified {
				t.Error("owned probe credential cleanup denial not verified")
			}
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
		case "/emby/Users/Me":
			if r.Header.Get("X-Emby-Token") == "control-token" {
				w.WriteHeader(200)
			} else if logouts.Load() > 0 {
				w.WriteHeader(401)
			} else {
				w.WriteHeader(200)
			}
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
	proxy, stop := labProbeProxy(t, labReferenceData{base: backend.URL + "/emby", token: "control-token", device: "control-device", vars: map[string]string{"device_id": "owned-device"}})
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
		case "/Users/Me":
			if r.Header.Get("X-Emby-Token") == "control-token" {
				w.WriteHeader(200)
			} else if loggedOut.Load() > 0 {
				w.WriteHeader(401)
			} else {
				w.WriteHeader(200)
			}
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
	proxy, cleanup := labProbeProxy(t, labReferenceData{base: backend.URL, token: "control-token", device: "control-device", vars: map[string]string{"device_id": "owned-device"}})
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
				case "/Users/Me":
					if r.Header.Get("X-Emby-Token") == "primary-token" {
						w.WriteHeader(200)
					} else if logouts.Load() > 0 {
						w.WriteHeader(401)
					} else {
						w.WriteHeader(200)
					}
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

func TestLabCleanupRequiresProtectedDenialAndHealthyControl(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		logout, denial, controlBefore, controlAfter int
		verified                                    bool
	}{
		{"normal-logout", 204, 401, 200, 200, true},
		{"already-invalid-explicit-denial", 401, 401, 200, 200, true},
		{"logout401-token-still-valid", 401, 200, 200, 200, false},
		{"logout204-token-still-valid", 204, 200, 200, 200, false},
		{"all-auth-broken", 401, 401, 401, 401, false},
		{"endpoint-failed-after-logout", 204, 401, 200, 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var controls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token, device, err := labRequestIdentity(r)
				if err != nil {
					t.Error("invalid auth headers")
					w.WriteHeader(400)
					return
				}
				if token == "control-token" {
					if device != "control-device" || r.URL.Path != "/Users/Me" || r.Method != "GET" {
						t.Error("control credential used outside health request")
					}
					if controls.Add(1) == 1 {
						w.WriteHeader(tc.controlBefore)
					} else {
						w.WriteHeader(tc.controlAfter)
					}
					return
				}
				if token != "owned-token" || device != "owned-device" {
					t.Error("foreign cleanup")
					w.WriteHeader(403)
					return
				}
				if r.URL.Path == "/Sessions/Logout" && r.Method == "POST" {
					w.WriteHeader(tc.logout)
				} else if r.URL.Path == "/Users/Me" && r.Method == "GET" {
					w.WriteHeader(tc.denial)
				} else {
					t.Error("unexpected cleanup request")
					w.WriteHeader(404)
				}
			}))
			defer backend.Close()
			result := (labReferenceData{base: backend.URL, token: "owned-token", device: "owned-device"}).logoutWithProtectedDenial(labReferenceData{base: backend.URL, token: "control-token", device: "control-device"})
			if result.Verified != tc.verified || result.LogoutStatus != tc.logout || result.ProtectedStatus != tc.denial || controls.Load() != 2 {
				t.Fatalf("cleanup proof mismatch: %+v", result)
			}
		})
	}
}

func TestLabProbeProxyDistinctProfileDevices(t *testing.T) {
	var mu sync.Mutex
	active := map[string]bool{}
	logouts := map[string]int{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, device, err := labRequestIdentity(r)
		if err != nil {
			t.Error("auth identity invalid")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			if device != "owned-findroid" && device != "owned-swiftfin" {
				t.Error("foreign device authenticated")
			}
			active[device] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-" + device, "SessionInfo": map[string]string{"Id": "session-" + device, "DeviceId": device}})
		case "/Users/Me":
			if token == "primary-token" && device == "primary-device" {
				w.WriteHeader(200)
				return
			}
			if token == "token-"+device && active[device] {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(401)
			}
		case "/Sessions/Logout":
			if token != "token-"+device || !active[device] {
				t.Error("wrong cleanup identity")
				w.WriteHeader(401)
				return
			}
			active[device] = false
			logouts[device]++
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	proxy, cleanup := labProbeProxy(t, labReferenceData{base: backend.URL, token: "primary-token", device: "primary-device"}, "owned-findroid", "owned-swiftfin")
	for _, device := range []string{"owned-findroid", "owned-swiftfin"} {
		req, _ := http.NewRequest("POST", proxy+"/Users/AuthenticateByName", strings.NewReader(`{}`))
		req.Header.Set("Authorization", `MediaBrowser DeviceId="`+device+`"`)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("owned per-profile login failed")
		}
	}
	cross, _ := http.NewRequest("POST", proxy+"/Sessions/Playing", strings.NewReader(`{"ItemId":"item","PlaySessionId":"play"}`))
	cross.Header.Set("X-Emby-Token", "token-owned-findroid")
	cross.Header.Set("Authorization", `MediaBrowser DeviceId="owned-swiftfin"`)
	denied, err := http.DefaultClient.Do(cross)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != 403 {
		t.Fatal("issued token crossed profile device identity")
	}
	cleanup()
	mu.Lock()
	defer mu.Unlock()
	if active["owned-findroid"] || active["owned-swiftfin"] || logouts["owned-findroid"] != 1 || logouts["owned-swiftfin"] != 1 {
		t.Fatal("distinct profile identities not cleaned independently")
	}
}

// Each independent comparison owns a complete target lifetime, including its
// cache, viewer, broker plans, prepared sources and HTTP listener. Recorded and
// live phases intentionally do not share it. The historical shared-viewer
// sequence remains a separate UX regression candidate, not conformance evidence.
type labProfileTarget struct {
	app      *App
	viewer   *core.User
	server   *httptest.Server
	protocol *mbconform.Protocol
	values   map[string]string
	version  string
	close    func()
}

func newLabProfileTarget(t *testing.T, listing labReferenceData, dialect, profile string) *labProfileTarget {
	t.Helper()
	a, viewer, dispose := newAlignedLabFixture(t, listing, labFixtureOptions{PublicViewerPicker: map[string]bool{"emby": true}})
	target := &labProfileTarget{app: a, viewer: viewer}
	if len(a.playback.Sessions()) != 0 {
		t.Fatal("fresh target contains playback state")
	}
	fresh := a.frontends.broker.ConformLabLifecycleCounters(viewer.ID)
	for _, key := range []string{"plans", "pending", "source_leases", "broker_sessions"} {
		if fresh[key] != 0 {
			t.Fatal("fresh target contains broker state")
		}
	}
	target.close = sync.OnceFunc(func() {
		if target.server != nil {
			target.server.Close()
		}
		labTargetLifecycle(t, a, viewer.ID, "before_app_close")
		dispose()
		labTargetLifecycle(t, a, viewer.ID, "after_app_close")
		counts := a.frontends.broker.ConformLabLifecycleCounters(viewer.ID)
		for _, key := range []string{"plans", "pending", "source_leases", "source_plan_refs", "source_session_refs", "broker_sessions", "detail_inflight"} {
			if counts[key] != 0 {
				t.Errorf("target App.Close left %s=%d", key, counts[key])
			}
		}
		if len(a.playback.Sessions()) != 0 {
			t.Error("target App.Close left playback reservations")
		}
	})
	t.Cleanup(target.close)
	inst, err := a.frontends.registry.Mount(context.Background(), core.Protocol(dialect))
	if err != nil {
		t.Fatal("target mount failed")
	}
	target.version = inst.Status(context.Background()).TargetVersion
	var seq atomic.Uint32
	target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:1234", seq.Add(1)%200+1)
		inst.Handler().ServeHTTP(w, r)
	}))
	p, err := mbconform.Load(dialect, filepath.Join("..", "conform", "profiles", dialect))
	if err != nil {
		t.Fatal("profile load failed")
	}
	target.protocol = p
	userID, _ := ids.GUID(ids.User, viewer.ID)
	viewID, _ := ids.GUID(ids.View, 1)
	itemID := ids.RatingKeyGUID(core.MovieKey(10378))
	target.values = map[string]string{"username": "fixture-viewer", "password": "fixture-password", "device_id": "conform-" + profile, "user_id": userID, "item_id": itemID, "itemId": itemID, "media_source_id": itemID, "parent_id": viewID, "play_session_id": "initial", "position_ticks": "0"}
	corpus := p.Corpus(profile)
	if len(corpus) == 0 {
		t.Fatal("empty profile")
	}
	// Fetch expands the real profile headers, keeping preflight on this client's
	// DeviceId, product, version and capability scope (never Findroid defaults).
	headers := corpus[0].Headers
	// Infuse's captured corpus has unresolved identity templates (a separate
	// alignment task). Give only the fixture preflight a concrete current-client
	// identity; leave the corpus and its mismatches untouched.
	if profile == "infuse-jellyfin" {
		headers = map[string]http.Header{profile + "-json": {
			"Authorization": {`MediaBrowser Client="Infuse-Direct", Device="Conformance fixture", DeviceId="${device_id}", Version="8.5.9"`},
			"Content-Type":  {"application/json"},
		}}
	}
	loginReq := conform.Request{Method: "POST", Path: "/Users/AuthenticateByName", Headers: headers, Body: []byte(`{"Username":"fixture-viewer","Pw":"fixture-password"}`)}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := p.Fetch(context.Background(), client, target.server.URL, "", loginReq, profile, target.values, true)
	var login struct{ AccessToken string }
	if err != nil || response.Status != http.StatusOK || json.Unmarshal(response.Body, &login) != nil || login.AccessToken == "" {
		t.Fatal("target current-client login failed")
	}
	target.values["access_token"] = login.AccessToken
	for _, path := range []string{"/Items/" + itemID, "/Items?Recursive=true&Limit=50", "/Items/" + itemID + "/Similar"} {
		req := conform.Request{Method: "GET", Path: path, Headers: headers}
		response, err = p.Fetch(context.Background(), client, target.server.URL, login.AccessToken, req, profile, target.values, true)
		if err != nil || response.Status != http.StatusOK {
			t.Fatal("current-client preflight failed")
		}
		if path == "/Items/"+itemID {
			var item struct{ ImageTags map[string]string }
			if json.Unmarshal(response.Body, &item) != nil || item.ImageTags["Primary"] == "" {
				t.Fatal("item artwork not issued")
			}
			image, err := client.Get(target.server.URL + "/Items/" + itemID + "/Images/Primary?tag=" + url.QueryEscape(item.ImageTags["Primary"]))
			if err != nil {
				t.Fatal("artwork fetch failed")
			}
			_, err = io.Copy(io.Discard, image.Body)
			image.Body.Close()
			if err != nil || image.StatusCode != http.StatusOK || !strings.HasPrefix(image.Header.Get("Content-Type"), "image/") {
				t.Fatal("artwork fetch invalid")
			}
		}
		if strings.HasPrefix(path, "/Items?") {
			var grid struct {
				Items []struct {
					Id        string
					ImageTags map[string]string
				}
			}
			if json.Unmarshal(response.Body, &grid) != nil {
				t.Fatal("grid invalid")
			}
			found := false
			for _, entry := range grid.Items {
				if entry.Id == itemID && entry.ImageTags["Primary"] != "" {
					found = true
				}
			}
			if !found {
				t.Fatal("grid did not issue scoped artwork for seeded title")
			}
		}
		if strings.HasSuffix(path, "/Similar") && bytes.Contains(response.Body, []byte("Future hidden title")) {
			t.Fatal("unreleased candidate was exposed")
		}
	}
	labTargetLifecycle(t, a, viewer.ID, "after_current_client_preflight")
	return target
}

func labTargetLifecycle(t *testing.T, a *App, viewerID int64, stage string) {
	total, prepared, open := 0, 0, 0
	for _, source := range a.playback.Sessions() {
		total++
		if source.PreparedOnly {
			prepared++
		}
		if source.Source != nil {
			open++
		}
	}
	t.Logf("lab_target_lifecycle stage=%s playback_total=%d prepared=%d active=%d sources_open=%d owner_visible=%d broker=%v non_atomic=true", stage, total, prepared, total-prepared, open, len((ownerPlayback{Playback: a.playback, app: a}).Sessions()), a.frontends.broker.ConformLabLifecycleCounters(viewerID))
}

// Anonymous inherited pipes avoid persisting token-bearing variable JSON. Pipe
// writers finish on both success and early child exit; no background work survives.
func labCommandVariables(cmd *exec.Cmd, variables ...map[string]string) ([]byte, error) {
	var readers, writers []*os.File
	var payloads [][]byte
	defer func() {
		for _, f := range readers {
			f.Close()
		}
		for _, f := range writers {
			f.Close()
		}
	}()
	for _, values := range variables {
		payload, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		readers = append(readers, r)
		writers = append(writers, w)
		payloads = append(payloads, payload)
	}
	cmd.ExtraFiles = readers
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func(w *os.File, payload []byte) { defer wg.Done(); defer w.Close(); _, _ = w.Write(payload) }(w, payloads[i])
	}
	out, err := cmd.Output()
	for _, r := range readers {
		r.Close()
	}
	wg.Wait()
	return out, err
}
