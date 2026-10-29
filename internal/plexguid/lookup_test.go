package plexguid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLookupVerifiedIdentity(t *testing.T) {
	const movie = "plex://movie/5d7768265af944001f1f6976"
	const show = "plex://show/5d9c086c46115600200aa2fe"
	for _, tc := range []struct {
		name, guid, body string
		want             core.RatingKey
		wantErr          bool
	}{
		{"movie", movie, `{"guid":"` + movie + `","type":"movie","Guid":[{"id":"tmdb://550"}]}`, core.MovieKey(550), false},
		{"show", show, `{"guid":"` + show + `","type":"show","Guid":[{"id":"tmdb://1396"}]}`, core.ShowKey(1396), false},
		{"wrong guid", movie, `{"guid":"plex://movie/5d7768265af944001f1f6977","type":"movie","Guid":[{"id":"tmdb://550"}]}`, 0, true},
		{"wrong type", movie, `{"guid":"` + movie + `","type":"show","Guid":[{"id":"tmdb://550"}]}`, 0, true},
		{"missing external", movie, `{"guid":"` + movie + `","type":"movie"}`, 0, true},
		{"ambiguous", movie, `{"guid":"` + movie + `","type":"movie","Guid":[{"id":"tmdb://550"},{"id":"tmdb://551"}]}`, 0, true},
		{"bad external", movie, `{"guid":"` + movie + `","type":"movie","Guid":[{"id":"tmdb://-1"},{"id":"tmdb://9999999999999999999"},{"id":"tmdb://550/1"}]}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/library/metadata/"+tc.guid[len(tc.guid)-24:] || r.URL.Query().Get("includeGuids") != "1" || r.Header.Get("X-Plex-Token") != "fixture" {
					t.Errorf("unexpected request %s", r.URL)
				}
				fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[%s]}}`, tc.body)
			}))
			defer upstream.Close()
			pg := New(db, Options{BaseURL: upstream.URL, Token: func() string { return "fixture" }})
			defer pg.Close()
			k, err := pg.Lookup(ctx, tc.guid)
			if k != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got %v %v want %v err=%v", k, err, tc.want, tc.wantErr)
			}
			if !tc.wantErr {
				pg.o.Token = nil
				again, e := pg.Lookup(ctx, tc.guid)
				if again != k || e != nil || calls.Load() != 1 || pg.Cached(ctx, k) != tc.guid {
					t.Fatalf("cache=%v err=%v calls=%d", again, e, calls.Load())
				}
			} else if got := pg.Cached(ctx, core.MovieKey(550)); got != "" {
				t.Fatal("unverified identity cached", got)
			}
		})
	}
}

func TestLookupBoundaries(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const guid = "plex://movie/5d7768265af944001f1f6976"
	var calls atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "{}") }))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer upstream.Close()
	pg := New(db, Options{BaseURL: upstream.URL, Token: func() string { return "fixture" }})
	defer pg.Close()
	for _, g := range []string{"", "plex://movie/../../secret", "https://localhost/private", "plex://episode/5d7768265af944001f1f6976"} {
		if _, e := pg.Lookup(ctx, g); !errors.Is(e, core.ErrNotFound) {
			t.Fatalf("invalid %q: %v", g, e)
		}
	}
	if _, e := pg.Lookup(ctx, guid); e == nil || calls.Load() != 0 {
		t.Fatalf("redirect followed: %v %d", e, calls.Load())
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e := pg.Lookup(canceled, guid); !errors.Is(e, context.Canceled) {
		t.Fatalf("canceled lookup: %v", e)
	}
	pg.o.Token = nil
	if _, e := pg.Lookup(ctx, guid); !errors.Is(e, core.ErrNotFound) {
		t.Fatal(e)
	}
	if e := pg.storeMapping(ctx, core.MovieKey(550), guid, time.Now().Add(-2*cacheTTL).Unix()); e != nil {
		t.Fatal(e)
	}
	if _, e := pg.Lookup(ctx, guid); !errors.Is(e, core.ErrNotFound) {
		t.Fatalf("expired cache accepted: %v", e)
	}
	if e := pg.storeMapping(ctx, core.MovieKey(550), guid, time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	if e := pg.storeMapping(ctx, core.MovieKey(551), guid, time.Now().Unix()); e != nil {
		t.Fatal(e)
	}
	if _, e := pg.Lookup(ctx, guid); e == nil {
		t.Fatal("ambiguous cache accepted")
	}
}

func TestLookupMissingProviderIdentity(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	pg := New(db, Options{BaseURL: upstream.URL, Token: func() string { return "fixture" }})
	defer pg.Close()
	if k, err := pg.Lookup(ctx, "plex://movie/5d7768265af944001f1f6976"); k != 0 || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown identity=%v %v", k, err)
	}
}
