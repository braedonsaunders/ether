package catalog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestPersonIMDbPersistsAcrossCreditReplacement(t *testing.T) {
	var details atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/person/777/combined_credits":
			fmt.Fprint(w, `{"id":777,"cast":[{"id":601,"media_type":"movie","title":"Credit","release_date":"2020-01-01","popularity":50,"vote_count":500,"character":"Lead"}]}`)
		case "/person/777":
			details.Add(1)
			fmt.Fprint(w, `{"id":777,"name":"Actor","imdb_id":"nm0000138"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	db := store.OpenTest(t)
	client := tmdb.New("fake", slog.Default(), tmdb.WithBaseURL(srv.URL), tmdb.WithCacheTTL(0))
	t.Cleanup(client.Close)
	c, err := New(db, client, config.Catalog{MinPopularity: 1, MinVotes: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Person: 777}})
	if err != nil || len(page.Items) != 1 || len(page.Items[0].Cast) != 1 || page.Items[0].Cast[0].IMDB != "nm0000138" || details.Load() != 1 {
		t.Fatalf("hydrated person: %+v details=%d err=%v", page, details.Load(), err)
	}
	key := core.MovieKey(601)
	if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, Title: "Credit", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Cast: []core.Person{{TMDB: 777, Name: "Actor", Role: "Lead"}}}, true); err != nil {
		t.Fatal(err)
	}
	cached, err := c.CachedGet(ctx, key)
	if err != nil || len(cached.Cast) != 1 || cached.Cast[0].IMDB != "nm0000138" {
		t.Fatalf("cached replacement: %+v %v", cached, err)
	}
	mirror, err := NewMirror(db, time.Now).Titles(ctx, []core.RatingKey{key})
	if err != nil || len(mirror) != 1 || len(mirror[0].Cast) != 1 || mirror[0].Cast[0].IMDB != "nm0000138" || details.Load() != 1 {
		t.Fatalf("mirror or unexpected fetch: %+v details=%d err=%v", mirror, details.Load(), err)
	}
}

func TestValidPersonIMDB(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{{"nm0000138", true}, {"nm123456789012", true}, {"tt0000138", false}, {"nm0000000", false}, {"nm123", false}, {"nm1234567/x", false}, {"", false}} {
		if got := validPersonIMDB(tc.id); got != tc.valid {
			t.Errorf("%q: %v", tc.id, got)
		}
	}
}
