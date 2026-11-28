package animemap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// fixture mirrors the dataset's shape with synthetic ids: a show split
// into seasons and cours (with a special filed under season 4), a long
// TMDB season whose cours are separate TVDB seasons (#559), a show with
// one whole-series entry, an ambiguous season, and films.
const fixture = `[
 {"type":"TV","kitsu_id":101,"mal_id":201,"anilist_id":301,"anidb_id":401,"themoviedb_id":{"tv":1000},"season":{"tvdb":1,"tmdb":1}},
 {"type":"OVA","kitsu_id":102,"themoviedb_id":{"tv":1000},"season":{"tvdb":0,"tmdb":0}},
 {"type":"TV","kitsu_id":103,"themoviedb_id":{"tv":1000},"season":{"tvdb":3,"tmdb":3}},
 {"type":"TV","kitsu_id":104,"themoviedb_id":{"tv":1000},"season":{"tvdb":3,"tmdb":3},"episode_offset":{"tvdb":12,"tmdb":12}},
 {"type":"TV","kitsu_id":105,"themoviedb_id":{"tv":1000},"season":{"tvdb":4,"tmdb":4}},
 {"type":"TV","kitsu_id":106,"themoviedb_id":{"tv":1000},"season":{"tvdb":4,"tmdb":4},"episode_offset":{"tvdb":16,"tmdb":16}},
 {"type":"SPECIAL","kitsu_id":107,"themoviedb_id":{"tv":1000},"season":{"tvdb":4,"tmdb":4},"episode_offset":{"tvdb":28,"tmdb":28}},
 {"type":"MOVIE","kitsu_id":108,"themoviedb_id":{"movie":[5000]},"season":{"tvdb":0,"tmdb":0}},
 {"type":"TV","kitsu_id":"12","themoviedb_id":{"tv":2000}},
 {"type":"TV","kitsu_id":201,"themoviedb_id":{"tv":3000},"season":{"tmdb":1}},
 {"type":"TV","kitsu_id":202,"themoviedb_id":{"tv":3000},"season":{"tmdb":1}},
 {"type":"MOVIE","kitsu_id":301,"themoviedb_id":{"movie":[6000]}},
 {"type":"MOVIE","kitsu_id":302,"themoviedb_id":{"movie":[6000]}},
 {"type":"TV","mal_id":999,"themoviedb_id":{"tv":4000},"season":{"tmdb":1}},
 {"type":"TV","kitsu_id":null,"themoviedb_id":{"tv":"5000"},"season":{"tmdb":1}},
 {"type":"TV","kitsu_id":501,"mal_id":54724,"themoviedb_id":{"tv":226119},"season":{"tvdb":1,"tmdb":1}},
 {"type":"TV","kitsu_id":502,"mal_id":59131,"themoviedb_id":{"tv":226119},"season":{"tvdb":2,"tmdb":1},"episode_offset":{"tmdb":12}},
 {"type":"TV","kitsu_id":503,"mal_id":60608,"themoviedb_id":{"tv":226119},"season":{"tvdb":3,"tmdb":1},"episode_offset":{"tmdb":24}},
 {"type":"TV","kitsu_id":601,"themoviedb_id":{"tv":7000},"season":{"tvdb":"a","tmdb":1}},
 {"type":"TV","kitsu_id":602,"themoviedb_id":{"tv":7000},"season":{"tvdb":"a","tmdb":1},"episode_offset":{"tmdb":12}}
]`

func serve(t *testing.T, body string, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ep(tmdb int64, season, episode, absolute int) core.SearchRequest {
	return core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TMDB: tmdb}, Season: season, Episode: episode, AbsoluteEpisode: absolute}
}

func TestLookup(t *testing.T) {
	srv := serve(t, fixture, nil)
	s := New(Options{URL: srv.URL, Client: srv.Client()})
	for _, tc := range []struct {
		name string
		req  core.SearchRequest
		db   string
		id   int64
		ep   int
		ok   bool
	}{
		{"first season", ep(1000, 1, 5, 5), Kitsu, 101, 5, true},
		{"other databases", ep(1000, 1, 5, 5), MAL, 201, 5, true},
		{"anilist", ep(1000, 1, 5, 5), AniList, 301, 5, true},
		{"anidb", ep(1000, 1, 5, 5), AniDB, 401, 5, true},
		{"season part one", ep(1000, 3, 12, 0), Kitsu, 103, 12, true},
		{"season part two starts after its offset", ep(1000, 3, 13, 0), Kitsu, 104, 1, true},
		{"season part two", ep(1000, 3, 22, 0), Kitsu, 104, 10, true},
		{"a special filed under a season is not an episode", ep(1000, 4, 30, 0), Kitsu, 106, 14, true},
		{"specials have no mapping", ep(1000, 0, 2, 0), Kitsu, 0, 0, false},
		{"unmapped season", ep(1000, 2, 1, 0), Kitsu, 0, 0, false},
		{"whole-series entry numbers absolutely", ep(2000, 21, 180, 1071), Kitsu, 12, 1071, true},
		{"whole-series entry first season", ep(2000, 1, 7, 0), Kitsu, 12, 7, true},
		{"whole-series entry without an absolute count", ep(2000, 21, 180, 0), Kitsu, 0, 0, false},
		{"ambiguous season", ep(3000, 1, 1, 1), Kitsu, 0, 0, false},
		{"database without an id", ep(4000, 1, 1, 1), Kitsu, 0, 0, false},
		{"database with an id", ep(4000, 1, 1, 1), MAL, 999, 1, true},
		{"film", core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 5000}}, Kitsu, 108, 0, true},
		{"ambiguous film", core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 6000}}, Kitsu, 0, 0, false},
		{"unknown title", core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 7}}, Kitsu, 0, 0, false},
		{"no TMDB id", core.SearchRequest{Type: core.TypeMovie}, Kitsu, 0, 0, false},
	} {
		id, episode, ok, err := s.Lookup(context.Background(), tc.req, tc.db)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if id != tc.id || episode != tc.ep || ok != tc.ok {
			t.Errorf("%s: got %d:%d %v, want %d:%d %v", tc.name, id, episode, ok, tc.id, tc.ep, tc.ok)
		}
	}
}

func TestLookupCour(t *testing.T) {
	srv := serve(t, fixture, nil)
	s := New(Options{URL: srv.URL, Client: srv.Client()})
	for _, tc := range []struct {
		name string
		req  core.SearchRequest
		want core.CourMapping
		ok   bool
	}{
		{"single-cour season", ep(1000, 1, 5, 5), core.CourMapping{}, false},
		{"season part one stays cour 1", ep(1000, 3, 12, 0), core.CourMapping{}, false},
		{"season part two first episode with tvdb offset", ep(1000, 3, 13, 0), core.CourMapping{Cour: 2, Episode: 1, TVDBSeason: 3, TVDBEpisode: 13}, true},
		{"season part two tenth episode with tvdb offset", ep(1000, 3, 22, 0), core.CourMapping{Cour: 2, Episode: 10, TVDBSeason: 3, TVDBEpisode: 22}, true},
		{"special offset ignored for cour numbering", ep(1000, 4, 30, 0), core.CourMapping{Cour: 2, Episode: 14, TVDBSeason: 4, TVDBEpisode: 30}, true},
		{"long TMDB season cour 3 maps to TVDB S03E01", ep(226119, 1, 25, 25), core.CourMapping{Cour: 3, Episode: 1, TVDBSeason: 3, TVDBEpisode: 1}, true},
		{"long TMDB season cour 3 sixth episode maps to TVDB S03E06", ep(226119, 1, 30, 30), core.CourMapping{Cour: 3, Episode: 6, TVDBSeason: 3, TVDBEpisode: 6}, true},
		{"non-numeric tvdb season ignored without breaking cour", ep(7000, 1, 13, 13), core.CourMapping{Cour: 2, Episode: 1}, true},
		{"whole-series entry has no cour offset", ep(2000, 21, 180, 1071), core.CourMapping{}, false},
	} {
		got, ok, err := s.LookupCour(context.Background(), tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %+v ok=%v, want %+v ok=%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLoadIsSharedCachedAndBacksOff(t *testing.T) {
	var hits atomic.Int64
	srv := serve(t, fixture, &hits)
	now := time.Unix(1800000000, 0)
	s := New(Options{URL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }})
	for i := 0; i < 3; i++ {
		if _, _, _, err := s.Lookup(context.Background(), ep(1000, 1, 1, 1), Kitsu); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("fetched %d times, want once", hits.Load())
	}
	now = now.Add(25 * time.Hour)
	_, _, _, _ = s.Lookup(context.Background(), ep(1000, 1, 1, 1), Kitsu)
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() != 2 {
		t.Fatalf("stale dataset not refreshed: %d fetches", hits.Load())
	}

	var bad atomic.Int64
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bad.Add(1)
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer broken.Close()
	f := New(Options{URL: broken.URL, Client: broken.Client(), Now: func() time.Time { return now }})
	for i := 0; i < 3; i++ {
		if _, _, ok, err := f.Lookup(context.Background(), ep(1000, 1, 1, 1), Kitsu); !errors.Is(err, ErrUnavailable) || ok {
			t.Fatalf("lookup on a broken source: ok=%v err=%v", ok, err)
		}
	}
	if bad.Load() != 1 {
		t.Fatalf("failed source retried %d times inside the back-off", bad.Load())
	}
}

func TestDatasetBounds(t *testing.T) {
	big := "[" + strings.Repeat(`{"type":"TV","kitsu_id":1,"themoviedb_id":{"tv":1}},`, 2000) + `{"type":"TV"}]`
	srv := serve(t, big, nil)
	s := New(Options{URL: srv.URL, Client: srv.Client(), MaxBytes: 4096})
	if _, _, _, err := s.Lookup(context.Background(), ep(1, 1, 1, 1), Kitsu); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("oversized dataset accepted: %v", err)
	}
	for _, body := range []string{`{"not":"a list"}`, `[{"type":`, `<html>`} {
		srv := serve(t, body, nil)
		s := New(Options{URL: srv.URL, Client: srv.Client()})
		if _, _, _, err := s.Lookup(context.Background(), ep(1, 1, 1, 1), Kitsu); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%q: %v", body, err)
		}
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	s = New(Options{URL: slow.URL, Client: slow.Client(), Timeout: 50 * time.Millisecond})
	if _, _, _, err := s.Lookup(context.Background(), ep(1, 1, 1, 1), Kitsu); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("slow dataset: %v", err)
	}
}

func TestDefaultClientRefusesPrivateAddresses(t *testing.T) {
	srv := serve(t, fixture, nil) // loopback: not a public address
	s := New(Options{URL: srv.URL})
	if _, _, _, err := s.Lookup(context.Background(), ep(1000, 1, 1, 1), Kitsu); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("default client fetched a loopback URL: %v", err)
	}
}

func TestMergedDatasetsFribbWinsAndGapsFilled(t *testing.T) {
	const fribbBody = `[
		{"type":"TV","kitsu_id":101,"mal_id":201,"anilist_id":301,"anidb_id":401,"themoviedb_id":{"tv":1000},"season":{"tvdb":1,"tmdb":1}},
		{"type":"TV","mal_id":999,"themoviedb_id":{"tv":4000},"season":{"tmdb":1}},
		{"type":"TV","anidb_id":710,"anilist_id":1710},
		{"type":"TV","kitsu_id":201,"themoviedb_id":{"tv":3000},"season":{"tmdb":1}},
		{"type":"TV","kitsu_id":202,"themoviedb_id":{"tv":3000},"season":{"tmdb":1}},
		{"type":"MOVIE","kitsu_id":301,"themoviedb_id":{"movie":[6000]}},
		{"type":"MOVIE","kitsu_id":302,"themoviedb_id":{"movie":[6000]}}
	]`
	const xmlBody = `<anime-list>
		<anime anidbid="888" tvdbid="11000" defaulttvdbseason="1" tmdbtv="1000" tmdbseason="1"><name>Conflict S1</name></anime>
		<anime anidbid="401" tvdbid="11000" defaulttvdbseason="1" tmdbtv="99999" tmdbseason="2"><name>Fribb Already Placed</name></anime>
		<anime anidbid="402" tvdbid="11000" defaulttvdbseason="2" tmdbtv="1000" tmdbseason="2"><name>Missing Season 2</name></anime>
		<anime anidbid="710" tvdbid="18100" defaulttvdbseason="1" tmdbtv="8100" tmdbseason="1"><name>Fribb Missing TMDB</name></anime>
		<anime anidbid="901" tvdbid="19000" defaulttvdbseason="1" tmdbtv="9000" tmdbseason="1"><name>New Show Cour 1</name></anime>
		<anime anidbid="902" tvdbid="19000" defaulttvdbseason="2" tmdbtv="9000" tmdbseason="1" tmdboffset="12"><name>New Show Cour 2</name></anime>
		<anime anidbid="950" tvdbid="movie" tmdbid="9500" imdbid="tt0095000"><name>New Movie</name></anime>
		<anime anidbid="960" tvdbid="movie" imdbid="tt0096000"><name>IMDb Only Movie</name></anime>
		<anime anidbid="970" tvdbid="77000" defaulttvdbseason="1"><name>TVDB Single Season</name></anime>
		<anime anidbid="981" tvdbid="78000" defaulttvdbseason="1"><name>TVDB Split S1</name></anime>
		<anime anidbid="982" tvdbid="78000" defaulttvdbseason="2"><name>TVDB Split S2</name></anime>
		<anime anidbid="3001" tvdbid="13000" defaulttvdbseason="1" tmdbtv="3000" tmdbseason="1"><name>Fribb Ambiguous Show</name></anime>
		<anime anidbid="6001" tvdbid="movie" tmdbid="6000"><name>Fribb Ambiguous Film</name></anime>
	</anime-list>`
	const aodbBody = `{
		"license": {"name": "CC0"},
		"data": [
			{"type":"TV","sources":["https://anidb.net/anime/401","https://anilist.co/anime/99999","https://myanimelist.net/anime/201"]},
			{"type":"TV","sources":["https://anidb.net/anime/888","https://anilist.co/anime/8888","https://kitsu.app/anime/8887"]},
			{"type":"TV","sources":["https://myanimelist.net/anime/999","https://anilist.co/anime/4999","https://kitsu.app/anime/4888","https://anidb.net/anime/4777"]},
			{"type":"TV","sources":["https://anidb.net/anime/402","https://anilist.co/anime/1402","https://kitsu.io/anime/2402"]},
			{"type":"TV","sources":["https://anidb.net/anime/901","https://anilist.co/anime/1901","https://myanimelist.net/anime/2901"]},
			{"type":"TV","sources":["https://anidb.net/anime/902","https://anilist.co/anime/1902","https://myanimelist.net/anime/2902"]},
			{"type":"MOVIE","sources":["https://anidb.net/anime/950","https://anilist.co/anime/1950","https://kitsu.app/anime/2950"]},
			{"type":"MOVIE","sources":["https://anidb.net/anime/960","https://anilist.co/anime/1960"]},
			{"type":"TV","sources":["https://anidb.net/anime/970","https://anilist.co/anime/1970"]},
			{"type":"TV","sources":["https://anidb.net/anime/981","https://anilist.co/anime/1981"]},
			{"type":"TV","sources":["https://anidb.net/anime/982","https://anilist.co/anime/1982"]},
			{"type":"TV","sources":["https://anidb.net/anime/3001","https://kitsu.app/anime/3333"]},
			{"type":"MOVIE","sources":["https://anidb.net/anime/6001","https://kitsu.app/anime/6666"]}
		]
	}`

	fribbSrv := serve(t, fribbBody, nil)
	xmlSrv := serve(t, xmlBody, nil)
	aodbSrv := serve(t, aodbBody, nil)

	s := New(Options{
		URL:                fribbSrv.URL,
		AnimeListsURL:      xmlSrv.URL,
		OfflineDatabaseURL: aodbSrv.URL,
		Client:             fribbSrv.Client(),
	})

	for _, tc := range []struct {
		name string
		req  core.SearchRequest
		db   string
		id   int64
		ep   int
		ok   bool
	}{
		{"Fribb wins over conflicting AODB and XML IDs", ep(1000, 1, 5, 5), AniList, 301, 5, true},
		{"Fribb wins on Kitsu over XML S1", ep(1000, 1, 5, 5), Kitsu, 101, 5, true},
		{"Fribb placement prevents XML from relocating anidb 401 to TMDB 99999", ep(99999, 2, 1, 0), AniList, 0, 0, false},
		{"XML + AODB fill unmapped Season 2 on Fribb show", ep(1000, 2, 4, 0), AniList, 1402, 4, true},
		{"AODB fills missing AniList on Fribb MAL-only entry", ep(4000, 1, 2, 2), AniList, 4999, 2, true},
		{"AODB fills missing Kitsu on Fribb MAL-only entry", ep(4000, 1, 2, 2), Kitsu, 4888, 2, true},
		{"XML fills missing TMDB on Fribb AniDB entry while keeping Fribb AniList", ep(8100, 1, 3, 3), AniList, 1710, 3, true},
		{"XML + AODB bridge brand-new multi-cour show cour 1", ep(9000, 1, 12, 12), AniList, 1901, 12, true},
		{"XML + AODB bridge brand-new multi-cour show cour 2", ep(9000, 1, 15, 15), AniList, 1902, 3, true},
		{"XML + AODB bridge brand-new movie by TMDB", core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 9500}}, AniList, 1950, 0, true},
		{"XML + AODB bridge movie by IMDb when TMDB unmapped", core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 9600, IMDB: "tt0096000"}}, AniList, 1960, 0, true},
		{"TVDB-only request resolves via XML + AODB", core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TVDB: 78000}, Season: 2, Episode: 6}, AniList, 1982, 6, true},
		{"unmapped TMDB falls back to single-season TVDB safely", core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TMDB: 77777, TVDB: 77000}, Season: 1, Episode: 4}, AniList, 1970, 4, true},
		{"unmapped TMDB refuses multi-season TVDB guess", core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{TMDB: 78888, TVDB: 78000}, Season: 1, Episode: 15}, AniList, 0, 0, false},
		{"Fribb show ambiguity is not overridden by XML", ep(3000, 1, 1, 1), Kitsu, 0, 0, false},
		{"Fribb film ambiguity is not overridden by XML", core.SearchRequest{Type: core.TypeMovie, IDs: core.IDs{TMDB: 6000}}, Kitsu, 0, 0, false},
	} {
		id, episode, ok, err := s.Lookup(context.Background(), tc.req, tc.db)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if id != tc.id || episode != tc.ep || ok != tc.ok {
			t.Errorf("%s: got %d:%d %v, want %d:%d %v", tc.name, id, episode, ok, tc.id, tc.ep, tc.ok)
		}
	}

	cour, ok, err := s.LookupCour(context.Background(), ep(9000, 1, 15, 15))
	if err != nil {
		t.Fatalf("LookupCour on XML-mapped show: %v", err)
	}
	wantCour := core.CourMapping{Cour: 2, Episode: 3, TVDBSeason: 2, TVDBEpisode: 3}
	if !ok || cour != wantCour {
		t.Fatalf("LookupCour = %+v ok=%v, want %+v ok=true", cour, ok, wantCour)
	}

	// Secondary dataset outage must not break Fribb mappings.
	brokenSecondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(brokenSecondary.Close)
	degraded := New(Options{
		URL:                fribbSrv.URL,
		AnimeListsURL:      brokenSecondary.URL,
		OfflineDatabaseURL: brokenSecondary.URL,
		Client:             fribbSrv.Client(),
	})
	if id, episode, ok, err := degraded.Lookup(context.Background(), ep(1000, 1, 5, 5), AniList); err != nil || !ok || id != 301 || episode != 5 {
		t.Fatalf("degraded secondary lookup: got %d:%d ok=%v err=%v, want 301:5 ok=true", id, episode, ok, err)
	}
}

func TestDurableDiskCacheAndLookupCached(t *testing.T) {
	var hits atomic.Int64
	srv := serve(t, fixture, &hits)
	cacheDir := t.TempDir()
	now := time.Unix(1800000000, 0)

	s1 := New(Options{
		URL:      srv.URL,
		CacheDir: cacheDir,
		Client:   srv.Client(),
		Now:      func() time.Time { return now },
	})

	// Cold LookupCached must never hit the network.
	if _, _, ok := s1.LookupCached(ep(1000, 1, 5, 5), AniList); ok || hits.Load() != 0 {
		t.Fatalf("cold LookupCached: ok=%v hits=%d, want ok=false hits=0", ok, hits.Load())
	}

	// First Lookup populates memory and durable disk cache.
	if id, episode, ok, err := s1.Lookup(context.Background(), ep(1000, 1, 5, 5), AniList); err != nil || !ok || id != 301 || episode != 5 {
		t.Fatalf("initial Lookup: %d:%d ok=%v err=%v", id, episode, ok, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}

	// A new Service reading CacheDir serves LookupCached and Lookup without network I/O.
	s2 := New(Options{
		URL:      srv.URL,
		CacheDir: cacheDir,
		Client:   srv.Client(),
		Now:      func() time.Time { return now.Add(time.Hour) },
	})
	if id, episode, ok := s2.LookupCached(ep(1000, 1, 5, 5), AniList); !ok || id != 301 || episode != 5 {
		t.Fatalf("warm LookupCached from disk: %d:%d ok=%v", id, episode, ok)
	}
	if id, episode, ok, err := s2.Lookup(context.Background(), ep(1000, 1, 5, 5), AniList); err != nil || !ok || id != 301 || episode != 5 {
		t.Fatalf("warm Lookup from disk: %d:%d ok=%v err=%v", id, episode, ok, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("disk cache bypassed: hits = %d, want 1", hits.Load())
	}

	// Past TTL, LookupCached still serves the stale snapshot without network I/O,
	// while Lookup refreshes from upstream.
	now = now.Add(25 * time.Hour)
	if id, episode, ok := s2.LookupCached(ep(1000, 1, 5, 5), AniList); !ok || id != 301 || episode != 5 || hits.Load() != 1 {
		t.Fatalf("stale LookupCached: %d:%d ok=%v hits=%d", id, episode, ok, hits.Load())
	}
	if _, _, ok, err := s2.Lookup(context.Background(), ep(1000, 1, 5, 5), AniList); err != nil || !ok {
		t.Fatalf("expired Lookup refresh: ok=%v err=%v", ok, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() != 2 {
		t.Fatalf("expired Lookup refresh: hits=%d, want hits=2", hits.Load())
	}
}

func TestStaleWhileRevalidateDoesNotBlock(t *testing.T) {
	var hits atomic.Int64
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 2 {
			select {
			case <-block:
			case <-r.Context().Done():
			}
		}
		_, _ = w.Write([]byte(fixture))
	}))
	defer func() {
		close(block)
		srv.Close()
	}()

	now := time.Unix(1800000000, 0)
	s := New(Options{URL: srv.URL, Client: srv.Client(), Now: func() time.Time { return now }, Timeout: 2 * time.Second})
	if id, episode, ok, err := s.Lookup(context.Background(), ep(1000, 1, 5, 5), Kitsu); err != nil || !ok || id != 101 || episode != 5 {
		t.Fatalf("initial Lookup: %d:%d ok=%v err=%v", id, episode, ok, err)
	}

	// Expire TTL and verify Lookup immediately serves stale data without blocking
	// on the slow background refresh.
	now = now.Add(25 * time.Hour)
	start := time.Now()
	id, episode, ok, err := s.Lookup(context.Background(), ep(1000, 1, 5, 5), Kitsu)
	elapsed := time.Since(start)
	if err != nil || !ok || id != 101 || episode != 5 {
		t.Fatalf("stale Lookup: %d:%d ok=%v err=%v", id, episode, ok, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("stale Lookup blocked for %v while refreshing", elapsed)
	}
}
