package plexguid

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestPerInstallClientIdentifier(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Plex-Client-Identifier")
		w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"plex://movie/5d7768265af944001f1f6976","type":"movie","Guid":[{"id":"tmdb://550"}]}]}}`))
	}))
	defer srv.Close()

	const machineID = "0123456789abcdef0123456789abcdef01234567"
	s := New(db, Options{
		BaseURL:          srv.URL,
		Token:            func() string { return "fixture" },
		ClientIdentifier: func() string { return ClientIdentifier("", machineID) },
	})
	if _, err := s.Resolve(ctx, core.MovieKey(550)); err != nil {
		t.Fatal(err)
	}
	if gotHeader != machineID {
		t.Fatalf("X-Plex-Client-Identifier = %q, want %q", gotHeader, machineID)
	}
	if gotHeader == "ether-metadata" {
		t.Fatal("used fixed shared client identifier")
	}

	// Two unconfigured instances must still receive distinct client identifiers.
	a := New(db, Options{BaseURL: srv.URL}).ClientIdentifier()
	b := New(db, Options{BaseURL: srv.URL}).ClientIdentifier()
	if a == "" || b == "" || a == "ether-metadata" || a == b {
		t.Fatalf("fallback ClientIdentifier should be unique per instance: a=%q b=%q", a, b)
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

func TestChildrenPaging120Episodes(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		showGUID   = "plex://show/5d9c086c46115600200aa2fe"
		seasonGUID = "plex://season/602e5e2194f08e002d001122"
		totalEps   = 120
	)
	epGUID := func(n int) string {
		return fmt.Sprintf("plex://episode/602e5e2294f08e002d%06x", n)
	}

	var epPageStarts []int
	var epPageSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/matches":
			if r.URL.Query().Get("type") == "2" && r.URL.Query().Get("guid") == "tmdb://456" {
				w.Write([]byte(`{"MediaContainer":{"Metadata":[{"guid":"` + showGUID + `","type":"show","Guid":[{"id":"tmdb://456"}]}]}}`))
				return
			}
			w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		case "/library/metadata/5d9c086c46115600200aa2fe/children":
			if r.URL.Query().Get("X-Plex-Container-Size") != "50" || r.URL.Query().Get("X-Plex-Container-Start") != "0" {
				t.Fatalf("season children query = %v", r.URL.Query())
			}
			w.Write([]byte(`{"MediaContainer":{"totalSize":1,"Metadata":[{"guid":"` + seasonGUID + `","type":"season","index":1}]}}`))
		case "/library/metadata/602e5e2194f08e002d001122/children":
			start, err1 := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
			size, err2 := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
			if err1 != nil || err2 != nil || size != 50 {
				t.Fatalf("episode children paging params start=%q size=%q", r.URL.Query().Get("X-Plex-Container-Start"), r.URL.Query().Get("X-Plex-Container-Size"))
			}
			epPageStarts = append(epPageStarts, start)
			epPageSizes = append(epPageSizes, size)
			end := min(start+size, totalEps)
			type item struct {
				GUID        string `json:"guid"`
				Type        string `json:"type"`
				Index       int    `json:"index"`
				ParentIndex int    `json:"parentIndex"`
			}
			var items []item
			for idx := start + 1; idx <= end; idx++ {
				items = append(items, item{
					GUID:        epGUID(idx),
					Type:        "episode",
					Index:       idx,
					ParentIndex: 1,
				})
			}
			resp := map[string]any{
				"MediaContainer": map[string]any{
					"totalSize": totalEps,
					"size":      len(items),
					"Metadata":  items,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }})
	got120, err := s.Resolve(ctx, core.EpisodeKey(456, 1, 120))
	if err != nil || got120 != epGUID(120) {
		t.Fatalf("resolve episode 120 = %q, err = %v, want %q", got120, err, epGUID(120))
	}
	if len(epPageStarts) != 3 || epPageStarts[0] != 0 || epPageStarts[1] != 50 || epPageStarts[2] != 100 {
		t.Fatalf("episode children page starts = %v, sizes = %v, want [0 50 100] with size 50", epPageStarts, epPageSizes)
	}
	for _, ep := range []int{1, 50, 51, 100, 101, 120} {
		if cached := s.Cached(ctx, core.EpisodeKey(456, 1, ep)); cached != epGUID(ep) {
			t.Fatalf("cached episode %d = %q, want %q", ep, cached, epGUID(ep))
		}
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
