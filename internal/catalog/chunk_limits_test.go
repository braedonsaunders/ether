package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func newChunkTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := New(store.OpenTest(t), nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func upsertGenreMovies(t *testing.T, ctx context.Context, c *Catalog, n int) []core.Title {
	t.Helper()
	titles := make([]core.Title, 0, n)
	for i := 0; i < n; i++ {
		titles = append(titles, core.Title{
			Key:      core.MovieKey(int64(i + 1)),
			Type:     core.TypeMovie,
			Title:    fmt.Sprintf("Movie %04d", i),
			Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
			Genres:   []string{fmt.Sprintf("genre-%04d", i)},
		})
	}
	if err := c.upsertMany(ctx, titles, upsertOpts{browsable: true}); err != nil {
		t.Fatal(err)
	}
	return titles
}

// Mais itens que qualquer chunk do pacote (200/400/500) e que o limite
// clássico de variáveis do SQLite (999): todos devem voltar hidratados,
// na ordem exata da fatia de entrada.
func TestHydrateSliceBeyondChunkReturnsAllInOrder(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	const n = 1001
	titles := upsertGenreMovies(t, ctx, c, n)

	// Entrada em ordem reversa: hydrate não pode reordenar a fatia.
	items := make([]core.Title, 0, n)
	for i := n - 1; i >= 0; i-- {
		items = append(items, core.Title{Key: titles[i].Key, Type: core.TypeMovie})
	}
	if err := c.hydrateSlice(ctx, items); err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("hydrate returned %d items, want %d", len(items), n)
	}
	for pos, item := range items {
		want := titles[n-1-pos]
		if item.Key != want.Key {
			t.Fatalf("position %d: key %v, want %v (order changed)", pos, item.Key, want.Key)
		}
		if len(item.Genres) != 1 || item.Genres[0] != want.Genres[0] {
			t.Fatalf("position %d (key %v): genres %v, want %v", pos, item.Key, item.Genres, want.Genres)
		}
	}
}

// A faceta genre deve respeitar o mesmo LIMIT 200 de language/studio/country.
func TestFacetGenreLimitedTo200(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	upsertGenreMovies(t, ctx, c, 205)

	vals, err := c.Facet(ctx, core.TypeMovie, "genre")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 200 {
		t.Fatalf("genre facet returned %d values, want 200 (LIMIT 200)", len(vals))
	}
	// Todos com Count=1: desempate por genre ASC.
	if vals[0].Key != "genre-0000" || vals[199].Key != "genre-0199" {
		t.Fatalf("genre facet bounds = %q..%q, want genre-0000..genre-0199", vals[0].Key, vals[199].Key)
	}
	for _, v := range vals {
		if v.Count != 1 {
			t.Fatalf("genre %q count = %d, want 1", v.Key, v.Count)
		}
	}
}
