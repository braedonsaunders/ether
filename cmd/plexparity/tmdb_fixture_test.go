package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestTMDBFixtureWithRealClient(t *testing.T) {
	srv := httptest.NewServer(tmdbFixture())
	defer srv.Close()
	client := tmdb.New("fixture", nil, tmdb.WithBaseURL(srv.URL+"/3"), tmdb.WithMaxRetries(0))
	defer client.Close()
	ctx := context.Background()
	movies, err := client.DiscoverMovies(ctx, tmdb.DiscoverParams{})
	if err != nil || len(movies.Results) != 60 {
		t.Fatalf("movies=%v err=%v", movies, err)
	}
	movie, err := client.MovieDetails(ctx, 10003)
	if err != nil || movie.ReleaseDates == nil || len(movie.ReleaseDates.Results) == 0 || movie.ReleaseDates.Results[0].ReleaseDate[0].Certification != "R" {
		t.Fatalf("movie rating missing: %v %v", movie, err)
	}
	show, err := client.TVDetails(ctx, 20001)
	if err != nil || show.Name != "Fixture Show 001" {
		t.Fatalf("show=%v err=%v", show, err)
	}
	season, err := client.SeasonDetails(ctx, 20001, 1)
	if err != nil || len(season.Episodes) != 4 {
		t.Fatalf("season=%v err=%v", season, err)
	}
	resp, err := http.Get(srv.URL + "/3/unimplemented")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
}

func TestTMDBMissingEndpointsAreRecorded(t *testing.T) {
	tracker := &fixtureTracker{}
	srv := httptest.NewServer(trackedTMDBFixture(tracker))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/3/missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if paths := tracker.failures(); len(paths) != 1 || paths[0] != "/3/missing" {
		t.Fatalf("missing paths = %v", paths)
	}
}
