package collections_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/store"
)

type describeChat struct {
	calls atomic.Int32
	block chan struct{}
	fail  atomic.Bool
	genre string
}

func (c *describeChat) Enabled() bool { return true }
func (c *describeChat) Chat(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
	c.calls.Add(1)
	if c.block != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.block:
		}
	}
	if c.fail.Load() {
		return "", errors.New("offline")
	}
	b, _ := json.Marshal(map[string]any{"spec": map[string]any{"name": "Generated picks", "mediaType": "mixed", "sources": []map[string]any{{"kind": "catalog"}}, "match": "all", "rules": []map[string]any{{"field": "genre", "op": "in", "value": []string{c.genre}}, {"field": "votes", "op": "gte", "value": 20}}, "sort": "popularity", "sortDesc": true, "limit": 200}})
	return string(b), nil
}

type describeCatalog struct {
	core.Catalog
	calls atomic.Int32
}

func (c *describeCatalog) DescribeCandidates(ctx context.Context, sp core.CollectionSpec, n int) ([]core.Title, error) {
	c.calls.Add(1)
	return c.Catalog.(core.DescriptiveCatalog).DescribeCandidates(ctx, sp, n)
}

func TestDescriptiveSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat, err := catalog.New(db, nil, config.Catalog{Language: "en-US", Region: "US"}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	for i, g := range []string{"Comedy", "Romance", "Family", "Animation", "Horror", "Thriller", "Mystery", "Drama", "Adventure", "War", "Fantasy", "Science Fiction", "Action", "Music"} {
		title := core.Title{Key: core.MovieKey(int64(i + 1)), IDs: core.IDs{TMDB: int64(i + 1)}, Type: core.TypeMovie, Title: g + " fixture", Genres: []string{g}, Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Year: 2000, VoteCount: 100, Popularity: 100, Rating: 8, ContentRating: "PG"}
		if err := cat.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	counted := &describeCatalog{Catalog: cat}
	svc, err := collections.New(db, counted, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if svc.DescriptiveSearchAvailable() {
		t.Fatal("advertised without AI")
	}
	disabled, err := svc.DescribeSearch(ctx, nil, "cozy cute girls night in", true)
	if err != nil || disabled.State != "unavailable" || counted.calls.Load() != 0 {
		t.Fatalf("disabled: %+v %v", disabled, err)
	}
	cases := []struct{ q, g string }{{"cozy cute girls night in", "Romance"}, {"scary creepy night", "Horror"}, {"rainy night", "Mystery"}, {"epic adventure", "Adventure"}, {"mind bending", "Science Fiction"}, {"date night", "Romance"}, {"family movie night", "Family"}, {"background easy watching", "Comedy"}, {"tearjerker", "Drama"}, {"adrenaline", "Action"}, {"80s sci-fi under 2 hours", "Science Fiction"}}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			chat := &describeChat{genre: tc.g, block: make(chan struct{})}
			svc.SetLLM(chat)
			before := counted.calls.Load()
			pending, e := svc.DescribeSearch(ctx, &core.User{ID: 7}, tc.q, false)
			if e != nil || pending.State != "pending" {
				t.Fatalf("pending: %+v %v", pending, e)
			}
			close(chat.block)
			ready, e := svc.DescribeSearch(ctx, &core.User{ID: 7}, tc.q, true)
			if e != nil || ready.State != "ready" || len(ready.Items) == 0 {
				t.Fatalf("ready: %+v %v", ready, e)
			}
			if ready.Items[0].Genres[0] != tc.g {
				t.Fatalf("wrong genre: %+v", ready.Items)
			}
			_, e = svc.DescribeSearch(ctx, &core.User{ID: 7}, "  "+strings.ToUpper(tc.q)+"  ", true)
			if e != nil || chat.calls.Load() != 1 || counted.calls.Load() != before+1 {
				t.Fatalf("cache/model/query bound: model %d queries %d err %v", chat.calls.Load(), counted.calls.Load()-before, e)
			}
			c, items, ok, e := svc.VibeCollection(ctx, &core.User{ID: 7}, tc.q)
			if e != nil || !ok || !c.Key.IsEphemeral() || len(items) == 0 {
				t.Fatalf("protocol bridge %+v %v %v", c, ok, e)
			}
			if _, e := svc.Get(ctx, &core.User{ID: 8}, c.Key); !errors.Is(e, core.ErrNotFound) {
				t.Fatalf("other viewer: %v", e)
			}
			t.Logf("%s -> %s", tc.q, ready.Items[0].Title)
		})
	}
	failed := &describeChat{genre: "Comedy"}
	failed.fail.Store(true)
	svc.SetLLM(failed)
	for i := 0; i < 2; i++ {
		result, e := svc.DescribeSearch(ctx, nil, "unavailable model query", true)
		if e == nil || result.State != "failed" {
			t.Fatalf("failure: %+v %v", result, e)
		}
	}
	if failed.calls.Load() != 1 {
		t.Fatal("failed model was retried without backoff")
	}
}
