//go:build plexparity && conformlab

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	tmdbpkg "github.com/braedonsaunders/ether/internal/tmdb"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Run: LAB_ENV=/path/to/lab.env LAB_JELLYFIN_URL=http://host:8096 LAB_EMBY_URL=http://host:8097/emby go test -tags plexparity,conformlab ./internal/app -run TestMediaBrowserLabConformance -count=1 -v
// LAB_REPORT_DIR optionally retains redacted JSON reports; otherwise only counts
// are printed. Credentials and generated media stay in a private temporary dir.
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
	defer tmdb.Close()
	t.Setenv("ETHER_PARITY_TMDB_URL", tmdb.URL+"/3")
	dir := t.TempDir()
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Fatal("ffmpeg is required for the opt-in lab harness")
	}
	makeMedia := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "15", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", filepath.Join(dir, "parity.mp4"))
	if output, e := makeMedia.CombinedOutput(); e != nil {
		t.Fatalf("generate fixture media: %v: %s", e, output)
	}
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
		for i := range c.Quality.Profiles {
			c.Quality.Profiles[i].MinSizeMBPerMin = 0
			c.Quality.Profiles[i].MaxSizeMBPerMin = 0
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	a, e := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
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
	request.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="lab-conform", Version="1", Token="`+jellyfinRef.token+`"`)
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
		if e = a.catalog.UpsertTitle(ctx, title, true); e != nil {
			t.Fatal(e)
		}
	}
	if _, err := a.catalog.Similar(ctx, core.MovieKey(10378), 5); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		proto, client string
		reference     labReferenceData
	}{{"jellyfin", "findroid,jellycon,jellyfin-androidtv,streamyfin,swiftfin", jellyfinRef}, {"emby", "emby-sdk", embyRef}} {
		t.Run(spec.proto, func(t *testing.T) {
			inst, e := a.frontends.registry.Mount(ctx, core.Protocol(spec.proto))
			if e != nil {
				t.Fatal(e)
			}
			defer a.frontends.registry.Unmount(ctx, core.Protocol(spec.proto))
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
			for _, mode := range []string{"compare", "live"} {
				report := conform.Report{}
				for _, clientName := range strings.Split(spec.client, ",") {
					values["device_id"] = "conform-" + clientName
					b, _ = json.Marshal(values)
					if err := os.WriteFile(targetFile, b, 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"run", "./cmd/conformdiff", mode, "-protocol", spec.proto, "-client", clientName, "-target", srv.URL, "-target-vars", targetFile, "-reference-vars", refFile, "-format", "json", "-timeout", "15s"}
					if mode == "live" {
						args = append(args, "-reference-server", spec.reference.base)
					}
					cmd := exec.Command("go", args...)
					cmd.Dir = "../.."
					cmd.Env = append(os.Environ(), "CONFORM_TARGET_TOKEN="+login.AccessToken, "CONFORM_REFERENCE_TOKEN="+spec.reference.token)
					out, err := cmd.Output()
					var partial conform.Report
					if e = json.Unmarshal(out, &partial); e != nil {
						t.Errorf("%s %s %s invalid report: %v", spec.proto, mode, clientName, err)
						continue
					}
					for _, diff := range partial.Differences {
						if mode == "live" && diff.Kind == conform.KindStatus && diff.Reference == float64(http.StatusUnauthorized) {
							t.Errorf("%s %s reference authorization expired; comparison is invalid", spec.proto, clientName)
							break
						}
					}
					report.Differences = append(report.Differences, partial.Differences...)
					report.ReferenceDefects = append(report.ReferenceDefects, partial.ReferenceDefects...)
					for _, session := range (ownerPlayback{Playback: a.playback, app: a}).Sessions() {
						_ = a.stopOwnerSession(session.ID)
					}
				}
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

		})
	}
}

type labReferenceData struct {
	base, token string
	vars        map[string]string
}

func (ref labReferenceData) logout(t *testing.T) {
	t.Helper()
	req, err := http.NewRequest("POST", ref.base+"/Sessions/Logout", nil)
	if err != nil {
		t.Error("could not construct lab sign-out")
		return
	}
	req.Header.Set("X-Emby-Token", ref.token)
	req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="lab-conform", Version="1", Token="`+ref.token+`"`)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Error("lab sign-out failed")
		return
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Errorf("lab sign-out returned HTTP %d", response.StatusCode)
	}
}

func readLabSettings(path string) (map[string]string, error) {
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
	body, _ := json.Marshal(map[string]string{"Username": username, "Pw": password})
	req, err := http.NewRequest("POST", strings.TrimRight(base, "/")+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		t.Fatal("invalid lab URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="lab-login", Version="1"`)
	req.Header.Set("X-Emby-Authorization", req.Header.Get("Authorization"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("lab authentication request failed")
	}
	var login struct {
		AccessToken, ServerId string
		User                  struct{ Id string }
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&login)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || login.AccessToken == "" || login.User.Id == "" {
		t.Fatalf("lab authentication failed (HTTP %d)", resp.StatusCode)
	}
	ref := labReferenceData{base: strings.TrimRight(base, "/"), token: login.AccessToken}
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
		req.Header.Set("Authorization", `MediaBrowser Client="Conform", Device="Conform", DeviceId="lab-conform", Version="1", Token="`+login.AccessToken+`"`)
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
	completed = true
	return labReferenceData{base: strings.TrimRight(base, "/"), token: login.AccessToken, vars: map[string]string{
		"username": username, "password": password, "user_id": login.User.Id, "access_token": login.AccessToken,
		"item_id": itemID, "itemId": itemID, "parent_id": viewID, "media_source_id": itemID,
		"play_session_id": "initial", "position_ticks": "0", "device_id": "lab-run", "server_id": login.ServerId,
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
