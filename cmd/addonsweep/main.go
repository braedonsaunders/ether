// Command addonsweep is a developer tool, not shipped in installers. It
// drives Ether's REAL plugin code against live Stremio add-ons and prints a
// matrix of what works and what does not, so "every kind of add-on works"
// can be checked end to end instead of argued about.
//
// It builds a plugins.Manager the way internal/app does (temp-dir SQLite
// store, the real remote.Factory with a real safefetch guard/egress client,
// no tunnel) and for each add-on installs it through Manager.InstallAddon --
// the same call the console's "Add add-on" makes -- then exercises every
// resource the manifest declares through Ether's own code paths:
// stremio.Addon, stremio.Convert and the Live TV add-on import.
//
//	go run ./cmd/addonsweep [-only name] [-json] [-timeout 60s] [-keep]
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plugins"
	"github.com/braedonsaunders/ether/internal/plugins/adapters"
	"github.com/braedonsaunders/ether/internal/plugins/remote"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/stremio"
)

func main() {
	only := flag.String("only", "", "run only add-ons whose name contains this (case-insensitive)")
	asJSON := flag.Bool("json", false, "print the matrix as JSON")
	timeout := flag.Duration("timeout", 90*time.Second, "per-add-on budget")
	keep := flag.Bool("keep", false, "keep the temp store directory")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx := context.Background()
	dir, err := os.MkdirTemp("", "addonsweep-*")
	if err != nil {
		fatal(err)
	}
	if !*keep {
		defer os.RemoveAll(dir)
	} else {
		fmt.Fprintf(os.Stderr, "store: %s\n", dir)
	}

	h, err := newHarness(ctx, dir, log)
	if err != nil {
		fatal(err)
	}
	defer h.close()

	var rows []*addonResult
	for _, a := range addonList() {
		if *only != "" && !strings.Contains(strings.ToLower(a.Name), strings.ToLower(*only)) {
			continue
		}
		fmt.Fprintf(os.Stderr, "sweeping %s ...\n", a.Name)
		rows = append(rows, h.sweep(ctx, a, *timeout))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fatal(err)
		}
		return
	}
	printMatrix(os.Stdout, rows)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "addonsweep:", err)
	os.Exit(1)
}

// harness is a minimal but real plugin host: the same manager, remote
// runtime factory, guards and adapters the server wires in
// internal/app/plugins.go, minus the parts an add-on sweep never touches
// (tunnel, pairing, wasm, catalogue, cluster).
type harness struct {
	db  *sql.DB
	mgr *plugins.Manager
	tv  *livetv.Service
	ss  *adapters.StreamSources
}

// newHarness builds the store, manager, remote runtime, Live TV service and
// adapters the sweep drives.
func newHarness(ctx context.Context, dir string, log *slog.Logger) (*harness, error) {
	db, err := store.Open(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	tv, err := livetv.New(livetv.Options{DB: db, Logger: log.With("component", "livetv")})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("live tv: %w", err)
	}
	mgr, err := plugins.New(plugins.Deps{DB: db, Logger: log, IsLeader: func() bool { return true }})
	if err != nil {
		tv.Close()
		db.Close()
		return nil, fmt.Errorf("plugins: %w", err)
	}
	// The real remote.Factory: URL mode for http(s) links, raw-github mode
	// for repo links, with a real safefetch guard. No tunnel is needed for
	// add-ons Ether fetches itself.
	factory := &remote.Factory{
		TrustedLAN: remote.GrantsLAN(mgr.GrantsOf),
		Logger:     log,
	}
	mgr.RegisterRuntime("remote", factory.RuntimeFactory())
	// The Live TV add-on import ("Add its channels and guide to Live TV")
	// lives in adapters; Attach registers the channel source kind.
	adapters.Attach(mgr, nil, nil, tv)
	// The real installed-add-on indexer path a Plex/Jellyfin search uses.
	ss := adapters.NewStreamSources(nil)
	ss.Bind(mgr, nil)
	if err := mgr.Start(ctx); err != nil {
		tv.Close()
		db.Close()
		return nil, fmt.Errorf("start: %w", err)
	}
	return &harness{db: db, mgr: mgr, tv: tv, ss: ss}, nil
}

func (h *harness) close() {
	if h.tv != nil {
		_ = h.tv.Close()
	}
	if h.db != nil {
		_ = h.db.Close()
	}
}

// addonSpec is one add-on to sweep.
type addonSpec struct {
	Name string
	// Link is exactly the link an owner pastes.
	Link string
	// ExpectConfig marks an add-on whose unconfigured link must be
	// recognised as needing configuration (configurationRequired).
	ExpectConfig bool
	// Note is free text carried into the matrix.
	Note string
	// Synthetic names a stream kind the sweep checks by feeding a synthetic
	// stream through stremio.Convert (no reachable public add-on yields it).
	Synthetic string
}

// addonList is the brief's list, links exactly as owners paste them, plus
// the extra shapes.
func addonList() []addonSpec {
	return []addonSpec{
		{Name: "Cinemeta", Link: "https://v3-cinemeta.strem.io/manifest.json", Note: "catalog/meta, search"},
		{Name: "OpenSubtitles v3", Link: "https://opensubtitles-v3.strem.io/manifest.json", Note: "subtitles"},
		{Name: "Torrentio", Link: "https://torrentio.strem.fun/manifest.json", Note: "infoHash streams"},
		{Name: "Torrentio configured", Link: "https://torrentio.strem.fun/sort=qualitysize|qualityfilter=480p,scr,cam/manifest.json", Note: "configured path"},
		{Name: "Torrentio stremio://", Link: "stremio://torrentio.strem.fun/manifest.json", Note: "stremio:// scheme"},
		{Name: "Anime Kitsu", Link: "https://anime-kitsu.strem.fun/manifest.json", Note: "kitsu: ids, search, skip"},
		{Name: "WatchHub", Link: "https://watchhub.strem.io/manifest.json", Note: "externalUrl streams"},
		{Name: "YouTube", Link: "https://v3-channels.strem.io/manifest.json", Note: "channel type, ytId streams"},
		{Name: "USA TV Next repo", Link: "https://github.com/yowmamasita/usa-tv-next", Note: "github repo link -> raw"},
		{Name: "USA TV Next raw", Link: "https://raw.githubusercontent.com/yowmamasita/usa-tv-next/main/manifest.json", Note: "static, tv"},
		{Name: "USA TV Next stremio://raw", Link: "stremio://raw.githubusercontent.com/yowmamasita/usa-tv-next/main/manifest.json", Note: "static, tv, stremio://"},
		{Name: "Torrentio /configure", Link: "https://torrentio.strem.fun/configure", Note: "configure link resolves to base"},
		{Name: "TMDB", Link: "https://tmdb.elfhosted.com/manifest.json", ExpectConfig: true, Note: "may need config"},
		{Name: "MediaFusion", Link: "https://mediafusion.elfhosted.com/manifest.json", ExpectConfig: true, Note: "configurationRequired, encrypted config"},
		{Name: "Comet", Link: "https://comet.elfhosted.com/manifest.json", ExpectConfig: true, Note: "configurationRequired"},
		// Extra shapes the brief asks to search for:
		// - a static-file add-on served from a plain LAN host (python
		//   http.server on loopback, the LAN trust path), carrying
		//   behaviorHints.proxyHeaders/notWebReady on its streams;
		// - a real *.github.io static add-on (found via gh search):
		//   pigamer37.github.io/stremio, which serves addon_catalog;
		// - synthetic ytId and nzbUrl streams fed through stremio.Convert,
		//   because no reachable public add-on returns them unconfigured.
		{Name: "LAN static (tv)", Link: "", Note: "static files over LAN HTTP; tv catalogs; proxyHeaders/notWebReady"},
		{Name: "GitHub Pages static", Link: "https://pigamer37.github.io/stremio/manifest.json", Note: "real *.github.io static add-on; addon_catalog"},
		{Name: "Synthetic ytId stream", Link: "", Synthetic: "ytId", Note: "stremio.Convert on a ytId stream"},
		{Name: "Synthetic nzbUrl stream", Link: "", Synthetic: "nzbUrl", Note: "stremio.Convert on an nzbUrl stream"},
	}
}

// resultCell is one matrix cell.
type resultCell struct {
	Status string `json:"status"` // PASS, FAIL, N/A, UNREACHABLE
	Detail string `json:"detail,omitempty"`
	MS     int64  `json:"ms,omitempty"`
}

// addonResult is one row of the matrix.
type addonResult struct {
	Name    string `json:"name"`
	Link    string `json:"link"`
	Note    string `json:"note,omitempty"`
	Install resultCell
	Catalog resultCell
	Search  resultCell
	Meta    resultCell
	Stream  resultCell
	Indexer resultCell
	Subs    resultCell
	LiveTV  resultCell
	AddCat  resultCell
	Config  resultCell
	// StreamKinds records what the add-on's stream route returned.
	StreamKinds map[string]int `json:"streamKinds,omitempty"`
	// Types and Resource summarise what the add-on declared.
	Types    []string `json:"types,omitempty"`
	Resource []string `json:"resources,omitempty"`
}

func pass(ms int64) resultCell    { return resultCell{Status: "PASS", MS: ms} }
func na(detail string) resultCell { return resultCell{Status: "N/A", Detail: detail} }
func fail(detail string, ms int64) resultCell {
	return resultCell{Status: "FAIL", Detail: detail, MS: ms}
}
func unreachable(detail string, ms int64) resultCell {
	return resultCell{Status: "UNREACHABLE", Detail: detail, MS: ms}
}

// sweep installs and exercises one add-on.
func (h *harness) sweep(ctx context.Context, spec addonSpec, budget time.Duration) *addonResult {
	res := &addonResult{Name: spec.Name, Link: spec.Link, Note: spec.Note}
	res.Catalog, res.Search, res.Meta, res.Stream, res.Subs = na("not declared"), na("no search extra"), na("not declared"), na("not declared"), na("not declared")
	res.Indexer = na("no stream resource")
	res.LiveTV, res.AddCat, res.Config = na("no tv catalogs"), na("not declared"), na("")

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	if spec.Synthetic != "" {
		h.sweepSynthetic(ctx, spec, res)
		return res
	}
	link := spec.Link
	var cleanup func()
	if spec.Link == "" && spec.Name == "LAN static (tv)" {
		link, cleanup = h.serveLANAddon(ctx, lanShapeTV)
		res.Link = link
	}
	if cleanup != nil {
		defer cleanup()
	}
	if link == "" {
		res.Install = fail("no link", 0)
		return res
	}

	start := time.Now()
	rec, err := h.mgr.InstallAddon(ctx, plugins.AddonInstall{Link: link})
	instMS := time.Since(start).Milliseconds()
	if err != nil {
		res.Install = classifyErr(err, instMS)
		return res
	}
	res.Install = pass(instMS)
	res.Types = rec.Manifest.Stremio().Types
	res.Resource = resourceNames(rec.Manifest.Stremio())

	addon := h.mgr.Addon(rec)
	man := addon.Manifest

	// Configuration: an add-on that merely declares `configurable` is fully
	// usable on its defaults, so every resource is still exercised; only a
	// `configurationRequired` link is skipped, and Ether must recognise it
	// (surface it, not crash and not claim Healthy).
	if man.BehaviorHints.Configurable || man.BehaviorHints.ConfigurationRequired {
		cfg := h.checkConfig(ctx, rec)
		res.Config = cfg
		if spec.ExpectConfig && cfg.Status != "PASS" {
			res.Install = fail("configuration not surfaced: "+cfg.Detail, instMS)
		}
	}
	if man.BehaviorHints.ConfigurationRequired {
		res.Catalog = na("configuration required")
		res.Search = na("configuration required")
		res.Meta = na("configuration required")
		res.Stream = na("configuration required")
		res.Subs = na("configuration required")
		res.LiveTV = na("configuration required")
		res.AddCat = na("configuration required")
		return res
	}

	res.Catalog = h.checkCatalog(ctx, addon)
	res.Search = h.checkSearch(ctx, addon)
	res.Meta = h.checkMeta(ctx, addon)
	res.Stream, res.StreamKinds = h.checkStream(ctx, rec, addon)
	res.Indexer = h.checkIndexer(ctx, rec)
	res.Subs = h.checkSubtitles(ctx, addon)
	res.LiveTV = h.checkLiveTV(ctx, rec)
	res.AddCat = h.checkAddonCatalog(ctx, addon)
	return res
}

// sweepSynthetic feeds a synthetic stream of one kind through Ether's real
// converter (stremio.Convert) and records whether it is accepted or clearly
// refused, never a crash. Used for stream kinds no reachable public add-on
// yields unconfigured (ytId, nzbUrl).
func (h *harness) sweepSynthetic(_ context.Context, spec addonSpec, res *addonResult) {
	res.Link = "(synthetic " + spec.Synthetic + " stream)"
	res.Install = resultCell{Status: "N/A", Detail: "synthetic"}
	res.Catalog = na("synthetic")
	res.Search = na("synthetic")
	res.Meta = na("synthetic")
	res.Subs = na("synthetic")
	res.LiveTV = na("synthetic")
	res.AddCat = na("synthetic")
	res.Config = na("synthetic")

	var raw string
	switch spec.Synthetic {
	case "ytId":
		raw = `{"name":"YouTube","title":"A video","ytId":"dQw4w9WgXcQ"}`
	case "nzbUrl":
		raw = `{"name":"Usenet","title":"A.Release.2024.1080p.WEB-DL\n\ud83d\udc64 10 \ud83d\udcbe 2.0 GB","nzbUrl":"https://example.test/release.nzb"}`
	default:
		res.Install = fail("unknown synthetic kind "+spec.Synthetic, 0)
		return
	}
	streams, kinds := classifyStreams([]byte(`{"streams":[` + raw + `]}`))
	res.StreamKinds = kinds
	req := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0111161"}, Title: "Probe", Year: 2000}
	look := stremio.Lookup{Type: stremio.TypeMovie, ID: "tt0111161"}

	start := time.Now()
	accepted, refused := func() (a int, r int) {
		defer func() {
			if rec := recover(); rec != nil {
				res.Stream = fail(fmt.Sprintf("stremio.Convert panicked on a %s stream: %v", spec.Synthetic, rec), time.Since(start).Milliseconds())
			}
		}()
		conv := stremio.Convert(streams, req, look, stremio.ConvertOptions{Instance: "synthetic"})
		return len(conv.Releases), conv.SkippedNoHash + conv.SkippedAmbiguous
	}()
	if res.Stream.Status == "FAIL" {
		return
	}
	ms := time.Since(start).Milliseconds()
	switch spec.Synthetic {
	case "ytId":
		// A ytId stream has nothing a server can stream: refused cleanly.
		if accepted == 0 && refused > 0 {
			res.Stream = resultCell{Status: "PASS", Detail: fmt.Sprintf("refused cleanly, no crash (kind=%s)", kindsString(kinds)), MS: ms}
		} else {
			res.Stream = fail(fmt.Sprintf("ytId accepted=%d refused=%d; expected a clean refusal", accepted, refused), ms)
		}
	case "nzbUrl":
		// An nzbUrl stream must become a Usenet release Ether can fetch.
		if accepted == 1 {
			proto := convProtocol(streams, req, look)
			if proto == "" {
				proto = "usenet (zero-value)"
			}
			res.Stream = resultCell{Status: "PASS", Detail: "accepted as Usenet release (protocol=" + proto + ")", MS: ms}
		} else {
			res.Stream = fail(fmt.Sprintf("nzbUrl accepted=%d refused=%d; expected one Usenet release", accepted, refused), ms)
		}
	}
}

// convProtocol returns the protocol of the first converted release, for the
// synthetic nzbUrl note.
func convProtocol(streams []stremio.Stream, req core.SearchRequest, look stremio.Lookup) string {
	conv := stremio.Convert(streams, req, look, stremio.ConvertOptions{Instance: "synthetic"})
	if len(conv.Releases) == 0 {
		return "none"
	}
	return string(conv.Releases[0].Protocol)
}

// classifyErr turns an install error into a matrix cell: an HTTP status the
// add-on itself answered is UNREACHABLE (not Ether's bug); anything else is
// a FAIL to investigate.
func classifyErr(err error, ms int64) resultCell {
	if err == nil {
		return pass(ms)
	}
	var he *stremio.HTTPError
	if errors.As(err, &he) {
		return unreachable(fmt.Sprintf("HTTP %d", he.Status), ms)
	}
	msg := err.Error()
	for _, marker := range []string{"HTTP 403", "HTTP 404", "HTTP 429", "HTTP 500", "HTTP 502", "HTTP 503", "status 403", "status 404", "status 429", "status 500", "status 502", "status 503"} {
		if strings.Contains(msg, marker) {
			return unreachable(msg, ms)
		}
	}
	if strings.Contains(msg, "fetch failed") || strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return unreachable(msg, ms)
	}
	return fail(msg, ms)
}

func resourceNames(m stremio.Manifest) []string {
	out := make([]string, 0, len(m.Resources))
	for _, r := range m.Resources {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// --- checks -------------------------------------------------------------

// metaItem is one catalog entry a later check can read meta/streams for.
type metaItem struct {
	typ  string
	id   string
	name string
}

// checkCatalog reads every catalog the manifest declares without required
// extras.
func (h *harness) checkCatalog(ctx context.Context, addon stremio.Addon) resultCell {
	cats := addon.Manifest.Catalogs
	if len(cats) == 0 {
		return na("no catalogs")
	}
	start := time.Now()
	var read int
	var firstErr error
	for _, c := range cats {
		if hasRequiredExtra(c) {
			continue
		}
		if _, err := addon.Catalog(ctx, c, nil, 50); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s/%s: %w", c.Type, c.ID, err)
			}
			continue
		}
		read++
	}
	ms := time.Since(start).Milliseconds()
	if read == 0 {
		if firstErr != nil {
			return classifyErr(firstErr, ms)
		}
		return na("every catalog needs extras")
	}
	return pass(ms)
}

// checkSearch runs a search-extra catalog with query "star" and a skip-
// declaring catalog's paging, through stremio.Addon.Catalog.
func (h *harness) checkSearch(ctx context.Context, addon stremio.Addon) resultCell {
	var withSearch, withSkip *stremio.Catalog
	for i := range addon.Manifest.Catalogs {
		c := addon.Manifest.Catalogs[i]
		if _, ok := c.ExtraNamed("search"); ok && withSearch == nil {
			withSearch = &c
		}
		if c.SupportsSkip() && withSkip == nil {
			withSkip = &c
		}
	}
	if withSearch == nil && withSkip == nil {
		return na("no search or skip extra")
	}
	start := time.Now()
	var errs []string
	if withSearch != nil {
		if _, err := addon.Catalog(ctx, *withSearch, map[string]string{"search": "star"}, 20); err != nil {
			errs = append(errs, "search: "+err.Error())
		}
	}
	if withSkip != nil {
		if _, err := addon.Catalog(ctx, *withSkip, nil, 120); err != nil {
			errs = append(errs, "skip: "+err.Error())
		}
	}
	ms := time.Since(start).Milliseconds()
	if len(errs) > 0 {
		return classifyErr(fmt.Errorf("%s", strings.Join(errs, "; ")), ms)
	}
	return pass(ms)
}

// checkMeta reads the meta of the first catalog item; for a series it
// verifies videos are present.
func (h *harness) checkMeta(ctx context.Context, addon stremio.Addon) resultCell {
	if !addon.Manifest.HasResource(stremio.ResourceMeta) {
		return na("no meta resource")
	}
	items := h.firstItems(ctx, addon, 4)
	if len(items) == 0 {
		return na("no catalog items to read meta for")
	}
	start := time.Now()
	var lastErr error
	for _, it := range items {
		meta, err := addon.Meta(ctx, it.typ, it.id)
		if err != nil {
			lastErr = err
			continue
		}
		if meta.ID == "" {
			lastErr = fmt.Errorf("meta %s/%s returned no id", it.typ, it.id)
			continue
		}
		ms := time.Since(start).Milliseconds()
		if it.typ == stremio.TypeSeries && len(meta.Videos) == 0 {
			return fail(fmt.Sprintf("series meta %s has no videos", it.id), ms)
		}
		return pass(ms)
	}
	return classifyErr(lastErr, time.Since(start).Milliseconds())
}

// checkStream asks the stream route Ether would ask for a movie, series and
// tv channel, only for types the add-on lists and ids its prefixes allow,
// classifies the stream kinds the add-on returned, and confirms Ether's own
// converter accepts them.
func (h *harness) checkStream(ctx context.Context, rec plugins.Record, addon stremio.Addon) (resultCell, map[string]int) {
	man := addon.Manifest
	if !man.HasResource(stremio.ResourceStream) {
		return na("no stream resource"), nil
	}
	type probe struct {
		typ, id string
	}
	var probes []probe
	if man.TakesID(stremio.ResourceStream, stremio.TypeMovie, "tt0111161") {
		probes = append(probes, probe{stremio.TypeMovie, "tt0111161"})
	}
	if man.TakesID(stremio.ResourceStream, stremio.TypeSeries, "tt0903747:1:1") {
		probes = append(probes, probe{stremio.TypeSeries, "tt0903747:1:1"})
	}
	for _, it := range h.firstItemsOfType(ctx, addon, stremio.TypeTV, 4) {
		if man.TakesID(stremio.ResourceStream, stremio.TypeTV, it.id) {
			probes = append(probes, probe{stremio.TypeTV, it.id})
			break
		}
	}
	if len(probes) == 0 {
		return na("add-on lists no probeable stream type"), nil
	}

	start := time.Now()
	kinds := map[string]int{}
	var accepted int
	var lastErr error
	for _, p := range probes {
		raw, err := h.rawRoute(ctx, rec.ID, "/stream/"+p.typ+"/"+url.PathEscape(p.id)+".json")
		if err != nil {
			lastErr = err
			continue
		}
		streams, kindCounts := classifyStreams(raw)
		for k, n := range kindCounts {
			kinds[k] += n
		}
		if len(streams) == 0 {
			continue
		}
		req := searchRequestFor(p.typ, p.id)
		look, ok := stremio.LookupStreams(ctx, man, req, nil, nil)
		if !ok {
			look = stremio.Lookup{Type: p.typ, ID: p.id}
		}
		conv := stremio.Convert(streams, req, look, stremio.ConvertOptions{Instance: rec.ID})
		accepted += len(conv.Releases)
	}
	ms := time.Since(start).Milliseconds()
	if lastErr != nil {
		return classifyErr(lastErr, ms), kinds
	}
	if len(kinds) == 0 {
		// The route answered but with no streams (an add-on with nothing to
		// offer for this id); the route itself works.
		return pass(ms), kinds
	}
	if accepted == 0 && onlyUnplayable(kinds) {
		// Every stream is an external link or a YouTube id. No media server
		// can play those; Ether correctly rejects them. Reaching this is a
		// PASS with the rejected kinds recorded, not an Ether failure.
		return resultCell{Status: "PASS", Detail: "only non-playable kinds (correctly rejected): " + kindsString(kinds), MS: ms}, kinds
	}
	return pass(ms), kinds
}

// checkIndexer drives the real installed-add-on indexer path a media-server
// search uses: ExpandAddonIndexers builds the row, StreamSources.Backend
// gives the newznab backend, and Search runs stremio.LookupStreams +
// stremio.Convert behind the search budget. This is the end-to-end proof
// for a stream add-on.
func (h *harness) checkIndexer(ctx context.Context, rec plugins.Record) resultCell {
	if !rec.Manifest.Stremio().HasResource(stremio.ResourceStream) {
		return na("no stream resource")
	}
	if !rec.Manifest.HasCapability(plugins.CapStreamSource) {
		return na("no stream_source role")
	}
	rows := adapters.ExpandAddonIndexers(ctx, h.mgr, nil)
	var row *config.Indexer
	for i := range rows {
		if rows[i].Plugin == rec.ID {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return fail("ExpandAddonIndexers produced no row for the add-on", 0)
	}
	backend, err := h.ss.Backend(*row)
	if err != nil {
		return fail("Backend: "+err.Error(), 0)
	}
	req := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0111161"}, Title: "The Shawshank Redemption", Year: 1994}
	start := time.Now()
	rels, err := backend.Search(ctx, req, func(context.Context, string) error { return nil })
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return classifyErr(err, ms)
	}
	// An add-on that serves no release for this probe still proves the path
	// (MediaFusion returns an empty list unconfigured); only an error is a
	// failure.
	return resultCell{Status: "PASS", Detail: fmt.Sprintf("%d releases", len(rels)), MS: ms}
}

// checkSubtitles lists subtitles for tt0111161, and for the add-on's own
// first catalog item id (an add-on that keys on kitsu:/tmdb: ids will not
// recognise an IMDb id), when the subtitles resource is declared.
func (h *harness) checkSubtitles(ctx context.Context, addon stremio.Addon) resultCell {
	var probes []metaItem
	if addon.Manifest.Serves(stremio.ResourceSubtitles, stremio.TypeMovie) {
		probes = append(probes, metaItem{typ: stremio.TypeMovie, id: "tt0111161"})
	}
	for _, it := range h.firstItems(ctx, addon, 4) {
		if addon.Manifest.Serves(stremio.ResourceSubtitles, it.typ) {
			probes = append(probes, it)
		}
	}
	if len(probes) == 0 {
		return na("no subtitles resource for the declared types")
	}
	start := time.Now()
	var lastErr error
	for _, p := range probes {
		if _, err := addon.Subtitles(ctx, p.typ, p.id, nil); err != nil {
			lastErr = err
			continue
		}
		return pass(time.Since(start).Milliseconds())
	}
	return classifyErr(lastErr, time.Since(start).Milliseconds())
}

// checkLiveTV runs the Live TV add-on import ("Add its channels and guide
// to Live TV") and reports channel/stream counts.
func (h *harness) checkLiveTV(ctx context.Context, rec plugins.Record) resultCell {
	if len(rec.Manifest.Stremio().CatalogsOfType(stremio.TypeTV)) == 0 {
		return na("no tv catalogs")
	}
	if h.tv == nil {
		return na("live tv unavailable")
	}
	if !rec.Manifest.HasCapability(plugins.CapLiveTV) {
		return fail("tv add-on has no Live TV role after install", 0)
	}
	kindID := extend.PluginID(rec.ID, plugins.StremioChannelsKey)
	if _, ok := h.tv.KindRegistry().Lookup(kindID); !ok {
		return fail("live tv source kind not registered", 0)
	}
	start := time.Now()
	src, err := h.tv.ImportSource(ctx, livetv.Source{Name: rec.Name, Kind: kindID})
	if err != nil {
		return classifyErr(err, time.Since(start).Milliseconds())
	}
	channels, err := h.tv.SourceChannels(ctx, src.ID)
	if err != nil {
		return classifyErr(err, time.Since(start).Milliseconds())
	}
	ms := time.Since(start).Milliseconds()
	if len(channels) == 0 {
		return fail("import produced no channels", ms)
	}
	// behaviorHints.proxyHeaders.request must survive the import as input
	// headers: a channel with a Referer/User-Agent must keep them.
	var sawHeader, expectedHeader bool
	for _, c := range channels {
		for _, in := range c.StreamSources {
			if strings.EqualFold(in.URL, "https://example.test/lan1.m3u8") {
				expectedHeader = true
				if strings.EqualFold(in.Headers["Referer"], "https://example.test/") && in.Headers["User-Agent"] != "" {
					sawHeader = true
				}
			}
		}
	}
	if expectedHeader && !sawHeader {
		return fail("Live TV import dropped behaviorHints.proxyHeaders", ms)
	}
	detail := fmt.Sprintf("%d channels", len(channels))
	if sawHeader {
		detail += ", proxyHeaders kept"
	}
	return resultCell{Status: "PASS", Detail: detail, MS: ms}
}

// checkAddonCatalog lists the addon_catalog resource when declared.
func (h *harness) checkAddonCatalog(ctx context.Context, addon stremio.Addon) resultCell {
	if !addon.Manifest.HasResource(stremio.ResourceAddonCatalog) {
		return na("not declared")
	}
	cats := addon.Manifest.AddonCatalogs
	if len(cats) == 0 {
		return na("addon_catalog resource with no catalogs")
	}
	start := time.Now()
	var lastErr error
	for _, c := range cats {
		if _, _, err := addon.CatalogPage(ctx, c.Type, c.ID, nil); err != nil {
			lastErr = err
			continue
		}
		return pass(time.Since(start).Milliseconds())
	}
	return classifyErr(lastErr, time.Since(start).Milliseconds())
}

// checkConfig asks the manager how it presents this add-on's configuration.
// A configurationRequired add-on is a PASS when Ether says so (Required set)
// rather than crashing or reporting Healthy.
func (h *harness) checkConfig(ctx context.Context, rec plugins.Record) resultCell {
	cfg, err := h.mgr.AddonConfig(ctx, rec.ID)
	if err != nil {
		return fail("AddonConfig: "+err.Error(), 0)
	}
	if !cfg.Configurable {
		return fail("configuration required but AddonConfig says not configurable", 0)
	}
	detail := "configurable"
	if cfg.Required {
		detail = "configuration required surfaced"
	}
	return resultCell{Status: "PASS", Detail: detail}
}

// firstItems reads the first few catalog items across declared catalogs.
func (h *harness) firstItems(ctx context.Context, addon stremio.Addon, limit int) []metaItem {
	var out []metaItem
	for _, c := range addon.Manifest.Catalogs {
		if hasRequiredExtra(c) {
			continue
		}
		items, _, err := addon.CatalogPage(ctx, c.Type, c.ID, nil)
		if err != nil {
			continue
		}
		for _, m := range items {
			if m.ID == "" {
				continue
			}
			out = append(out, metaItem{typ: firstNonEmpty(m.Type, c.Type), id: m.ID, name: m.Name})
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (h *harness) firstItemsOfType(ctx context.Context, addon stremio.Addon, typ string, limit int) []metaItem {
	var out []metaItem
	for _, c := range addon.Manifest.CatalogsOfType(typ) {
		if hasRequiredExtra(c) {
			continue
		}
		items, _, err := addon.CatalogPage(ctx, c.Type, c.ID, nil)
		if err != nil {
			continue
		}
		for _, m := range items {
			if m.ID == "" {
				continue
			}
			out = append(out, metaItem{typ: typ, id: m.ID, name: m.Name})
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// rawRoute reads one route through the same guarded, metered manager call
// the add-on fetcher uses.
func (h *harness) rawRoute(ctx context.Context, pluginID, route string) ([]byte, error) {
	resp, err := h.mgr.Call(ctx, pluginID, plugins.Request{Method: http.MethodGet, Path: route})
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return nil, &stremio.HTTPError{Status: resp.Status}
	}
	return resp.Body, nil
}

// classifyStreams decodes a stream route answer into Ether's own stremio
// stream values (for conversion) and counts the stream kinds by the keys
// the add-on used: url, infoHash, ytId, externalUrl, nzbUrl, other.
func classifyStreams(body []byte) ([]stremio.Stream, map[string]int) {
	var env struct {
		Streams []json.RawMessage `json:"streams"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, nil
	}
	var streams []stremio.Stream
	kinds := map[string]int{}
	for _, raw := range env.Streams {
		var keys struct {
			URL         string `json:"url"`
			InfoHash    string `json:"infoHash"`
			NZBURL      string `json:"nzbUrl"`
			YTID        string `json:"ytId"`
			ExternalURL string `json:"externalUrl"`
		}
		_ = json.Unmarshal(raw, &keys)
		switch {
		case strings.TrimSpace(keys.InfoHash) != "":
			kinds["infoHash"]++
		case strings.TrimSpace(keys.NZBURL) != "":
			kinds["nzbUrl"]++
		case strings.TrimSpace(keys.URL) != "":
			kinds["url"]++
		case strings.TrimSpace(keys.YTID) != "":
			kinds["ytId"]++
		case strings.TrimSpace(keys.ExternalURL) != "":
			kinds["externalUrl"]++
		default:
			kinds["other"]++
		}
		var s stremio.Stream
		if json.Unmarshal(raw, &s) == nil {
			streams = append(streams, s)
		}
	}
	return streams, kinds
}

func onlyUnplayable(kinds map[string]int) bool {
	for k, n := range kinds {
		if n == 0 {
			continue
		}
		switch k {
		case "url", "infoHash", "nzbUrl":
			return false
		}
	}
	return true
}

func kindsString(kinds map[string]int) string {
	parts := make([]string, 0, len(kinds))
	for k, n := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func hasRequiredExtra(c stremio.Catalog) bool {
	for _, e := range c.Extras() {
		if e.IsRequired && e.Name != "skip" {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// searchRequestFor builds the core.SearchRequest Ether would use to ask
// this add-on about the probe id.
func searchRequestFor(typ, id string) core.SearchRequest {
	req := core.SearchRequest{}
	switch typ {
	case stremio.TypeMovie:
		req.Type = core.TypeMovie
		req.IDs.IMDB = stremio.IMDbID(id)
		if req.IDs.IMDB == "" {
			req.IDs.TMDB = tmdbFromID(id)
		}
	case stremio.TypeSeries:
		req.Type = core.TypeEpisode
		base, season, episode := splitSeriesID(id)
		req.IDs.IMDB = stremio.IMDbID(base)
		req.Season, req.Episode = season, episode
	default:
		req.Type = core.TypeMovie
		req.IDs.IMDB = stremio.IMDbID(id)
	}
	req.Title = "Probe"
	req.Year = 2000
	return req
}

func tmdbFromID(id string) int64 {
	var n int64
	fmt.Sscanf(strings.TrimPrefix(id, "tmdb:"), "%d", &n)
	return n
}

func splitSeriesID(id string) (string, int, int) {
	parts := strings.Split(id, ":")
	if len(parts) < 3 {
		return id, 1, 1
	}
	var season, episode int
	fmt.Sscanf(parts[len(parts)-2], "%d", &season)
	fmt.Sscanf(parts[len(parts)-1], "%d", &episode)
	return strings.Join(parts[:len(parts)-2], ":"), season, episode
}

// --- LAN static add-on --------------------------------------------------

// serveLANAddon serves a small static add-on of the same shape as USA TV
// Next from a temp dir with python3 -m http.server on loopback, and returns
// the LAN install link plus a cleanup function. This exercises the LAN
// trust path.
// lanShape selects which static add-on the LAN server hosts.
type lanShape int

const (
	// lanShapeTV is a `tv` add-on: catalogs and streams, the USA TV Next
	// shape.
	lanShapeTV lanShape = iota
	// lanShapeAddonCatalog declares the addon_catalog resource, the
	// v3-cinemeta shape.
	lanShapeAddonCatalog
)

// serveLANAddon serves a static add-on from a temp dir with
// `python3 -m http.server` on loopback and returns its install link plus a
// cleanup function. This is the LAN trust path: a private host the owner
// pasted, which the remote factory trusts.
func (h *harness) serveLANAddon(_ context.Context, shape lanShape) (string, func()) {
	dir, err := os.MkdirTemp("", "addonsweep-lan-*")
	if err != nil {
		return "", nil
	}
	var files map[string]string
	switch shape {
	case lanShapeAddonCatalog:
		files = map[string]string{
			"manifest.json":              `{"id":"org.addonsweep.lancatalog","version":"1.0.0","name":"LAN Catalog","description":"LAN static add-on with addon_catalog","resources":["catalog","meta","addon_catalog"],"types":["movie","series"],"catalogs":[{"type":"movie","id":"top","name":"Top"}],"addonCatalogs":[{"type":"all","id":"installed","name":"Installed"}]}`,
			"catalog/movie/top.json":     `{"metas":[{"id":"tt0111161","type":"movie","name":"The Shawshank Redemption"}]}`,
			"meta/movie/tt0111161.json":  `{"meta":{"id":"tt0111161","type":"movie","name":"The Shawshank Redemption","description":"LAN"}}`,
			"catalog/all/installed.json": `{"metas":[{"id":"org.addonsweep.lantv","type":"other","name":"LAN TV"}]}`,
		}
	default:
		files = map[string]string{
			"manifest.json":        `{"id":"org.addonsweep.lantv","version":"1.0.0","name":"LAN TV","description":"LAN static add-on","resources":["catalog","stream"],"types":["tv"],"catalogs":[{"type":"tv","id":"live","name":"LAN"}]}`,
			"catalog/tv/live.json": `{"metas":[{"id":"lan1","name":"LAN One"},{"id":"lan2","name":"LAN Two"}]}`,
			"stream/tv/lan1.json":  `{"streams":[{"name":"LAN","url":"https://example.test/lan1.m3u8","behaviorHints":{"notWebReady":true,"proxyHeaders":{"request":{"Referer":"https://example.test/","User-Agent":"LANAgent/1.0"}}}}]}`,
			"stream/tv/lan2.json":  `{"streams":[{"name":"LAN","url":"https://example.test/lan2.m3u8"}]}`,
		}
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			os.RemoveAll(dir)
			return "", nil
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			os.RemoveAll(dir)
			return "", nil
		}
	}
	port, cmd, err := startPythonHTTPServer(dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/manifest.json", port)
	return link, func() {
		stopPythonHTTPServer(cmd)
		os.RemoveAll(dir)
	}
}

// startPythonHTTPServer starts `python3 -m http.server <port>` bound to
// loopback in dir, as the brief asks (the LAN trust path), and returns the
// chosen port and the command handle.
func startPythonHTTPServer(dir string) (int, *exec.Cmd, error) {
	port, err := freeLoopbackPort()
	if err != nil {
		return 0, nil, err
	}
	cmd := exec.Command("python3", "-m", "http.server", strconv.Itoa(port), "--bind", "127.0.0.1", "--directory", dir)
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	if !waitForPort(port, 3*time.Second) {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("python http.server did not start on %d", port)
	}
	return port, cmd, nil
}

func stopPythonHTTPServer(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForPort(port int, timeout time.Duration) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// --- matrix printer -----------------------------------------------------

// printMatrix writes the add-on x check matrix plus detail lines and a
// summary footer.
func printMatrix(w io.Writer, rows []*addonResult) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join([]string{
		"ADD-ON", "INSTALL", "CATALOG", "SEARCH", "META", "STREAM", "INDEXER", "SUBS", "TV", "ADDON_CAT", "CONFIG",
	}, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join([]string{
			r.Name,
			cellText(r.Install),
			cellText(r.Catalog),
			cellText(r.Search),
			cellText(r.Meta),
			cellText(r.Stream),
			cellText(r.Indexer),
			cellText(r.Subs),
			cellText(r.LiveTV),
			cellText(r.AddCat),
			cellText(r.Config),
		}, "\t"))
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	var passN, failN, unreachN int
	for _, r := range rows {
		for _, c := range []resultCell{r.Install, r.Catalog, r.Search, r.Meta, r.Stream, r.Indexer, r.Subs, r.LiveTV, r.AddCat, r.Config} {
			switch c.Status {
			case "PASS":
				passN++
			case "FAIL":
				failN++
			case "UNREACHABLE":
				unreachN++
			}
		}
	}
	fmt.Fprintf(w, "cells: PASS=%d FAIL=%d UNREACHABLE=%d (N/A omitted)\n", passN, failN, unreachN)

	for _, r := range rows {
		var notes []string
		add := func(label string, c resultCell) {
			if c.Status == "FAIL" || c.Status == "UNREACHABLE" {
				notes = append(notes, fmt.Sprintf("%s: %s %s", label, c.Status, c.Detail))
			}
		}
		add("install", r.Install)
		add("catalog", r.Catalog)
		add("search", r.Search)
		add("meta", r.Meta)
		add("stream", r.Stream)
		add("indexer", r.Indexer)
		add("subs", r.Subs)
		add("tv", r.LiveTV)
		add("addon_cat", r.AddCat)
		add("config", r.Config)
		if len(notes) > 0 {
			fmt.Fprintf(w, "\n%s (%s)\n", r.Name, r.Link)
			for _, n := range notes {
				fmt.Fprintf(w, "  - %s\n", n)
			}
			if len(r.StreamKinds) > 0 {
				fmt.Fprintf(w, "  stream kinds: %s\n", kindsString(r.StreamKinds))
			}
		}
	}

	var totalMS int64
	for _, r := range rows {
		totalMS += r.Install.MS + r.Catalog.MS + r.Search.MS + r.Meta.MS + r.Stream.MS + r.Indexer.MS + r.Subs.MS + r.LiveTV.MS + r.AddCat.MS
	}
	fmt.Fprintf(w, "\ntotal check time: %s across %d add-ons\n", time.Duration(totalMS)*time.Millisecond, len(rows))
}

func cellText(c resultCell) string {
	switch c.Status {
	case "PASS":
		return "PASS"
	case "N/A":
		return "N/A"
	case "UNREACHABLE":
		return "UNREACH " + paren(c.Detail)
	case "FAIL":
		return "FAIL " + paren(c.Detail)
	default:
		if c.Status == "" {
			return "-"
		}
		return c.Status
	}
}

func paren(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 70 {
		s = s[:67] + "..."
	}
	return "(" + s + ")"
}
