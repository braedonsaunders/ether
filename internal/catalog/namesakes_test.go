package catalog

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// seedNamesakeTitle writes a released movie/show row the Namesakes lookup reads.
func seedNamesakeTitle(t *testing.T, c *Catalog, key core.RatingKey, typ core.MediaType, title string, year int) {
	t.Helper()
	released := time.Date(year, 3, 1, 0, 0, 0, 0, time.UTC)
	err := c.upsert(context.Background(), core.Title{
		Key: key, Type: typ, Title: title, Year: year, Released: released, Runtime: 90 * time.Minute,
	}, upsertOpts{})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNamesakesArticleAndYearWindow is the catalogue-side invariant: a
// namesake is a same-type title whose normalized name matches ("The Runner"
// vs "Runner") and whose year is within one; a year gap or a different
// normalized name is not.
func TestNamesakesArticleAndYearWindow(t *testing.T) {
	ctx := context.Background()
	c, err := New(store.OpenTest(t), nil, config.Catalog{}, slog.New(slog.DiscardHandler),
		WithNow(func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }))
	if err != nil {
		t.Fatal(err)
	}
	seedNamesakeTitle(t, c, core.MovieKey(1), core.TypeMovie, "Runner", 2026)
	seedNamesakeTitle(t, c, core.MovieKey(2), core.TypeMovie, "The Runner", 2026) // article-only
	seedNamesakeTitle(t, c, core.MovieKey(3), core.TypeMovie, "Runner", 1998)     // year gap
	seedNamesakeTitle(t, c, core.ShowKey(4), core.TypeShow, "Runner", 2026)       // other type
	seedNamesakeTitle(t, c, core.MovieKey(5), core.TypeMovie, "Runner 2", 2026)   // different normalized title

	for _, tc := range []struct {
		key  core.RatingKey
		want bool
	}{
		{core.MovieKey(1), true},  // "The Runner" 2026 namesakes it
		{core.MovieKey(2), true},  // "Runner" 2026 namesakes it
		{core.MovieKey(3), false}, // year gap beyond +-1
		{core.ShowKey(4), false},  // a show is not a movie namesake
		{core.MovieKey(5), false}, // "Runner 2" is a different title
	} {
		got, err := c.Namesakes(ctx, tc.key)
		if err != nil {
			t.Fatalf("key %d: %v", tc.key, err)
		}
		if got != tc.want {
			t.Errorf("key %d: namesake = %v, want %v", tc.key, got, tc.want)
		}
	}
}
