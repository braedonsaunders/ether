package main

import (
	"context"
	"image"
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
	list, err := client.MovieList(ctx, "popular", 1)
	if err != nil || len(list.Results) != 60 {
		t.Fatalf("movie list=%v err=%v", list, err)
	}
	multi, err := client.SearchMulti(ctx, "Fixture", 1)
	if err != nil || len(multi.Results) != 80 {
		t.Fatalf("multi search count/error: %v %v", multi, err)
	}
	ids, err := client.EpisodeExternalIDs(ctx, 20001, 1, 2)
	if err != nil || ids == nil {
		t.Fatalf("episode IDs=%v err=%v", ids, err)
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
	resp, err = http.Get(srv.URL + "/3/images/w500/missing.jpg")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown image status = %d", resp.StatusCode)
	}
	if paths := tracker.failures(); len(paths) != 2 || paths[0] != "/3/images/w500/missing.jpg" {
		t.Fatalf("missing paths = %v", paths)
	}
}

func TestTMDBFixtureServesRealImages(t *testing.T) {
	srv := httptest.NewServer(tmdbFixture())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/3/images/w500/fixture.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("image response: %v", resp.Status)
	}
	im, format, err := image.Decode(resp.Body)
	if err != nil || format != "jpeg" || im.Bounds().Dx() != 320 || im.Bounds().Dy() != 180 {
		t.Fatalf("image=%v format=%s error=%v", im, format, err)
	}
}
