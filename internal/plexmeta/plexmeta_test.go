package plexmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestShowChildrenPaged(t *testing.T) {
	const showGUID = "plex://show/5d9c086c46115600200aa2fe"
	s := testService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/library/metadata/5d9c086c46115600200aa2fe":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + showGUID + `","type":"show","summary":"Show"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/children"):
			start := r.Header.Get("X-Plex-Container-Start")
			if start == "0" {
				w.Write([]byte(`{"MediaContainer":{"size":1,"totalSize":2,"Metadata":[{"guid":"plex://season/aaaaaaaaaaaaaaaaaaaaaaaa","type":"season","index":1,"summary":"S1"}]}}`))
				return
			}
			w.Write([]byte(`{"MediaContainer":{"size":1,"totalSize":2,"Metadata":[{"guid":"plex://season/bbbbbbbbbbbbbbbbbbbbbbbb","type":"season","index":2,"summary":"S2"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}), true)
	s.o.GUID = func(_ context.Context, k core.RatingKey) string {
		if k == core.ShowKey(1396) {
			return showGUID
		}
		return ""
	}
	s.Schedule(core.ShowKey(1396))
	s.Wait()
	s1, ok := s.Cached(context.Background(), core.SeasonKey(1396, 1))
	if !ok || s1.Summary != "S1" {
		t.Fatalf("season 1 %+v ok=%v", s1, ok)
	}
	s2, ok := s.Cached(context.Background(), core.SeasonKey(1396, 2))
	if !ok || s2.Summary != "S2" {
		t.Fatalf("season 2 %+v ok=%v", s2, ok)
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
