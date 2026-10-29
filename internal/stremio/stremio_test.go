package stremio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
)

const (
	hashA = "a584351126afcf50d32be068f3c9a62550d41ecc"
	hashB = "f0fd1cad0e24b69ff6aaadb9df40c50286327188"
	hashC = "1b42b99b9fbc0e0416a379787f250eb73b7434e3"
	hashD = "aa1a43787ff85fe5d029969857b0d64a6c9a05e4"
	// secret stands in for a debrid key inside an addon's settings path.
	secret = "SECRETKEY123"
)

const manifestJSON = `{"id":"org.example.streams","version":"1.0.0","name":"Example","resources":[{"name":"stream","types":["movie","series"],"idPrefixes":["tt","kitsu"]}],"types":["movie","series"],"catalogs":[]}`

// Shaped like real addon answers: torrent name, the file inside a pack,
// then a statistics line and a flags line.
var episodeStreams = `{"streams":[
 {"name":"Example\n4k","title":"Show (2008) S01 (2160p WEB-DL H265 DDP 5.1 - GRP)\nShow (2008) S01E02 (2160p WEB-DL H265 DDP 5.1 - GRP).mkv\n👤 73 💾 5.17 GB ⚙️ SiteA","infoHash":"` + strings.ToUpper(hashA) + `","fileIdx":1,"behaviorHints":{"filename":"Show (2008) S01E02 (2160p WEB-DL H265 DDP 5.1 - GRP).mkv"},"sources":["tracker:udp://tracker.example:1337/announce","dht:` + hashA + `"]},
 {"name":"Example\n4k","title":"Шоу / Show / Сезон: 1 [2008 WEB-DL 2160p]\nShow.S01E02.2160p.AMZN.WEB-DL.DTS-HD.MA.5.1.HEVC.mkv\n👤 69 💾 9.55 GB ⚙️ SiteB\n🇬🇧 / 🇷🇺","infoHash":"` + hashB + `","fileIdx":1},
 {"name":"Example\n1080p","title":"Show S01E02 1080p WEB-DL DDP5 1 H 264-XYZ\n👤 25 💾 1.5 GB ⚙️ SiteA","infoHash":"` + hashD + `","fileIdx":0,"behaviorHints":{"videoSize":1610612736}},
 {"name":"Example\n1080p","title":"Show S01E02 1080p WEB-DL DDP5 1 H 264-XYZ duplicate","infoHash":"` + hashD + `","fileIdx":0},
 {"name":"Usenet","title":"Show S01E02 1080p","nzbUrl":"https://nzb.example/x.nzb"},
 {"name":"Direct","title":"Show S01E02 720p","url":"https://cdn.example/video.mp4"}
]}`

var seasonStreams = `{"streams":[
 {"name":"Example","title":"Show (2008) S01 (2160p WEB-DL H265 DDP 5.1 - GRP)\nShow (2008) S01E01 (2160p WEB-DL H265 DDP 5.1 - GRP).mkv\n👤 73 💾 5.17 GB","infoHash":"` + hashA + `","fileIdx":0},
 {"name":"Example","title":"Show COMPLETE S01-S05 2160p WEB-DL x264-GRP\nShow.S01.2160p/Show.S01E01.2160p.WEB-DL.x264-GRP.mkv\n👤 37 💾 35.61 GB","infoHash":"` + hashC + `","fileIdx":1},
 {"name":"Example","title":"Show S01E01 1080p WEB-DL H 264-XYZ\n👤 25 💾 1.5 GB","infoHash":"` + hashD + `","fileIdx":0}
]}`

// A keyed addon answers with links to its own resolver and cached markers.
var keyedStreams = `{"streams":[
 {"name":"[RD+] Example\n4k","title":"Movie 2014 2160p REMUX DV HDR10+ TrueHD 7.1 Atmos-GRP\n👤 263 💾 95.63 GB","url":"https://addon.example/resolve/realdebrid/` + secret + `/` + hashA + `/null/2/Movie.mkv"},
 {"name":"[RD download] Example\n1080p","title":"Movie 2014 1080p BluRay x264-GRP\n👤 120 💾 12 GB","url":"https://addon.example/resolve/realdebrid/` + secret + `/` + hashB + `/null/0/Movie.mkv"},
 {"name":"[TB⚡] [RD⬇️] Other 4K","description":"Movie.2014.2160p.WEB-DL.DDP5.1.HEVC-GRP.mkv\n💾 20.1 GB 👤 50","url":"https://addon.example/playback?info_hash=` + hashC + `&file=0"}
]}`

type fakeAddon struct {
	mu        sync.Mutex
	paths     []string
	manifests int
	streams   map[string]string // request path suffix -> body
	status    int
	retry     string
}

func (f *fakeAddon) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.EscapedPath())
		status, retry := f.status, f.retry
		f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/manifest.json") {
			f.mu.Lock()
			f.manifests++
			f.mu.Unlock()
			fmt.Fprint(w, manifestJSON)
			return
		}
		if status != 0 {
			if retry != "" {
				w.Header().Set("Retry-After", retry)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, `{"err":"`+r.URL.Path+`"}`)
			return
		}
		for suffix, body := range f.streams {
			if strings.HasSuffix(r.URL.Path, suffix) {
				fmt.Fprint(w, body)
				return
			}
		}
		fmt.Fprint(w, `{"streams":[]}`)
	})
}

func (f *fakeAddon) streamRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.paths {
		if strings.Contains(p, "/stream/") {
			out = append(out, p)
		}
	}
	return out
}

func noReserve(context.Context, string) error { return nil }

func backendFor(t *testing.T, rawURL string) *Backend {
	t.Helper()
	b, err := NewService(nil).Backend(config.Indexer{Name: "addon", Kind: config.IndexerKindStremio, URL: rawURL})
	if err != nil {
		t.Fatal(err)
	}
	return b.(*Backend)
}

func TestBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://addon.example/manifest.json":                           "https://addon.example",
		"  stremio://addon.example/manifest.json ":                      "https://addon.example",
		"https://addon.example/providers=a,b%7Csort=size/manifest.json": "https://addon.example/providers=a,b%7Csort=size",
		"https://addon.example/abc/configure":                           "https://addon.example/abc",
		"https://user:pw@addon.example/x/?q=1#frag":                     "https://addon.example/x",
	} {
		u, err := BaseURL(raw)
		if err != nil || u.String() != want {
			t.Errorf("BaseURL(%q) = %v, %v; want %s", raw, u, err, want)
		}
	}
	for _, raw := range []string{"", "ftp://addon.example", "addon.example/manifest.json"} {
		if _, err := BaseURL(raw); err == nil {
			t.Errorf("BaseURL(%q) accepted", raw)
		}
	}
}

func TestManifestCheck(t *testing.T) {
	for body, want := range map[string]string{
		manifestJSON: "",
		`{"id":"x","resources":["stream"],"types":["movie"],"idPrefixes":["tt"]}`:                            "",
		`{"id":"x","resources":["catalog","meta"],"types":["movie"]}`:                                        "does not provide streams",
		`{"id":"x","resources":[{"name":"stream","types":["tv"]}]}`:                                          "movie or series",
		`{"id":"x","resources":[{"name":"stream","types":["movie"],"idPrefixes":["kitsu"]}]}`:                "IMDb",
		`{"id":"x","resources":["stream"],"types":["movie"],"behaviorHints":{"configurationRequired":true}}`: "configured first",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		err := NewService(srv.Client()).Test(t.Context(), config.Indexer{URL: srv.URL + "/manifest.json"})
		srv.Close()
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("manifest %s: err %v, want %q", body, err, want)
		}
	}
}

func TestSearchEpisodeReadsPackFilesAndSkipsNonTorrents(t *testing.T) {
	f := &fakeAddon{streams: map[string]string{"/stream/series/tt0903747:1:2.json": episodeStreams}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL+"/providers=a,b%7Csort=size/manifest.json")
	req := core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{IMDB: "tt0903747"}, Title: "Show", Year: 2008, Season: 1, Episode: 2}
	rels, err := b.Search(t.Context(), req, noReserve)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.streamRequests(); len(got) != 1 || got[0] != "/providers=a,b%7Csort=size/stream/series/tt0903747:1:2.json" {
		t.Fatalf("stream requests %v", got)
	}
	if len(rels) != 3 {
		t.Fatalf("want 3 torrent releases (duplicate, Usenet and direct links skipped), got %d: %+v", len(rels), rels)
	}
	a := rels[0]
	if a.InfoHash != hashA || a.Protocol != core.ProtocolTorrent || a.GUID != "btih:"+hashA+"/1" || a.Category != tvCategory {
		t.Fatalf("pack file release %+v", a)
	}
	if a.Name != "Show (2008) S01E02 (2160p WEB-DL H265 DDP 5.1 - GRP)" || a.Info.Height != 2160 {
		t.Fatalf("pack file should be named by its episode file: %q %+v", a.Name, a.Info)
	}
	if a.Seeders != 73 || a.Size != parseSize("💾 5.17 GB") || a.Size < 5<<30 {
		t.Fatalf("stats: seeders %d size %d", a.Seeders, a.Size)
	}
	if !strings.HasPrefix(a.MagnetURL, "magnet:?xt=urn:btih:"+hashA) || !strings.Contains(a.MagnetURL, "tracker.example") || strings.Contains(a.MagnetURL, "dht%3A") {
		t.Fatalf("magnet %q", a.MagnetURL)
	}
	// A foreign-language torrent name: the episode file names the release.
	if rels[1].Name != "Show.S01E02.2160p.AMZN.WEB-DL.DTS-HD.MA.5.1.HEVC" || rels[1].Seeders != 69 {
		t.Fatalf("foreign pack %q seeders %d", rels[1].Name, rels[1].Seeders)
	}
	// behaviorHints.videoSize wins over the text size.
	if rels[2].Size != 1610612736 || rels[2].Name != "Show S01E02 1080p WEB-DL DDP5 1 H 264-XYZ" {
		t.Fatalf("single episode %+v", rels[2])
	}
}

func TestSearchSeasonKeepsWholeSeasonPacks(t *testing.T) {
	f := &fakeAddon{streams: map[string]string{"/stream/series/tt0903747:1:1.json": seasonStreams}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL)
	rels, err := b.Search(t.Context(), core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{IMDB: "0903747"}, Title: "Show", Season: 1}, noReserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 {
		t.Fatalf("want the two packs, got %+v", rels)
	}
	for _, r := range rels {
		if r.Info.Episodes != nil || strings.Contains(r.GUID, "/") {
			t.Fatalf("pack release %+v", r)
		}
	}
	if rels[0].Name != "Show (2008) S01 (2160p WEB-DL H265 DDP 5.1 - GRP)" || rels[1].InfoHash != hashC {
		t.Fatalf("packs %q %q", rels[0].Name, rels[1].Name)
	}
}

func TestSearchKeyedAddonReadsHashesAndCachedMarkers(t *testing.T) {
	f := &fakeAddon{streams: map[string]string{"/stream/movie/tt0816692.json": keyedStreams}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL+"/realdebrid="+secret+"/manifest.json")
	rels, err := b.Search(t.Context(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0816692"}, Title: "Movie", Year: 2014}, noReserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 3 {
		t.Fatalf("got %+v", rels)
	}
	want := []struct {
		hash             string
		cached, uncached []string
	}{
		{hashA, []string{config.DebridRealDebrid}, nil},
		{hashB, nil, []string{config.DebridRealDebrid}},
		{hashC, []string{config.DebridTorBox}, []string{config.DebridRealDebrid}},
	}
	for i, w := range want {
		r := rels[i]
		if r.InfoHash != w.hash || fmt.Sprint(r.DebridCached) != fmt.Sprint(w.cached) || fmt.Sprint(r.DebridUncached) != fmt.Sprint(w.uncached) {
			t.Errorf("release %d: hash %s cached %v uncached %v; want %+v", i, r.InfoHash, r.DebridCached, r.DebridUncached, w)
		}
		if strings.Contains(r.MagnetURL, secret) || r.Category != movieCategory {
			t.Errorf("release %d leaks the addon key or has the wrong category: %+v", i, r)
		}
	}
	if rels[0].Info.Height != 2160 || !rels[0].Info.DolbyVision || rels[2].Name != "Movie.2014.2160p.WEB-DL.DDP5.1.HEVC-GRP" {
		t.Fatalf("names: %+v / %q", rels[0].Info, rels[2].Name)
	}
}

func TestSearchURLOnlyStreamsAreSkippedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	body := `{"streams":[
	 {"name":"Direct","title":"Movie 2014 1080p BluRay x264-GRP","url":"https://cdn.example/video.mp4"},
	 {"name":"Direct2","title":"Movie 2014 720p BluRay x264-GRP","url":"https://cdn.example/other.mp4"}
	]}`
	f := &fakeAddon{streams: map[string]string{"/stream/movie/tt0816692.json": body}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL+"/realdebrid="+secret+"/manifest.json")
	rels, err := b.Search(t.Context(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0816692"}, Title: "Movie", Year: 2014}, noReserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 0 {
		t.Fatalf("want 0 releases from URL-only streams, got %+v", rels)
	}
	log := buf.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "skippedNoHash=2") || !strings.Contains(log, "skippedAmbiguous=0") || !strings.Contains(log, "host="+strings.TrimPrefix(srv.URL, "http://")) {
		t.Fatalf("log sem host/contador observável: %q", log)
	}
	if strings.Contains(log, secret) || strings.Contains(log, "cdn.example") || strings.Contains(log, "/stream/") {
		t.Fatalf("log vazou URL/credencial: %q", log)
	}
	// O contador também é observável direto no convert.
	if _, skipped, ambiguous := convert([]stream{
		{Title: "Movie 2014 1080p", URL: "https://cdn.example/video.mp4"},
		{Title: "Movie 2014 720p", URL: "https://cdn.example/other.mp4"},
	}, core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false); skipped != 2 || ambiguous != 0 {
		t.Fatalf("skippedNoHash=%d skippedAmbiguous=%d want 2/0", skipped, ambiguous)
	}
}

// Contador honesto: só conta quem seria release se tivesse hash —
// duplicata sem-hash conta 1x, sem-nome não conta, e hash conflitante
// conta à parte como ambíguo, nunca como "without hash".
func TestConvertSkippedCountsOnlyHashCausedSkips(t *testing.T) {
	rels, skipped, ambiguous := convert([]stream{
		{Title: "Movie 2014 1080p BluRay x264-GRP", URL: "https://cdn.example/video.mp4"},
		{Title: "Movie 2014 1080p BluRay x264-GRP", URL: "https://cdn.example/video.mp4"}, // duplicata exata
		{Title: "Movie 2014 720p BluRay x264-GRP", URL: "https://cdn.example/other.mp4"},
		{URL: "https://cdn.example/unnamed.mp4"},                                                          // sem nome: morreria de qualquer jeito
		{Title: "Movie 2014 2160p BluRay x264-GRP", InfoHash: hashA, URL: "magnet:?xt=urn:btih:" + hashB}, // ambíguo
		{Title: "Movie 2014 2160p BluRay x264-GRP", InfoHash: hashA, URL: "magnet:?xt=urn:btih:" + hashB}, // ambíguo duplicado
	}, core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false)
	if len(rels) != 0 || skipped != 2 || ambiguous != 1 {
		t.Fatalf("rels=%d skippedNoHash=%d skippedAmbiguous=%d want 0/2/1", len(rels), skipped, ambiguous)
	}

	// Fora-de-pack não conta; pack sem hash conta (1x mesmo duplicado).
	rels, skipped, ambiguous = convert([]stream{
		{Title: "Show S01E02 1080p WEB-DL H 264-XYZ"},
		{Title: "Show (2008) S01 (2160p WEB-DL H265 DDP 5.1 - GRP)"},
		{Title: "Show (2008) S01 (2160p WEB-DL H265 DDP 5.1 - GRP)"},
	}, core.SearchRequest{Type: core.TypeEpisode, Title: "Show", Season: 1}, true)
	if len(rels) != 0 || skipped != 1 || ambiguous != 0 {
		t.Fatalf("packs: rels=%d skippedNoHash=%d skippedAmbiguous=%d want 0/1/0", len(rels), skipped, ambiguous)
	}
}

// Throttle: 3 Search seguidas com skips geram 1 Warn; após a janela,
// warn de novo. Relógio falso injetado em b.now, sem sleep.
func TestSearchSkipWarnThrottledPerBackend(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	body := `{"streams":[
	 {"name":"Direct","title":"Movie 2014 1080p BluRay x264-GRP","url":"https://cdn.example/video.mp4"}
	]}`
	f := &fakeAddon{streams: map[string]string{"/stream/movie/tt0816692.json": body}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	req := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0816692"}, Title: "Movie", Year: 2014}
	for i := 0; i < 3; i++ {
		if _, err := b.Search(t.Context(), req, noReserve); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("3 searches seguidas: %d WARN, want 1: %q", n, buf.String())
	}
	now = now.Add(skipWarnInterval + time.Minute)
	if _, err := b.Search(t.Context(), req, noReserve); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 2 {
		t.Fatalf("após a janela: %d WARN, want 2: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), "skippedNoHash=1") || !strings.Contains(buf.String(), "skippedAmbiguous=0") {
		t.Fatalf("warn sem os dois contadores: %q", buf.String())
	}
}

func TestSearchMagnetStreamBecomesRelease(t *testing.T) {
	body := `{"streams":[
	 {"name":"Magnet","title":"Movie 2014 1080p BluRay x264-GRP\n👤 12 💾 2 GB","url":"magnet:?xt=urn:btih:` + hashA + `&dn=Movie"}
	]}`
	f := &fakeAddon{streams: map[string]string{"/stream/movie/tt0816692.json": body}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL)
	rels, err := b.Search(t.Context(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0816692"}, Title: "Movie", Year: 2014}, noReserve)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].InfoHash != hashA || !strings.HasPrefix(rels[0].MagnetURL, "magnet:?xt=urn:btih:"+hashA) {
		t.Fatalf("magnet stream não virou release: %+v", rels)
	}
}

func TestSearchWithoutIMDbOrForShowsAsksNothing(t *testing.T) {
	f := &fakeAddon{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL)
	reserved := 0
	reserve := func(context.Context, string) error { reserved++; return nil }
	for _, req := range []core.SearchRequest{
		{Type: core.TypeMovie, Title: "No IDs"},
		{Type: core.TypeMovie, IDs: core.IDs{IMDB: "not-an-id"}},
		{Type: core.TypeShow, IDs: core.IDs{IMDB: "tt1"}},
		{Type: core.TypeEpisode, IDs: core.IDs{IMDB: "tt1"}},
	} {
		if rels, err := b.Search(t.Context(), req, reserve); rels != nil || err != nil {
			t.Fatalf("%+v: %v %v", req, rels, err)
		}
	}
	if reserved != 0 || len(f.streamRequests()) != 0 {
		t.Fatalf("reserved %d, requests %v", reserved, f.streamRequests())
	}
}

func TestSearchErrorsNeverLeakTheAddonPath(t *testing.T) {
	f := &fakeAddon{status: http.StatusInternalServerError}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	b := backendFor(t, srv.URL+"/realdebrid="+secret)
	_, err := b.Search(t.Context(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt1"}}, noReserve)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err %v", err)
	}
	srv.Close() // connection refused: *url.Error would print the full URL
	_, err = b.Search(t.Context(), core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt1"}}, noReserve)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport err %v", err)
	}
	if _, err := NewService(nil).Backend(config.Indexer{Name: "x", URL: "ftp://h/" + secret}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("config err %v", err)
	}
}

// Through the indexer Client: results are cached, budgets are reserved,
// and a 429 pauses the addon (Retry-After honored) instead of retrying.
func TestClientCachesAndPausesAddon(t *testing.T) {
	f := &fakeAddon{streams: map[string]string{"/stream/movie/tt0816692.json": keyedStreams}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	svc := NewService(srv.Client())
	cfg := config.Indexer{ID: "ad", Name: "Addon", Kind: config.IndexerKindStremio, Protocol: "torrent", URL: srv.URL, Enabled: true}
	c := newznab.New(cfg, nil, newznab.Options{Backend: svc.Backend})
	req := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0816692"}, Title: "Movie", Year: 2014}
	for i := 0; i < 2; i++ {
		rels, err := c.Search(t.Context(), req)
		if err != nil || len(rels) != 3 || rels[0].Indexer != "Addon" || rels[0].ID == "" {
			t.Fatalf("search %d: %v %+v", i, err, rels)
		}
	}
	if n := len(f.streamRequests()); n != 1 {
		t.Fatalf("cached search asked the addon again: %d requests", n)
	}
	if f.manifests != 1 {
		t.Fatalf("manifest fetched %d times", f.manifests)
	}

	f.mu.Lock()
	f.status, f.retry = http.StatusTooManyRequests, "120"
	f.mu.Unlock()
	other := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0000002"}, Title: "Other"}
	if _, err := c.Search(t.Context(), other); !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatalf("429: %v", err)
	}
	third := core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{IMDB: "tt0000003"}, Title: "Third"}
	if _, err := c.Search(t.Context(), third); !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatalf("paused addon: %v", err)
	}
	if n := len(f.streamRequests()); n != 2 {
		t.Fatalf("paused addon was asked again: %d requests", n)
	}
}

func TestDebridHints(t *testing.T) {
	for name, want := range map[string]string{
		"[RD+] Torrentio\n4k":           "[realdebrid] []",
		"[RD download] Torrentio":       "[] [realdebrid]",
		"[RD⚡] Comet 4K":                "[realdebrid] []",
		"[AD⬇️] Comet":                  "[] [alldebrid]",
		"MediaFusion PM ⏳ 1080p":        "[] [premiumize]",
		"[TB+] [PM+] Multi":             "[torbox premiumize] []",
		"[DL+] Torrentio\n1080p":        "[debridlink] []",
		"[OC download] Torrentio":       "[] [offcloud]",
		"[DL⚡] [OC⬇️] Comet":            "[debridlink] [offcloud]",
		"Torrentio 1080p WEB-DL+ x264":  "[] []",
		"Torrentio\n4k HDR":             "[] []",
		"BRAD+ADVENTURE no service tag": "[] []",
	} {
		c, u := debridHints(name)
		if got := fmt.Sprint(c) + " " + fmt.Sprint(u); got != want {
			t.Errorf("%q: %s, want %s", name, got, want)
		}
	}
}

func FuzzConvert(f *testing.F) {
	f.Add(episodeStreams, "Show", 1, 2)
	f.Add(keyedStreams, "Movie", 0, 0)
	f.Fuzz(func(t *testing.T, text, title string, season, episode int) {
		s := []stream{{Name: text, Title: text, Description: text, InfoHash: hashA, URL: text}}
		req := core.SearchRequest{Type: core.TypeEpisode, Title: title, Season: season, Episode: episode}
		for _, packs := range []bool{false, true} {
			rels, _, _ := convert(s, req, packs)
			for _, r := range rels {
				if r.InfoHash != hashA || r.Protocol != core.ProtocolTorrent {
					t.Fatalf("%+v", r)
				}
			}
		}
	})
}

// A pack's episode file often names only the episode; the torrent name
// supplies the quality facts ranking needs.
func TestEpisodeFileBorrowsQualityFromTorrentName(t *testing.T) {
	s := []stream{{Title: "Show S01 2160p WEB-DL DV HEVC DDP5.1 Atmos-GRP\nShow S01E02 Episode Title.mkv\n👤 9 💾 7 GB", InfoHash: hashA}}
	rels, _, _ := convert(s, core.SearchRequest{Type: core.TypeEpisode, Title: "Show", Season: 1, Episode: 2}, false)
	if len(rels) != 1 {
		t.Fatalf("%+v", rels)
	}
	info := rels[0].Info
	if rels[0].Name != "Show S01E02 Episode Title" || info.Height != 2160 || !info.DolbyVision || !info.Atmos || info.Group != "GRP" || fmt.Sprint(info.Episodes) != "[2]" {
		t.Fatalf("%q %+v", rels[0].Name, info)
	}
}

// Ether identifies itself; an addon that refuses servers gets a clear,
// actionable error that still names only the host.
func TestHonestUserAgentAndRefusal(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	err := NewService(nil).Test(t.Context(), config.Indexer{URL: srv.URL + "/" + secret + "/manifest.json"})
	if ua != userAgent || !strings.HasPrefix(ua, "Ether ") {
		t.Fatalf("user agent %q", ua)
	}
	if err == nil || !strings.Contains(err.Error(), "refused Ether") || strings.Contains(err.Error(), secret) {
		t.Fatalf("err %v", err)
	}
}
