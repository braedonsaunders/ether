package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestDetailFillsSeriesNameFromAuthorizedCachedParent(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, AllowedSections: []int{2}, MaxContentRating: "TV-PG"}
	show := &core.Title{Key: core.ShowKey(55), Type: core.TypeShow, Title: "The Series", Year: 2004, ContentRating: "TV-PG"}
	episode := &core.Title{Key: core.EpisodeKey(55, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, Title: "Episode", Year: 2020, ContentRating: "TV-PG"}
	get := func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		if key == show.Key {
			return show, nil
		}
		if key == episode.Key {
			return episode, nil
		}
		return nil, core.ErrNotFound
	}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Get: func(ctx context.Context, _ *core.User, key core.RatingKey) (*core.Title, error) { return get(ctx, key) }, CachedGet: get, State: serviceState{}, Visibility: Visibility{GetTitle: get, SectionFor: func(core.MediaType) (int, bool) { return 2, true }}}}
	detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: viewer, Key: episode.Key})
	if err != nil || detail.Title.ShowTitle != show.Title || detail.Title.ShowYear != 2004 || detail.Title.Year != 2020 || episode.ShowTitle != "" {
		t.Fatalf("series identity %+v: %v", detail.Title, err)
	}
	show.ContentRating = "TV-MA"
	if _, err = s.Item(ctx, frontend.ItemRequest{Viewer: viewer, Key: episode.Key}); err == nil {
		t.Fatal("restricted parent exposed through detail")
	}
}
