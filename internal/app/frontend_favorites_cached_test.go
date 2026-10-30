package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"testing"
)

func TestFavoriteTitlesCachedInheritedPolicy(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, MaxContentRating: "PG"}
	show := core.Title{Key: core.ShowKey(4), Type: core.TypeShow, ContentRating: "TV-MA"}
	episode := core.Title{Key: core.EpisodeKey(4, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	lookup := map[core.RatingKey]core.Title{episode.Key: episode, show.Key: show}
	cached := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		out := map[core.RatingKey]core.Title{}
		for _, k := range keys {
			if v, ok := lookup[k]; ok {
				out[k] = v
			}
		}
		return out, nil
	}
	vis := experience.Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }}
	if ok, err := favoriteTitlesWithCache(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("restricted parent allowed: %v %v", ok, err)
	}
	delete(lookup, show.Key)
	if ok, err := favoriteTitlesWithCache(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("missing parent allowed: %v %v", ok, err)
	}
	show.ContentRating = "TV-G"
	lookup[show.Key] = show
	if ok, err := favoriteTitlesWithCache(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || !ok {
		t.Fatalf("visible item denied: %v %v", ok, err)
	}
	viewer.AllowedSections = []int{1}
	if ok, err := favoriteTitlesWithCache(ctx, viewer, []core.RatingKey{episode.Key}, vis, cached); err != nil || ok {
		t.Fatalf("revoked section allowed: %v %v", ok, err)
	}
}

func TestResolveBrowseParentsPreservesGetOutcomes(t *testing.T) {
	ctx := context.Background()
	warm, cold, stale, eligible, missing := core.ShowKey(1), core.ShowKey(2), core.ShowKey(3), core.ShowKey(4), core.ShowKey(5)
	calls := map[core.RatingKey]int{}
	get := func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		calls[key]++
		if key == missing {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: key, Type: core.TypeShow}, nil
	}
	got := resolveBrowseParents(ctx, []core.RatingKey{warm, warm, cold, stale, eligible, missing, missing}, get)
	for _, key := range []core.RatingKey{warm, cold, stale, eligible} {
		if got[key].Key != key || calls[key] != 1 {
			t.Fatalf("key %s result %+v calls %d", key, got[key], calls[key])
		}
	}
	if _, ok := got[missing]; ok || calls[missing] != 1 {
		t.Fatal("missing parent not denied")
	}
}
