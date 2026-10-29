package newznab

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func queries(attempts []map[string][]string) []string {
	var out []string
	for _, a := range attempts {
		if q := a["q"]; len(q) > 0 && a["t"][0] == "search" {
			out = append(out, q[0])
		}
	}
	return out
}

func TestAnimeEpisodesAlsoSearchAbsoluteNumbers(t *testing.T) {
	c := New(config.Indexer{Name: "ix", URL: "https://ix.example", Enabled: true, Protocol: "torrent"}, nil, Options{})
	req := core.SearchRequest{Type: core.TypeEpisode, Title: "One Piece", Season: 21, Episode: 180, AbsoluteEpisode: 1071, Anime: true}
	var got []map[string][]string
	for _, a := range c.searchAttempts(req, nil) {
		got = append(got, a)
	}
	found := false
	for _, q := range queries(got) {
		found = found || q == "One Piece 1071"
	}
	if !found {
		t.Fatalf("no absolute query in %v", queries(got))
	}
	// The same episode without the anime flag searches exactly as before.
	plain := req
	plain.Anime = false
	for _, a := range c.searchAttempts(plain, nil) {
		if q := a.Get("q"); q == "One Piece 1071" {
			t.Fatal("non-anime request searched an absolute number")
		}
	}
	if cacheKey(c, plain) == cacheKey(c, req) {
		t.Fatal("anime request shares the plain request's cache entry")
	}
	plain.AbsoluteEpisode = 0
	if cacheKey(c, plain) != cacheKey(c, core.SearchRequest{Type: core.TypeEpisode, Title: "One Piece", Season: 21, Episode: 180}) {
		t.Fatal("non-anime cache key depends on new fields")
	}
	// Later seasons without a catalogue count are not guessed.
	unknown := req
	unknown.AbsoluteEpisode = 0
	for _, a := range c.searchAttempts(unknown, nil) {
		if q := a.Get("q"); q == "One Piece 180" {
			t.Fatal("season episode searched as an absolute number")
		}
	}
}
