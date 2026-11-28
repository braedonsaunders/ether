package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/diagnostics"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/perfharness"
	"github.com/braedonsaunders/ether/internal/store"
)

type perfMatrixCase struct {
	name, movieMode, showMode, visibility                 string
	upcomingMovies, upcomingEpisodes, sources, live, rows bool
	viewer                                                int
}

var perfMatrixCases = []perfMatrixCase{
	{name: "baseline"},
	{name: "playable", movieMode: config.ModePlayable, showMode: config.ModePlayable},
	{name: "live", live: true},
	{name: "playable-restricted", movieMode: config.ModePlayable, showMode: config.ModePlayable, visibility: "hide", viewer: perfharness.RestrictedViewer},
	{name: "digital", movieMode: config.ModeDigital, showMode: config.ModeDigital},
	{name: "playable-movies", movieMode: config.ModePlayable, showMode: config.ModeDigital},
	{name: "playable-shows", movieMode: config.ModeDigital, showMode: config.ModePlayable},
	{name: "hide-default", visibility: "hide"},
	{name: "ordered-rules", visibility: "show"},
	{name: "upcoming-movies", upcomingMovies: true},
	{name: "upcoming-episodes", upcomingEpisodes: true},
	{name: "new-viewer", viewer: 1},
	{name: "restricted", viewer: perfharness.RestrictedViewer},
	{name: "plex-home", viewer: 3},
	{name: "recommendations", rows: true, live: true},
	{name: "sources", sources: true},
	{name: "playable-upcoming-sources", movieMode: config.ModePlayable, showMode: config.ModePlayable, upcomingMovies: true, upcomingEpisodes: true, sources: true},
	{name: "live-playable-rules", movieMode: config.ModePlayable, showMode: config.ModePlayable, visibility: "show", live: true},
}

var perfMatrixScale = perfharness.Scale{
	Movies: 35_000, Shows: 15_000, SeasonsPerShow: 3, EpisodesPerSeason: 6,
	Viewers: 2, HistoryPerViewer: 20_000, InProgressPerViewer: 40, PluginItems: 10_000,
}

func testPerformanceMatrix(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	cache := os.Getenv("ETHER_PERF_GATE_FIXTURES")
	if cache == "" {
		cache = t.TempDir()
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	var report perfharness.MatrixReport
	path := os.Getenv("ETHER_PERF_GATE_REPORT")
	writeReport := func() {
		if path == "" {
			return
		}
		var out bytes.Buffer
		report.Write(&out, time.Since(start))
		if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
			t.Error("write matrix report:", err)
		}
	}
	t.Cleanup(func() {
		writeReport()
		t.Logf("matrix elapsed %s, %d over-budget route/configuration pairs, %d coverage gaps", time.Since(start).Round(time.Second), report.OffenderCount(), report.GapCount())
	})
	clients := strings.FieldsFunc(os.Getenv("ETHER_PERF_GATE_CLIENTS"), func(r rune) bool { return r == ',' })
	for i := range clients {
		clients[i] = strings.TrimSpace(clients[i])
	}
	var music perfMusicFixture
	if len(clients) == 0 || slices.Contains(clients, "subsonic") {
		dir := os.Getenv("ETHER_PERF_GATE_MUSIC_SOURCE")
		if dir == "" {
			t.Fatal("Subsonic matrix requires ETHER_PERF_GATE_MUSIC_SOURCE to point to the hosted Music checkout")
		}
		var err error
		music, err = readPerfMusicFixture(dir)
		if err != nil {
			t.Fatal(err)
		}
	}
	cases := perfMatrixCases
	if only := os.Getenv("ETHER_PERF_GATE_MATRIX_CASES"); only != "" {
		names := strings.Split(only, ",")
		cases = slices.DeleteFunc(slices.Clone(cases), func(c perfMatrixCase) bool {
			return !slices.Contains(names, c.name)
		})
		if len(cases) == 0 {
			t.Fatalf("no matrix configurations match %q", only)
		}
	}
	keep := os.Getenv("ETHER_PERF_GATE_KEEP")
	if keep != "" {
		if err := os.MkdirAll(keep, 0o700); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(cases))
		for _, c := range cases {
			names = append(names, c.name)
			// The observer appends. Clear all selected traces before replay
			// so a failed local rerun cannot publish a previous case's data.
			if err := os.WriteFile(filepath.Join(keep, "matrix-"+c.name+"-trace.jsonl"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(keep, "matrix-cases.txt"), []byte(strings.Join(names, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := perfMatrixFixture(t, ctx, cache)
	for _, c := range cases {
		if ctx.Err() != nil {
			t.Error(ctx.Err())
			break
		}
		t.Run(c.name, func(t *testing.T) {
			caseCtx, stopCase := context.WithTimeout(ctx, 3*time.Minute)
			defer stopCase()
			runPerfMatrixCase(t, caseCtx, fixture, c, music, clients, keep, &report, writeReport)
		})
	}
	if report.OffenderCount()+report.GapCount() != 0 {
		t.Fail()
	}
}

func perfMatrixFixture(t *testing.T, ctx context.Context, cache string) string {
	t.Helper()
	path := filepath.Join(cache, "matrix-v3.db")
	stamp := filepath.Join(cache, "matrix-v3.date")
	if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) == time.Now().UTC().Format(time.DateOnly) {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	scratch, err := os.MkdirTemp(cache, "build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(scratch)
	db, err := store.Open(ctx, scratch)
	if err != nil {
		t.Fatal(err)
	}
	if err := perfharness.Generate(ctx, db, perfMatrixScale, time.Now()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Keep one first-time viewer distinct from the heavy and restricted viewers.
	for _, query := range perfMatrixNewViewerQueries {
		if _, err := db.ExecContext(ctx, query); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	// Publish the normal serving indexes before cloning the fixture.
	cfg, err := config.Open(scratch)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	cat, err := catalog.New(db, nil, cfg.Get().Catalog, nil, catalog.WithStartupContext(ctx))
	if err == nil {
		err = cat.SeedXtreamIDs(ctx)
	}
	if err == nil {
		_, err = cat.RebuildRank(ctx)
	}
	if err == nil {
		err = cat.ReplacePopularityIndexes(ctx)
	}
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(scratch, "ether.db"), path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamp, []byte(time.Now().UTC().Format(time.DateOnly)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var perfMatrixNewViewerQueries = []string{
	"DELETE FROM state_watch WHERE user_id=(SELECT id FROM auth_users WHERE username='viewer01')",
}

func runPerfMatrixCase(t *testing.T, ctx context.Context, fixture string, c perfMatrixCase, music perfMusicFixture, clients []string, keep string, report *perfharness.MatrixReport, writeReport func()) {
	start := time.Now()
	defer func() { t.Logf("replayed in %s", time.Since(start).Round(time.Second)) }()
	dir, err := os.MkdirTemp(filepath.Dir(fixture), "case-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := cloneFile(fixture, filepath.Join(dir, "ether.db")); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var consoleFixtureURL string
	var consoleDefinition string
	if len(clients) == 0 || slices.Contains(clients, "console") {
		definition, err := os.ReadFile("../cardigann/testdata/public.yml")
		if err != nil {
			t.Fatal(err)
		}
		draft, err := json.Marshal(map[string]string{"yaml": string(definition)})
		if err != nil {
			t.Fatal(err)
		}
		consoleDefinition = string(draft)
		definitions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/pms/") && r.Header.Get("X-Plex-Token") != "matrix-fixture-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/definitions/":
				_, _ = io.WriteString(w, `["fakepublic.yml"]`)
			case "/definitions/fakepublic.yml":
				_, _ = w.Write(definition)
			case "/tracker/browse.php":
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, `<table id="torrents"><tbody><tr><td><a href="browse.php?cat=1">Movies</a>
					<a class="title" href="details.php?id=1">Matrix Fixture 2026 1080p</a><a href="download.php?id=1">Download</a></td>
					<td class="size">700 MB</td><td class="seeders">7</td><td class="leechers">1</td></tr></tbody></table>`)
			case "/pms/identity":
				_, _ = io.WriteString(w, `{"MediaContainer":{"machineIdentifier":"matrix-scratch-pms","version":"1.41.0"}}`)
			case "/pms/myplex/account":
				_, _ = io.WriteString(w, `{"MyPlex":{"username":"Matrix PMS owner"}}`)
			case "/pms/accounts":
				_, _ = io.WriteString(w, `{"MediaContainer":{"size":1,"Account":[{"id":1,"name":"Matrix PMS owner"}]}}`)
			case "/pms/library/sections", "/pms/status/sessions/history/all":
				_, _ = io.WriteString(w, `{"MediaContainer":{"size":0,"totalSize":0}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer definitions.Close()
		consoleFixtureURL = definitions.URL
	}
	if err := cfg.Update(func(cfg *config.Config) error {
		cfg.IndexerSites.DefinitionsURL = ""
		if consoleFixtureURL != "" {
			cfg.IndexerSites.DefinitionsURL = consoleFixtureURL + "/definitions/"
		}
		cfg.SetupComplete = true
		cfg.Frontends = map[string]config.Frontend{"plex": {Enabled: true}, "jellyfin": {Enabled: true}, "emby": {Enabled: true}, "silo": {Enabled: true}, "vela": {Enabled: true}, "stremio": {Enabled: true}, "xtream": {Enabled: true}}
		cfg.Server.BindAddress = "127.0.0.1"
		cfg.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		if c.live {
			cfg.Plex.Token = "fixture-plex-owner"
			cfg.Plex.Claimed = true
		}
		cfg.Availability.Mode = config.ModeImmediate
		cfg.Availability.MovieMode, cfg.Availability.ShowMode = c.movieMode, c.showMode
		cfg.Availability.ApplyMode()
		cfg.Catalog.ShowUpcomingMovies, cfg.Catalog.ShowUpcomingEpisodes = c.upcomingMovies, c.upcomingEpisodes
		if c.sources {
			cfg.Debrid.Providers = []config.DebridProvider{{ID: "fixture", Kind: config.DebridTorBox, APIKey: "fixture", Enabled: true}}
			cfg.Usenet.Providers = []config.UsenetProvider{{ID: "fixture", Host: "127.0.0.1", Port: 1, Connections: 1, MinIdle: -1, Enabled: true}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedPerfMatrixCatalogue(ctx, db, c.live, len(clients) == 0 || slices.Contains(clients, "remote plugin")); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := seedPerfMatrixPlaylistCoverage(ctx, db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if len(clients) == 0 || slices.Contains(clients, "infuse-jellyfin") || slices.Contains(clients, "emby-sdk") {
		if _, err := db.ExecContext(ctx, `UPDATE auth_users SET thumb=?, plex_id=COALESCE(plex_id, 910100+id)
			WHERE active=1`, matrixAvatarURL); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	viewers, err := perfharness.MintCredentials(ctx, db, 4)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if consoleFixtureURL != "" {
		viewers[0].ConsoleFixtureURL = consoleFixtureURL
		viewers[0].ConsoleDefinition = consoleDefinition
	}
	if pg := os.Getenv(PerfGatePGEnv); pg != "" {
		defer perfGatePostgres(t, ctx, dir, strings.ReplaceAll(c.name, "-", "_"), pg)()
	}

	trace := filepath.Join(dir, "trace.jsonl")
	if keep != "" {
		trace = filepath.Join(keep, "matrix-"+c.name+"-trace.jsonl")
	}
	t.Setenv(PerfTraceEnv, trace)
	options := Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if c.live {
		options.plextvHTTP = &http.Client{Transport: perfMatrixPlexTV{}}
	}
	if c.sources {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
		}))
		defer provider.Close()
		options.debridProviderOptions = debrid.ProviderOptions{BaseURL: provider.URL, HTTPClient: provider.Client()}
	}
	// All stock matrix posters share this synthetic TMDB path. Seed its
	// cache, so authorised artwork requests exercise the route offline.
	var poster bytes.Buffer
	if err := jpeg.Encode(&poster, image.NewRGBA(image.Rect(0, 0, 10, 16)), nil); err != nil {
		t.Fatal(err)
	}
	options.urlImages = staticUntrustedImages{byURL: map[string]struct {
		data []byte
		ct   string
	}{matrixAlbumCoverURL: {poster.Bytes(), "image/jpeg"}}}
	a, err := New(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, key := range []string{"original/p.jpg", "w500/p.jpg", "w185/p.jpg", "original/b.jpg"} {
		if _, _, err := a.images.GetLoaded(ctx, key, func(context.Context) ([]byte, string, error) {
			return poster.Bytes(), "image/jpeg", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "infuse-jellyfin") || slices.Contains(clients, "emby-sdk") {
		hash := sha256.Sum256([]byte("plex-avatar-v1\x00" + matrixAvatarURL))
		if _, _, err := a.images.GetLoaded(ctx, fmt.Sprintf("plex-avatar/%x", hash), func(context.Context) ([]byte, string, error) {
			return poster.Bytes(), "image/jpeg", nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "console") {
		limiter, err := identity.NewLoginLimiter(a.db, 15*time.Minute, 30, 8, nil)
		if err != nil {
			t.Fatal(err)
		}
		lockoutIP := netip.MustParseAddr("192.0.2.1")
		for _, viewer := range []string{"matrix-locked-a", "matrix-locked-b"} {
			for range 8 {
				if _, err := limiter.Allow(ctx, viewer, lockoutIP); err != nil {
					t.Fatal("console lockout fixture:", err)
				}
			}
		}
		a.support.Observe(diagnostics.Event{Kind: diagnostics.KindDevice, Protocol: "jellyfin",
			DeviceKey: "matrix-console-playback", DeviceName: "Matrix living room", Product: "Matrix player"})
		templates, err := a.onair.Templates(ctx)
		if err != nil || len(templates) == 0 {
			t.Fatalf("On-Air templates fixture: %v", err)
		}
		look := templates[0].Style
		look.Name = "Matrix fixture look"
		if _, err := a.onair.SaveLook(ctx, look); err != nil {
			t.Fatal("On-Air look fixture:", err)
		}
		if _, err := a.onair.UploadAsset(ctx, "matrix-artwork.jpg", "image", "", bytes.NewReader(poster.Bytes())); err != nil {
			t.Fatal("On-Air artwork fixture:", err)
		}
		for _, id := range []string{"matrix-cache-release", "matrix-cache-block", "matrix-cache-clear-all"} {
			if err := a.cache.Put(id, 0, []byte("matrix cache fixture")); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now().UTC()
		play := &core.PlaySession{
			ID: "matrix-recorded-play", UserID: viewers[0].ID, Username: viewers[0].Username,
			Key: core.MovieKey(106), TitleName: "Fixture movie", ClientID: "matrix-console-playback",
			StartedAt: now.Add(-10 * time.Minute), BytesServed: 1 << 20,
		}
		if err := a.stats.Record(ctx, play, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	a.catalog.FreezeCacheMinute()
	viewers[0].PluginHost, err = perfMatrixPluginCredential(ctx, a, viewers[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(music.methods) != 0 {
		if err := music.install(ctx, a, &viewers[c.viewer]); err != nil {
			t.Fatal("Subsonic Music fixture:", err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "vela") {
		if err := seedPerfMatrixBranding(ctx, a); err != nil {
			t.Fatal("Vela branding fixture:", err)
		}
		if err := seedPerfMatrixSkins(ctx, a, poster.Bytes()); err != nil {
			t.Fatal("Vela skin fixture:", err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "vela") || slices.Contains(clients, "console") {
		if err := installPerfMatrixVelaCatalogue(ctx, a, viewers[c.viewer], poster.Bytes()); err != nil {
			t.Fatal("Vela catalogue fixture:", err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "infuse-jellyfin") || slices.Contains(clients, "emby-sdk") ||
		slices.Contains(clients, "moonfin") || slices.Contains(clients, "swiftfin") || slices.Contains(clients, "infuse-emby") {
		if err := installPerfMatrixMediaBrowserMusic(ctx, a, viewers[c.viewer]); err != nil {
			t.Fatal("MediaBrowser music fixture:", err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "infuse-jellyfin") {
		grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil, a.cfg)
		if err != nil {
			t.Fatal("Jellyfin web code fixture:", err)
		}
		viewers[c.viewer].MBWebCode, err = grants.IssueWebCode(ctx, viewers[c.viewer].ID)
		if err != nil {
			t.Fatal("Jellyfin web code fixture:", err)
		}
	}
	if len(clients) == 0 || slices.Contains(clients, "console") {
		if err := seedPerfMatrixConsoleQuickConnect(ctx, a, &viewers[0]); err != nil {
			t.Fatal("Console Quick Connect fixture:", err)
		}
		if err := seedPerfMatrixConsoleAPIKey(ctx, a); err != nil {
			t.Fatal("Console API key fixture:", err)
		}
		if err := seedPerfMatrixConsoleNotInterested(ctx, a); err != nil {
			t.Fatal("Console not-interested fixture:", err)
		}
	}
	if c.live {
		if len(clients) == 0 || slices.Contains(clients, "plex-appletv") {
			if err := a.livetv.SaveOwnerGroups(ctx, []livetv.OwnerGroup{{
				ID: "matrix-sports", Name: "Matrix sports",
				Groups: []livetv.OwnerGroupSource{{SourceID: "matrix-guide", Name: "Sports"}},
			}}); err != nil {
				t.Fatal("live guide group fixture:", err)
			}
		}
		if err := a.registrar.RefreshOwner(ctx); err != nil {
			report.AddGap(c.name, "plex", "", "", "Plex Pass fixture unavailable")
			t.Error("Plex Pass fixture:", err)
		}
		if err := installPerfMatrixSports(ctx, a); err != nil {
			report.AddGap(c.name, "harness", "", "", "sports overlay fixture unavailable")
			t.Error("sports overlay fixture:", err)
		}
	}
	if c.rows {
		if err := seedPerfMatrixRows(ctx, a); err != nil {
			report.AddGap(c.name, "harness", "", "", "recommendation rows could not be seeded")
			t.Error("recommendations fixture:", err)
		}
	}
	// The curated recommendations fixture already supplies collection-card rows.
	if !c.rows && (len(clients) == 0 || slices.Contains(clients, "silo-appletv") || slices.Contains(clients, "vela")) {
		if err := seedPerfMatrixCollectionRow(ctx, a); err != nil {
			t.Fatal("collection card row fixture:", err)
		}
	}
	if c.visibility != "" {
		show := core.LibraryExclusion{Action: "show", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "in", Value: json.RawMessage(`["Action","Comedy","Drama"]`)}}}
		rules := []core.LibraryExclusion{show}
		if c.visibility == "show" {
			rules = append(rules, core.LibraryExclusion{Action: "hide", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{{Field: "genre", Op: "in", Value: json.RawMessage(`["Horror"]`)}}})
		}
		for _, lib := range []core.LibraryID{core.LibraryMovies, core.LibraryShows} {
			if err := a.libraries.SetRules(ctx, lib, rules, c.visibility); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.refreshLibraryPolicies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if c.viewer == 0 {
		u, err := a.users.Owner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.personalize.Rebuild(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	a.frontends.start(ctx, server.Listener.Addr())
	checkpointCtx, stopCheckpoint := context.WithCancel(ctx)
	checkpointDone := make(chan struct{})
	go func() {
		defer close(checkpointDone)
		a.checkpointWAL(checkpointCtx)
	}()
	defer func() {
		stopCheckpoint()
		<-checkpointDone
	}()
	if len(music.methods) != 0 && a.frontends.control.listeners.sharedHandler("/subsonic/rest/ping.view") == nil {
		t.Fatal("Subsonic Music adapter shared listener unavailable")
	}
	var settleMisses atomic.Int64
	a.perf.mu.Lock()
	a.perf.asyncTrace = true
	a.perf.settleTrace = func(context.Context) {
		if !a.quiesce(ctx, 20*time.Second) {
			settleMisses.Add(1)
		}
	}
	a.perf.mu.Unlock()
	r := perfharness.NewRunner(server.Listener.Addr().(*net.TCPAddr).Port, true, viewers)
	defer r.Close()
	// A request still running at this point has already missed its replay
	// window. Its completed trace remains useful even when the client cancels.
	r.SetTimeout(8 * time.Second)
	r.SetBefore(func(context.Context) { a.perf.waitTraces() })
	availabilityMonitor := cfg.Get().Availability.Monitor
	cs := append(perfharness.MatrixClients(c.live, availabilityMonitor), perfharness.MatrixPluginHost())
	if len(music.methods) != 0 {
		cs = append(cs, perfharness.MatrixSubsonic(music.methods))
	}
	selectedClients := perfharness.Only(cs, clients)
	if len(selectedClients) == 0 {
		t.Fatalf("no matrix clients match %q", clients)
	}
	surfaces := map[string]bool{}
	for _, client := range selectedClients {
		surfaces[client.Surface] = true
	}
	inventory := perfMatrixInventory(t, a, surfaces, music.methods)
	r.SetMatrixRoutes(inventory)
	exceptions := matrixCaseRouteExceptions(inventory, c.live, availabilityMonitor)
	perfharness.AppendMatrixReads(cs, inventory, exceptions)
	cs = perfharness.Only(cs, clients)
	if steps := os.Getenv("ETHER_PERF_GATE_STEPS"); steps != "" {
		cs = perfharness.OnlySteps(cs, strings.Split(steps, ","))
	}
	var selected, ownerOnly []perfharness.Client
	for _, client := range cs {
		if client.Surface == "console" || client.Surface == "mcp" || client.Surface == "plugin-host" {
			ownerOnly = append(ownerOnly, client)
			continue
		}
		viewerClient, ownerClient := client, client
		viewerClient.Steps, ownerClient.Steps = nil, nil
		for _, step := range client.Steps {
			if step.OwnerOnly {
				ownerClient.Steps = append(ownerClient.Steps, step)
			} else {
				viewerClient.Steps = append(viewerClient.Steps, step)
			}
		}
		if len(viewerClient.Steps) != 0 {
			selected = append(selected, viewerClient)
		}
		if len(ownerClient.Steps) != 0 {
			ownerOnly = append(ownerOnly, ownerClient)
		}
	}
	r.Sequential(ctx, c.name, viewers[c.viewer:c.viewer+1], selected)
	r.Sequential(ctx, c.name, viewers[:1], ownerOnly)
	if ctx.Err() != nil {
		report.AddGap(c.name, "harness", "", "", "case deadline reached before completing replay")
	}
	server.Close()
	a.perf.waitTraces()
	if misses := settleMisses.Load(); misses != 0 {
		report.AddGap(c.name, "harness", "", "", fmt.Sprintf("background work was still active after %d responses", misses))
	}
	byTag, background, err := perfharness.ReadTrace(trace)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ETHER_PERF_GATE_STEPS") != "" {
		for _, result := range r.Results() {
			if record, ok := byTag[result.Tag]; ok {
				t.Logf("%s / %s: statements=%d rows=%d writes=%d HTTP=%d error=%q",
					result.Client, result.Step, record.Queries, record.Rows, record.Writes, result.Status, result.Err)
			}
		}
	}
	report.AddBackground(c.name, background)
	report.Add(c.name, r.Results(), byTag, cs)
	report.CheckRoutes(c.name, inventory, r.Results(), byTag, exceptions)
	t.Logf("recorded %d attempts", len(r.Results()))
	writeReport()
}

type perfMatrixPlexTV struct{}

func (perfMatrixPlexTV) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := http.StatusOK, `[]`
	switch r.URL.Path {
	case "/api/v2/user":
		body = `{"id":9101,"uuid":"matrix-owner","username":"owner","subscription":{"active":true}}`
	case "/api/v2/features":
	default:
		status, body = http.StatusNotFound, `{}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}
