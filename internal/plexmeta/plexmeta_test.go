package plexmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

const movieGUID = "plex://movie/5d7768265af944001f1f6976"

func testService(t *testing.T, h http.Handler, enabled bool) *Service {
	t.Helper()
	db := store.OpenTest(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s := New(db, Options{
		HTTP:    srv.Client(),
		BaseURL: srv.URL,
		Token:   func() string { return "fixture" },
		ClientIdentifier: func() string {
			return "install-id"
		},
		GUID: func(context.Context, core.RatingKey) string { return movieGUID },
		Enabled: func() bool {
			return enabled
		},
		Background: true,
		Now:        func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) },
	})
	t.Cleanup(s.Close)
	return s
}

func TestCachedNeverCallsProvider(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		time.Sleep(time.Second)
	}), true)
	if _, ok := s.Cached(context.Background(), core.MovieKey(550)); ok {
		t.Fatal("empty cache hit")
	}
	if calls.Load() != 0 {
		t.Fatal("Cached talked to Plex")
	}
}

func TestBackgroundFetchAndApply(t *testing.T) {
	s := testService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Client-Identifier") != "install-id" {
			t.Fatalf("client id %q", r.Header.Get("X-Plex-Client-Identifier"))
		}
		if r.URL.Path != "/library/metadata/5d7768265af944001f1f6976" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{
			"guid":"` + movieGUID + `","type":"movie",
			"summary":"Plex summary","tagline":"Plex tagline","year":1999,
			"originallyAvailableAt":"1999-10-15","contentRating":"R",
			"studio":"Plex Studio","thumb":"https://metadata-static.plex.tv/p.jpg",
			"art":"/library/metadata/x/art","audienceRating":9.1,"rating":8.8,
			"Genre":[{"tag":"Drama"}],
			"Role":[{"tag":"Theo Varga","role":"Alex","thumb":"https://image.plex.tv/a.jpg"}]
		}]}}`))
	}), true)
	s.Schedule(core.MovieKey(550))
	s.Wait()
	got, ok := s.Cached(context.Background(), core.MovieKey(550))
	if !ok || got.Summary != "Plex summary" || got.Studio != "Plex Studio" || got.ThumbURL == "" {
		t.Fatalf("cached %+v ok=%v", got, ok)
	}
	title := core.Title{Title: "TMDB", Summary: "TMDB summary", ContentRating: "PG-13", Released: time.Date(1998, 1, 1, 0, 0, 0, 0, time.UTC), Year: 1998}
	released := title.Released
	ApplyDisplay(&title, got.Overlay)
	if title.Summary != "Plex summary" || title.ContentRating != "R" || title.Year != 1999 || !title.Released.Equal(released) {
		t.Fatalf("apply %+v", title)
	}
}

func TestToggleOffDoesNotFetch(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}), false)
	s.Schedule(core.MovieKey(550))
	s.Wait()
	if calls.Load() != 0 {
		t.Fatal("disabled service fetched")
	}
}

func TestStaleWhileRevalidate(t *testing.T) {
	s := testService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + movieGUID + `","type":"movie","summary":"fresh"}]}}`))
	}), true)
	old := Overlay{Summary: "stale"}
	if err := s.store(context.Background(), core.MovieKey(550), movieGUID, old, s.o.Now().Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Cached(context.Background(), core.MovieKey(550))
	if !ok || got.Summary != "stale" || !got.Stale(s.o.Now()) {
		t.Fatalf("stale serve %+v ok=%v", got, ok)
	}
	s.Schedule(core.MovieKey(550))
	s.Wait()
	got, ok = s.Cached(context.Background(), core.MovieKey(550))
	if !ok || got.Summary != "fresh" {
		t.Fatalf("refresh %+v ok=%v", got, ok)
	}
}

func TestBreakerOn429(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}), true)
	s.Schedule(core.MovieKey(550))
	s.Wait()
	if !s.brk.Open() {
		t.Fatal("breaker stayed closed")
	}
	s.Schedule(core.MovieKey(550))
	s.Wait()
	if calls.Load() != 1 {
		t.Fatalf("breaker did not skip refetch: %d", calls.Load())
	}
}

func TestBreakerSuccessKeepsRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	b := breaker{cooldown: time.Minute, threshold: 5, now: func() time.Time { return now }}
	b.Failure(2 * time.Minute)
	if !b.Open() {
		t.Fatal("expected open after 429")
	}
	b.Success()
	if !b.Open() {
		t.Fatal("success cleared Retry-After")
	}
	now = now.Add(2 * time.Minute)
	if b.Open() {
		t.Fatal("still open after Retry-After")
	}
}

func TestImageHostAllowlist(t *testing.T) {
	for _, host := range []string{"metadata-static.plex.tv", "image.plex.tv", "images.plex.tv", "metadata.provider.plex.tv"} {
		if !ImageHost(host) {
			t.Fatalf("rejected %s", host)
		}
	}
	for _, host := range []string{"evil.plex.tv", "cdn.image.plex.tv", "example.com", "metadata.provider.plex.tv.evil.com"} {
		if ImageHost(host) {
			t.Fatalf("allowed %s", host)
		}
	}
}

func TestApplyDisplayKeepsTMDBPeopleAndGenres(t *testing.T) {
	title := core.Title{
		Genres: []string{"Drama"},
		Cast:   []core.Person{{TMDB: 42, Name: "Theo Varga", Role: "Old", ThumbPath: "/tmdb.jpg"}},
	}
	ApplyDisplay(&title, Overlay{
		Genres: []string{"PlexGenre"},
		Cast: []Person{
			{Name: "Theo Varga", Role: "Alex", Thumb: "https://image.plex.tv/a.jpg"},
			{Name: "Only Plex", Role: "Extra"},
		},
	})
	if len(title.Genres) != 1 || title.Genres[0] != "Drama" {
		t.Fatalf("genres %+v", title.Genres)
	}
	if len(title.Cast) != 1 || title.Cast[0].TMDB != 42 || title.Cast[0].Role != "Alex" || title.Cast[0].ThumbPath != "https://image.plex.tv/a.jpg" {
		t.Fatalf("cast %+v", title.Cast)
	}
}

func TestSaferRating(t *testing.T) {
	if got := SaferRating("PG", "R"); got != "R" {
		t.Fatal(got)
	}
	if got := SaferRating("R", "PG"); got != "R" {
		t.Fatal(got)
	}
	if got := SaferRating("PG", ""); got != "PG" {
		t.Fatal(got)
	}
	if contentrating.Exceeds(SaferRating("PG", "R"), "PG") == false {
		t.Fatal("safer R should exceed PG")
	}
}

func TestApplyDisplayDoesNotChangeReleased(t *testing.T) {
	released := time.Date(1999, 10, 15, 0, 0, 0, 0, time.UTC)
	title := core.Title{Released: released, AddedAt: released, Summary: "TMDB"}
	ApplyDisplay(&title, Overlay{Summary: "Plex", AvailableAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Year: 2000})
	if !title.Released.Equal(released) || !title.AddedAt.Equal(released) {
		t.Fatalf("dates changed: %+v", title)
	}
	if title.DateAdded(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) != released {
		t.Fatal("date-added followed Plex")
	}
}
