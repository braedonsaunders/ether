package stremio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type fakeAnime struct {
	mu    sync.Mutex
	calls []string
	ids   map[string][2]int64 // db -> id, episode
	err   error
}

func (f *fakeAnime) Lookup(_ context.Context, req core.SearchRequest, db string) (int64, int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, db)
	if f.err != nil {
		return 0, 0, false, f.err
	}
	v, ok := f.ids[db]
	return v[0], int(v[1]), ok, nil
}

func backendWith(t *testing.T, rawURL string, o Options, id string) *Backend {
	t.Helper()
	b, err := NewServiceWithOptions(o).Backend(config.Indexer{ID: id, Name: "addon", Kind: config.IndexerKindStremio, URL: rawURL})
	if err != nil {
		t.Fatal(err)
	}
	return b.(*Backend)
}

const kitsuManifest = `{"id":"anime","resources":[{"name":"stream","types":["series","movie"],"idPrefixes":["kitsu:"]}],"types":["series","movie"]}`

func animeRequest() core.SearchRequest {
	return core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{IMDB: "tt2560140", TMDB: 1429}, Title: "Attack on Titan",
		Season: 3, Episode: 15, AbsoluteEpisode: 52, Anime: true}
}

func TestAnimeOnlyAddonIsAskedByMappedEntryEpisode(t *testing.T) {
	body := `{"streams":[
	 {"name":"Anime","title":"[SubsPlease] Shingeki no Kyojin - 03 (1080p) [ABCD1234].mkv\n👤 40 💾 1.4 GB","infoHash":"` + hashA + `"},
	 {"name":"Anime","title":"[Group] Shingeki no Kyojin - 52 (1080p)","infoHash":"` + hashB + `"},
	 {"name":"Anime","title":"[Other] Shingeki no Kyojin - 07 (1080p)","infoHash":"` + hashC + `"}
	]}`
	f := &fakeAddon{manifest: kitsuManifest, streams: map[string]string{"/stream/series/kitsu:41982:3.json": body}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	anime := &fakeAnime{ids: map[string][2]int64{"kitsu": {41982, 3}}}
	b := backendWith(t, srv.URL, Options{Client: srv.Client(), Anime: anime}, "ix1")
	rels, err := b.Search(t.Context(), animeRequest(), noReserve)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.streamRequests(); len(got) != 1 || got[0] != "/stream/series/kitsu:41982:3.json" {
		t.Fatalf("stream requests %v", got)
	}
	if len(rels) != 3 {
		t.Fatalf("releases %+v", rels)
	}
	// The entry's third episode is the requested S3E15...
	if i := rels[0].Info; len(i.Seasons) != 1 || i.Seasons[0] != 3 || len(i.Episodes) != 1 || i.Episodes[0] != 15 {
		t.Fatalf("entry episode identity %+v", i)
	}
	// ...a series-absolute number keeps its own identity...
	if i := rels[1].Info; len(i.Seasons) != 0 || len(i.AbsoluteEpisodes) != 1 || i.AbsoluteEpisodes[0] != 52 {
		t.Fatalf("absolute release %+v", i)
	}
	// ...and another number is not relabelled as the requested episode.
	if i := rels[2].Info; len(i.Seasons) != 0 || (len(i.Episodes) > 0 && i.Episodes[0] == 15) || i.AbsoluteEpisodes[0] != 7 {
		t.Fatalf("other number relabelled %+v", i)
	}
}

func TestAnimeMappingIsOnlyConsultedWhenNeeded(t *testing.T) {
	f := &fakeAddon{manifest: kitsuManifest}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	anime := &fakeAnime{ids: map[string][2]int64{"kitsu": {41982, 3}}}
	b := backendWith(t, srv.URL, Options{Client: srv.Client(), Anime: anime}, "ix1")
	notAnime := animeRequest()
	notAnime.Anime = false
	season := animeRequest()
	season.Episode = 0
	for _, req := range []core.SearchRequest{notAnime, season} {
		if rels, err := b.Search(t.Context(), req, noReserve); err != nil || len(rels) != 0 {
			t.Fatalf("rels %v err %v", rels, err)
		}
	}
	if len(anime.calls) != 0 || len(f.streamRequests()) != 0 {
		t.Fatalf("mapping calls %v, requests %v", anime.calls, f.streamRequests())
	}
	// An unavailable or ambiguous mapping skips the addon quietly.
	anime.err = errors.New("unavailable")
	if rels, err := b.Search(t.Context(), animeRequest(), noReserve); err != nil || len(rels) != 0 || len(f.streamRequests()) != 0 {
		t.Fatalf("unavailable mapping: rels %v err %v requests %v", rels, err, f.streamRequests())
	}
	anime.err, anime.ids = nil, nil
	if rels, err := b.Search(t.Context(), animeRequest(), noReserve); err != nil || len(rels) != 0 || len(f.streamRequests()) != 0 {
		t.Fatalf("unmapped title: rels %v err %v", rels, err)
	}
}

func TestIDChoicePrefersIMDbThenTMDB(t *testing.T) {
	for _, tc := range []struct {
		manifest string
		req      core.SearchRequest
		want     string
	}{
		{`{"id":"x","resources":[{"name":"stream","types":["movie","series"],"idPrefixes":["tt","tmdb:","kitsu:"]}]}`,
			animeRequest(), "/stream/series/tt2560140:3:15.json"},
		{`{"id":"x","resources":[{"name":"stream","types":["movie","series"],"idPrefixes":["tmdb:","kitsu:"]}]}`,
			animeRequest(), "/stream/series/tmdb:1429:3:15.json"},
		{`{"id":"x","resources":[{"name":"stream","types":["movie"],"idPrefixes":["tt","tmdb"]}]}`,
			core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 603}, Title: "M"}, "/stream/movie/tmdb:603.json"},
		{`{"id":"x","resources":[{"name":"stream","types":["movie"],"idPrefixes":["kitsu"]}]}`,
			core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 5000}, Title: "Film", Anime: true}, "/stream/movie/kitsu:108.json"},
	} {
		f := &fakeAddon{manifest: tc.manifest}
		srv := httptest.NewServer(f.handler(t))
		anime := &fakeAnime{ids: map[string][2]int64{"kitsu": {108, 0}}}
		b := backendWith(t, srv.URL, Options{Client: srv.Client(), Anime: anime}, "ix1")
		if _, err := b.Search(t.Context(), tc.req, noReserve); err != nil {
			t.Fatal(err)
		}
		if got := f.streamRequests(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("manifest %s: requests %v, want %s", tc.manifest, got, tc.want)
		}
		srv.Close()
	}
}

func TestHTTPStreamIdentityIgnoresCredentialsAndNamesItsAddon(t *testing.T) {
	mk := func(u string) []stream {
		return []stream{{Name: "[RD+] Addon 4K", Title: "Movie 2014 2160p WEB-DL x265-GRP", URL: u, BehaviorHints: behaviorHints{VideoSize: 4 << 30}}}
	}
	req := core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}
	a, _, _ := convertWith(mk("https://addon.example/play/"+secret+"/token-1"), req, false, convertOptions{instance: "ix1"})
	b, _, _ := convertWith(mk("https://addon.example/play/"+secret+"/token-2"), req, false, convertOptions{instance: "ix1"})
	c, _, _ := convertWith(mk("https://addon.example/play/"+secret+"/token-1"), req, false, convertOptions{instance: "ix2"})
	if len(a) != 1 || a[0].GUID != b[0].GUID {
		t.Fatalf("GUID changed with the link's token: %q vs %q", a[0].GUID, b[0].GUID)
	}
	if a[0].GUID == c[0].GUID {
		t.Fatal("two configured addons share an HTTP release identity")
	}
	if strings.Contains(a[0].GUID, secret) || a[0].DebridCached[0] != config.DebridRealDebrid {
		t.Fatalf("release %+v", a[0])
	}
	// A different file or size is a different release.
	d, _, _ := convertWith([]stream{{Name: "[RD+] Addon 4K", Title: "Movie 2014 2160p WEB-DL x265-GRP", URL: "https://addon.example/x", BehaviorHints: behaviorHints{VideoSize: 5 << 30}}}, req, false, convertOptions{instance: "ix1"})
	if d[0].GUID == a[0].GUID {
		t.Fatal("size not part of the identity")
	}
}

func TestTorrentStreamKeepsAddonLinkAsFallback(t *testing.T) {
	streams := []stream{{Name: "[RD+] T", Title: "Movie 2014 1080p BluRay x264-GRP", URL: "https://addon.example/resolve/realdebrid/" + secret + "/" + hashA + "/null/0/Movie.mkv"}}
	rels, _, _ := convertWith(streams, core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false, convertOptions{origin: "https://addon.example"})
	if len(rels) != 1 || rels[0].Protocol != core.ProtocolTorrent || rels[0].InfoHash != hashA || rels[0].StreamURL == "" || rels[0].StreamOrigin != "https://addon.example" {
		t.Fatalf("release %+v", rels)
	}
	// A magnet is no HTTP fallback.
	rels, _, _ = convertWith([]stream{{Title: "Movie 2014 1080p BluRay x264-GRP", URL: "magnet:?xt=urn:btih:" + hashA}}, core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false, convertOptions{})
	if len(rels) != 1 || rels[0].StreamURL != "" {
		t.Fatalf("magnet release %+v", rels)
	}
}

func TestStreamRequestHeadersAreBounded(t *testing.T) {
	s := stream{BehaviorHints: behaviorHints{}}
	s.BehaviorHints.ProxyHeaders.Request = map[string]string{
		"referer": "https://site.example/", "Range": "bytes=0-", "Host": "evil.example",
		"X-Inject": "a\r\nX-Other: b", "User-Agent": "AddonPlayer/1.0",
	}
	got := s.requestHeaders()
	if len(got) != 2 || got["Referer"] != "https://site.example/" || got["User-Agent"] != "AddonPlayer/1.0" {
		t.Fatalf("headers %v", got)
	}
}

func TestUncachedHTTPStreamKeepsItsLabel(t *testing.T) {
	rels, _, _ := convertWith([]stream{{Name: "[RD download] Addon", Title: "Movie 2014 1080p BluRay x264-GRP", URL: "https://addon.example/x"}},
		core.SearchRequest{Type: core.TypeMovie, Title: "Movie"}, false, convertOptions{})
	if len(rels) != 1 || len(rels[0].DebridUncached) != 1 || len(rels[0].DebridCached) != 0 {
		t.Fatalf("release %+v", rels)
	}
}

func TestFetchNZBFromAddonOriginOnly(t *testing.T) {
	const nzb = `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file subject="x"></file></nzb>`
	addon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + secret + "/get.nzb":
			fmt.Fprint(w, nzb)
		case "/" + secret + "/page":
			fmt.Fprint(w, "<html>login</html>")
		case "/" + secret + "/gone":
			http.Error(w, "gone", http.StatusNotFound)
		case "/" + secret + "/elsewhere":
			http.Redirect(w, r, "http://127.0.0.2:1/x.nzb", http.StatusFound)
		default:
			fmt.Fprint(w, manifestJSON)
		}
	}))
	defer addon.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, nzb) }))
	defer other.Close()
	b := backendWith(t, addon.URL+"/"+secret+"/manifest.json", Options{}, "ix1")
	var grabs []string
	reserve := func(_ context.Context, kind string) error { grabs = append(grabs, kind); return nil }
	rel := core.Release{Name: "Movie 2014 1080p", Protocol: core.ProtocolUsenet, NZBURL: addon.URL + "/" + secret + "/get.nzb"}
	body, err := b.FetchNZB(t.Context(), rel, reserve)
	if err != nil || !strings.Contains(string(body), "<nzb") || len(grabs) != 1 || grabs[0] != "grab" {
		t.Fatalf("addon NZB: %v %q grabs %v", err, body, grabs)
	}
	for name, link := range map[string]string{
		"private host other than the addon": other.URL + "/x.nzb",
		"redirect away from the addon":      addon.URL + "/" + secret + "/elsewhere",
		"not an NZB":                        addon.URL + "/" + secret + "/page",
		"HTTP error":                        addon.URL + "/" + secret + "/gone",
	} {
		rel.NZBURL = link
		_, err := b.FetchNZB(t.Context(), rel, reserve)
		if err == nil {
			t.Errorf("%s: fetched", name)
			continue
		}
		if errors.Is(err, core.ErrReleaseRejected) || errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) {
			t.Errorf("%s: server answer classified as permanent: %v", name, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the addon path: %v", name, err)
		}
	}
	if _, err := b.FetchNZB(t.Context(), core.Release{Name: "t", Protocol: core.ProtocolTorrent, InfoHash: hashA}, reserve); !errors.Is(err, core.ErrReleaseRejected) {
		t.Fatalf("torrent: %v", err)
	}
	if _, err := b.FetchNZB(t.Context(), core.Release{Name: "m", Protocol: core.ProtocolUsenet, NZBURL: "stremio:///x"}, reserve); !errors.Is(err, core.ErrReleaseRejected) {
		t.Fatalf("malformed NZB link: %v", err)
	}
	budget := errors.New("budget")
	rel.NZBURL = addon.URL + "/" + secret + "/get.nzb"
	if _, err := b.FetchNZB(t.Context(), rel, func(context.Context, string) error { return budget }); !errors.Is(err, budget) {
		t.Fatalf("grab budget ignored: %v", err)
	}
}
