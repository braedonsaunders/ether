package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

func TestCachedSessionTitleNeverRefreshesMetadata(t *testing.T) {
	ctx := context.Background()
	episode := core.EpisodeKey(4, 1, 1)
	show := core.ShowKey(4)
	calls := 0
	remote := 0
	titles := map[core.RatingKey]*core.Title{episode: {Key: episode, Type: core.TypeEpisode, ShowKey: show, ContentRating: "PG"}, show: {Key: show, Type: core.TypeShow, ContentRating: "PG"}}
	cached := func(_ context.Context, k core.RatingKey) (*core.Title, error) {
		calls++
		if titles[k] == nil {
			return nil, core.ErrNotFound
		}
		return titles[k], nil
	}
	vis := experience.Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { remote++; return nil, core.ErrNotFound }}
	viewer := &core.Viewer{ID: 1, MaxContentRating: "PG"}
	for i := 0; i < 10; i++ {
		if _, err := cachedSessionTitle(ctx, viewer, episode, cached, vis); err != nil {
			t.Fatal(err)
		}
	}
	if remote != 0 || calls != 20 {
		t.Fatalf("remote=%d cached=%d", remote, calls)
	}
	titles[show].ContentRating = "R"
	if _, err := cachedSessionTitle(ctx, viewer, episode, cached, vis); err == nil {
		t.Fatal("restricted parent allowed")
	}
	delete(titles, show)
	if _, err := cachedSessionTitle(ctx, viewer, episode, cached, vis); err == nil {
		t.Fatal("missing parent allowed")
	}
	if remote != 0 {
		t.Fatalf("remote metadata calls: %d", remote)
	}
}
