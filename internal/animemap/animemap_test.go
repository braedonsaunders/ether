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
// into seasons and cours (with a special filed under season 4), a show
// with one whole-series entry, an ambiguous season, and films.
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
 {"type":"TV","kitsu_id":null,"themoviedb_id":{"tv":"5000"},"season":{"tmdb":1}}
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
