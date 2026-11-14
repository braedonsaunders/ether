package collections_test

import (
	"context"
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
	"github.com/braedonsaunders/ether/internal/store"
)

func TestParseVibePositives(t *testing.T) {
	cases := []struct {
		q     string
		title string
		check func(core.VibeQuery) bool
	}{
		{"90s thrillers under 100 mins", "90s Thrillers Under 100 Minutes", func(v core.VibeQuery) bool {
			f := v.Filter
			return f.YearFrom == 1990 && f.YearTo == 1999 && f.MaxRuntime == 100*time.Minute &&
				len(f.Genres) == 1 && f.Genres[0] == "Thriller"
		}},
		{"feel-good 80s comedies", "Feel-Good 80s Comedies", func(v core.VibeQuery) bool {
			return v.Filter.YearFrom == 1980 && len(v.Filter.Genres) >= 2
		}},
		{"korean horror", "Korean Horror", func(v core.VibeQuery) bool {
			return len(v.Filter.Languages) == 1 && v.Filter.Languages[0] == "ko" &&
				len(v.Filter.Countries) == 2 && v.Filter.Genres[0] == "Horror"
		}},
		{"highly rated sci-fi from the 2010s", "Highly Rated 2010s Sci-Fi", func(v core.VibeQuery) bool {
			return v.Filter.MinRating == 7.5 && v.Sort == core.SortRating && v.Filter.YearFrom == 2010
		}},
		{"short animated movies", "Animated Movies Under 100 Minutes", func(v core.VibeQuery) bool {
			return v.Filter.Type == core.TypeMovie && v.Filter.MaxRuntime == 100*time.Minute &&
				v.Filter.Genres[0] == "Animation"
		}},
		{"shows like the office", "Shows Like The Office", func(v core.VibeQuery) bool {
			return v.SimilarSeed == "the office" && v.Filter.Type == core.TypeShow
		}},
		{"christopher nolan films", "Christopher Nolan Movies", func(v core.VibeQuery) bool {
			return v.PersonName == "christopher nolan" && v.Filter.Type == core.TypeMovie
		}},
		{"a24 movies", "A24 Movies", func(v core.VibeQuery) bool {
			return v.Filter.Studio == "A24"
		}},
		{"scary movies under 90 minutes", "", func(v core.VibeQuery) bool {
			return v.Filter.Genres[0] == "Horror" && v.Filter.MaxRuntime == 90*time.Minute
		}},
		{"rom-coms from 2015", "", func(v core.VibeQuery) bool { return v.Filter.YearFrom == 2015 }},
		{"war movies before 2000", "", func(v core.VibeQuery) bool { return v.Filter.YearTo == 1999 }},
		{"movies between 1990 and 1995", "", func(v core.VibeQuery) bool {
			return v.Filter.YearFrom == 1990 && v.Filter.YearTo == 1995
		}},
		{"< 2 hours of horror", "", func(v core.VibeQuery) bool {
			return v.Filter.MaxRuntime == 2*time.Hour
		}},
		{"acclaimed french dramas", "", func(v core.VibeQuery) bool {
			return v.Filter.MinRating == 7.5 && v.Filter.Languages[0] == "fr"
		}},
		{"films directed by denis villeneuve", "", func(v core.VibeQuery) bool {
			return v.PersonName == "denis villeneuve"
		}},
		{"90's action movies", "", func(v core.VibeQuery) bool { return v.Filter.YearFrom == 1990 }},
		{"latest crime shows", "", func(v core.VibeQuery) bool {
			return v.Sort == core.SortReleased && v.Filter.Type == core.TypeShow
		}},
		{"anime series", "", func(v core.VibeQuery) bool {
			return v.Filter.Languages[0] == "ja" && v.Filter.Type == core.TypeShow
		}},
		{"over 2 hours long dramas", "", func(v core.VibeQuery) bool {
			return v.Filter.MinRuntime == 2*time.Hour && v.Filter.MaxRuntime == 0
		}},
		{"best documentaries", "", func(v core.VibeQuery) bool { return v.Filter.MinRating == 8 }},
	}
	for _, tc := range cases {
		v, ok := collections.ParseVibe(tc.q)
		if !ok {
			t.Errorf("ParseVibe(%q): not a vibe, want vibe", tc.q)
			continue
		}
		if tc.title != "" && v.Title != tc.title {
			t.Errorf("ParseVibe(%q) title = %q, want %q", tc.q, v.Title, tc.title)
		}
		if tc.check != nil && !tc.check(v) {
			t.Errorf("ParseVibe(%q) = %+v, check failed", tc.q, v)
		}
	}
}

func TestParseVibeNegatives(t *testing.T) {
	for _, q := range []string{
		"", "inception", "the office", "breaking bad", "star wars",
		"the dark knight", "dark", "top gun", "christmas movies",
		"movies", "shows", "alien invasion", "horror of dracula",
		"the", "movie night", "something to watch", "dune part two",
		"neon", "pixar", "the godfather", "war and peace",
	} {
		if v, ok := collections.ParseVibe(q); ok {
			t.Errorf("ParseVibe(%q) = %+v, want not-a-vibe", q, v)
		}
	}
}

// vibeSetup is setup with a controllable clock and no seeded defaults.
func vibeSetup(t *testing.T, now *atomic.Int64, opts ...collections.Option) *collections.Service {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Catalog{Language: "en-US", Region: "US"}
	cat, err := catalog.New(db, nil, cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	seed := []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "Alien Invasion",
			Genres: []string{"Science Fiction"}, Year: 1985, Released: time.Date(1985, 6, 1, 0, 0, 0, 0, time.UTC),
			Runtime: 100 * time.Minute, Rating: 8.5, VoteCount: 100, Popularity: 50, ContentRating: "PG-13", PosterPath: "/a.jpg"},
		{Key: core.MovieKey(2), Type: core.TypeMovie, IDs: core.IDs{TMDB: 2}, Title: "Saga Returns",
			Genres: []string{"Science Fiction"}, Year: 1987, Released: time.Date(1987, 6, 1, 0, 0, 0, 0, time.UTC),
			Runtime: 110 * time.Minute, Rating: 8.1, VoteCount: 100, Popularity: 40, ContentRating: "R"},
		{Key: core.MovieKey(3), Type: core.TypeMovie, IDs: core.IDs{TMDB: 3}, Title: "Modern Drama",
			Genres: []string{"Drama"}, Year: 2020, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			Runtime: 95 * time.Minute, Rating: 9.0, VoteCount: 100, Popularity: 60, ContentRating: "PG"},
	}
	for _, title := range seed {
		if err := cat.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	all := []collections.Option{collections.WithIndexTTL(0), collections.WithLiveTTL(0)}
	if now != nil {
		all = append(all, collections.WithNow(func() time.Time { return time.Unix(now.Load(), 0) }))
	}
	svc, err := collections.New(db, cat, slog.Default(), append(all, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestVibeEphemeralLifecycle(t *testing.T) {
	ctx := context.Background()
	svc := vibeSetup(t, nil)
	user := &core.User{ID: 7}
	other := &core.User{ID: 8}

	c, items, ok, err := svc.VibeCollection(ctx, user, "80s sci-fi movies")
	if err != nil || !ok || c == nil {
		t.Fatalf("VibeCollection: ok=%v err=%v c=%+v", ok, err, c)
	}
	if !c.Key.IsEphemeral() || c.ChildCount != 2 || len(items) != 2 {
		t.Fatalf("ephemeral: %+v items=%d", c, len(items))
	}
	// Same query again: same collection.
	c2, _, ok, _ := svc.VibeCollection(ctx, user, "80s sci-fi movies")
	if !ok || c2.Key != c.Key {
		t.Fatalf("dedupe: %v vs %v", c2.Key, c.Key)
	}
	// Plain title search is not a vibe.
	if _, _, ok, _ := svc.VibeCollection(ctx, user, "Alien Invasion"); ok {
		t.Fatal("title search became a vibe")
	}
	// Owner-only Get/Items/List.
	got, err := svc.Get(ctx, user, c.Key)
	if err != nil || got.Title != c.Title || got.ChildCount != 2 {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if _, err := svc.Get(ctx, other, c.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other Get: %v", err)
	}
	if _, err := svc.Items(ctx, other, c.Key, 0, 10); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other Items: %v", err)
	}
	page, err := svc.Items(ctx, user, c.Key, 0, 10)
	if err != nil || page.Total != 2 {
		t.Fatalf("Items: %+v %v", page, err)
	}
	list, err := svc.List(ctx, user, core.TypeMovie)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lc := range list {
		if lc.Key == c.Key {
			found = true
		}
	}
	if !found {
		t.Fatalf("List missing ephemeral: %v", list)
	}

	// Promotion persists it as a personal smart collection; the old
	// ephemeral key keeps working as an alias.
	saved, ok, err := svc.PromoteEphemeral(ctx, user, c.Key)
	if err != nil || !ok || saved == nil {
		t.Fatalf("Promote: ok=%v err=%v", ok, err)
	}
	if saved.Key.IsEphemeral() || saved.Title != c.Title || saved.OwnerUserID != 7 {
		t.Fatalf("saved: %+v", saved)
	}
	alias, err := svc.Get(ctx, user, c.Key)
	if err != nil || alias.Key != saved.Key {
		t.Fatalf("alias Get: %+v %v", alias, err)
	}
	page, err = svc.Items(ctx, user, c.Key, 0, 10)
	if err != nil || page.Total != 2 {
		t.Fatalf("alias Items: %+v %v", page, err)
	}
	if _, err := svc.Get(ctx, other, saved.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other saved Get: %v", err)
	}
	// Promoting twice is idempotent.
	again, ok, err := svc.PromoteEphemeral(ctx, user, c.Key)
	if err != nil || !ok || again.Key != saved.Key {
		t.Fatalf("re-promote: %+v ok=%v %v", again, ok, err)
	}
}

func TestVibeEphemeralRestrictionsTTLAndLRU(t *testing.T) {
	ctx := context.Background()
	var now atomic.Int64
	now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	svc := vibeSetup(t, &now, collections.WithEphemeralLimits(time.Hour, 2))

	kid := &core.User{ID: 9, MaxContentRating: "PG-13"}
	c, items, ok, err := svc.VibeCollection(ctx, kid, "80s sci-fi")
	if err != nil || !ok {
		t.Fatalf("vibe: ok=%v %v", ok, err)
	}
	if len(items) != 1 || items[0].Title != "Alien Invasion" || c.ChildCount != 1 {
		t.Fatalf("restricted items: %+v count=%d", items, c.ChildCount)
	}

	user := &core.User{ID: 7}
	first, _, _, _ := svc.VibeCollection(ctx, user, "80s sci-fi")
	second, _, _, _ := svc.VibeCollection(ctx, user, "modern dramas")
	third, _, _, _ := svc.VibeCollection(ctx, user, "sci-fi under 105 mins")
	if first == nil || second == nil || third == nil {
		t.Fatal("expected three vibes")
	}
	// LRU max=2 across the registry (kid's + 3 = evictions happened);
	// the most recent must survive, retired ones 404.
	if _, err := svc.Get(ctx, user, third.Key); err != nil {
		t.Fatalf("newest ephemeral lost: %v", err)
	}
	// TTL: everything unpromoted dies after an hour.
	now.Add(int64(2 * time.Hour / time.Second))
	if _, err := svc.Get(ctx, user, third.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("TTL Get: %v", err)
	}
}

type stubRefiner struct{ calls atomic.Int32 }

func (s *stubRefiner) RefineVibe(_ context.Context, _ *core.User, q string) (core.VibeQuery, bool, error) {
	s.calls.Add(1)
	if !strings.Contains(q, "hug") {
		return core.VibeQuery{}, false, nil
	}
	return core.VibeQuery{
		Title: "Warm Hugs", Sort: core.SortRating, Desc: true,
		Filter: core.Filter{Type: core.TypeMovie, Genres: []string{"Drama"}},
	}, true, nil
}

func TestVibeRefinerFallback(t *testing.T) {
	ctx := context.Background()
	ref := &stubRefiner{}
	svc := vibeSetup(t, nil, collections.WithVibeRefiner(ref))
	user := &core.User{ID: 7}

	c, items, ok, err := svc.VibeCollection(ctx, user, "movies that feel like a warm hug")
	if err != nil || !ok || c.Title != "Warm Hugs" || len(items) != 1 {
		t.Fatalf("refined vibe: %+v items=%v ok=%v %v", c, items, ok, err)
	}
	// The deterministic parser wins without consulting the refiner.
	before := ref.calls.Load()
	if _, _, ok, _ := svc.VibeCollection(ctx, user, "80s sci-fi"); !ok {
		t.Fatal("deterministic vibe lost")
	}
	if ref.calls.Load() != before {
		t.Fatal("refiner consulted for a parseable query")
	}
	local := core.WithLocalResponse(ctx)
	for _, query := range []string{"movies that feel like a warm hug", "christopher nolan films", "shows like the office"} {
		if _, _, ok, err := svc.VibeCollection(local, user, query); err != nil || ok {
			t.Fatalf("local read attempted model or external resolution for %q: handled=%v err=%v", query, ok, err)
		}
	}
	if _, items, ok, err := svc.VibeCollection(local, user, "80s sci-fi"); err != nil || !ok || len(items) == 0 {
		t.Fatalf("local structured query lost: items=%v handled=%v err=%v", items, ok, err)
	}
	if ref.calls.Load() != before {
		t.Fatal("local response consulted the model refiner")
	}
}
