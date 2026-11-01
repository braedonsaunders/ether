package plexguid

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExactIdentityRequiredAndCached(t *testing.T) {
	for _, tc := range []struct{ name, ids, want string }{{"exact", `[{"id":"tmdb://550"}]`, "plex://movie/5d7768265af944001f1f6976"}, {"mismatch", `[{"id":"tmdb://551"}]`, ""}, {"absent", `[]`, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, e := store.OpenMemory(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Query().Get("guid") != "tmdb://550" || r.Header.Get("X-Plex-Token") != "fixture" {
					t.Fatal("request identity")
				}
				if r.Header.Get("X-Plex-Client-Identifier") != "install-id" {
					t.Fatalf("client id %q", r.Header.Get("X-Plex-Client-Identifier"))
				}
				w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"plex://movie/5d7768265af944001f1f6976","type":"movie","Guid":` + tc.ids + `}]}}`))
			}))
			defer srv.Close()
			s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }, ClientIdentifier: func() string { return "install-id" }})
			for i := 0; i < 2; i++ {
				guid, e := s.Resolve(ctx, core.MovieKey(550))
				if e != nil || guid != tc.want {
					t.Fatal(guid, e)
				}
			}
			if calls != 1 || s.Cached(ctx, core.MovieKey(550)) != tc.want {
				t.Fatal("cache", calls)
			}
		})
	}
}
func TestTokenNotForwardedOnRedirect(t *testing.T) {
	ctx := context.Background()
	db, _ := store.OpenMemory(ctx)
	defer db.Close()
	calls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer srv.Close()
	s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }})
	if _, e := s.Resolve(ctx, core.MovieKey(550)); e == nil || calls != 0 {
		t.Fatal("redirect followed", calls, e)
	}
}

func TestSeasonAndEpisodeResolutionAndCachedMany(t *testing.T) {
	ctx := context.Background()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()

	const (
		showGUID   = "plex://show/5d9c086c46115600200aa2fe"
		seasonGUID = "plex://season/602e5e2194f08e002d001122"
		ep1GUID    = "plex://episode/602e5e2294f08e002d003344"
		ep2GUID    = "plex://episode/602e5e2294f08e002d005566"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/matches":
			if r.URL.Query().Get("type") == "2" && r.URL.Query().Get("guid") == "tmdb://1396" {
				w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + showGUID + `","type":"show","Guid":[{"id":"tmdb://1396"}]}]}}`))
				return
			}
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		case "/library/metadata/5d9c086c46115600200aa2fe/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + seasonGUID + `","type":"season","index":1}]}}`))
		case "/library/metadata/602e5e2194f08e002d001122/children":
			w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + ep1GUID + `","type":"episode","index":1,"parentIndex":1},{"guid":"` + ep2GUID + `","type":"episode","index":2,"parentIndex":1}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }})
	gotEp, err := s.Resolve(ctx, core.EpisodeKey(1396, 1, 1))
	if err != nil || gotEp != ep1GUID {
		t.Fatalf("episode resolve=%q err=%v", gotEp, err)
	}
	batch := s.CachedMany(ctx, []core.RatingKey{
		core.ShowKey(1396),
		core.SeasonKey(1396, 1),
		core.EpisodeKey(1396, 1, 1),
		core.EpisodeKey(1396, 1, 2),
	})
	if batch[core.ShowKey(1396)] != showGUID ||
		batch[core.SeasonKey(1396, 1)] != seasonGUID ||
		batch[core.EpisodeKey(1396, 1, 1)] != ep1GUID ||
		batch[core.EpisodeKey(1396, 1, 2)] != ep2GUID {
		t.Fatalf("unexpected batch cache: %+v", batch)
	}
}

func TestBackgroundScheduleWarmsGUIDs(t *testing.T) {
	ctx := context.Background()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()

	const want = "plex://movie/5d7768265af944001f1f6976"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + want + `","type":"movie","Guid":[{"id":"tmdb://550"}]}]}}`))
	}))
	defer srv.Close()

	s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }, Background: true})
	defer s.Close()

	s.Schedule(core.MovieKey(550))
	s.Wait()

	if got := s.Cached(ctx, core.MovieKey(550)); got != want {
		t.Fatalf("background cached=%q, want %q", got, want)
	}
}
