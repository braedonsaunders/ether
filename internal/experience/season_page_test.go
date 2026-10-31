package experience

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestAllowTitlesUsesOneAvailabilityBatch(t *testing.T) {
	calls := 0
	titles := make([]core.Title, 8)
	for i := range titles {
		titles[i] = core.Title{Key: core.MovieKey(int64(i + 1)), Type: core.TypeMovie, Title: "t"}
	}
	s := &Service{d: ServiceDeps{Visibility: Visibility{
		SectionFor: func(core.MediaType) (int, bool) { return 1, true },
		FilterAvailable: func(_ context.Context, batch []core.Title) []core.Title {
			calls++
			if len(batch) != len(titles) {
				t.Fatalf("batch %d", len(batch))
			}
			return batch
		},
	}}}
	got, err := s.allowTitles(context.Background(), &core.User{ID: 1, IsOwner: true}, titles)
	if err != nil || calls != 1 || len(got) != len(titles) {
		t.Fatalf("allowed %d calls %d err %v", len(got), calls, err)
	}
}

func TestSeasonPageDoesNotLoadTheShow(t *testing.T) {
	show := core.ShowKey(7)
	season := 1
	s := &Service{d: ServiceDeps{
		Active: func(context.Context, int64) (*core.User, error) {
			return &core.User{ID: 1, IsOwner: true}, nil
		},
		Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: show, Type: core.TypeShow}, nil
		},
		Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			t.Fatal("loaded every episode")
			return nil, nil
		},
		SeasonEpisodes: func(_ context.Context, _ *core.User, got core.RatingKey, gotSeason, offset, limit int) (core.Page, bool, error) {
			if got != show || gotSeason != season || offset != 0 || limit != 50 {
				t.Fatalf("page %s season %d offset %d limit %d", got, gotSeason, offset, limit)
			}
			return core.Page{Items: []core.Title{{Key: core.EpisodeKey(7, 1, 1), Type: core.TypeEpisode, Season: 1, Episode: 1}}, Total: 2868}, true, nil
		},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }},
	}}
	page, err := s.Children(context.Background(), &core.User{ID: 1}, core.Device{}, show, frontend.ItemQuery{Recursive: true, Season: &season, Limit: 50, Types: []core.MediaType{core.TypeEpisode}})
	if err != nil || page.Total != 2868 || len(page.Items) != 1 || page.Items[0].Episode != 1 {
		t.Fatalf("page %+v %v", page, err)
	}
}

func TestSeasonPageKeepsTheScheduledTail(t *testing.T) {
	show := core.ShowKey(7)
	season := 1
	future := core.Title{Key: core.EpisodeKey(7, 1, 3), Type: core.TypeEpisode, Season: 1, Episode: 3, Released: time.Now().Add(48 * time.Hour)}
	s := &Service{d: ServiceDeps{
		Active: func(context.Context, int64) (*core.User, error) {
			return &core.User{ID: 1, IsOwner: true}, nil
		},
		Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: show, Type: core.TypeShow}, nil
		},
		Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			t.Fatal("loaded every episode")
			return nil, nil
		},
		SeasonEpisodes: func(context.Context, *core.User, core.RatingKey, int, int, int) (core.Page, bool, error) {
			return core.Page{Items: []core.Title{
				{Key: core.EpisodeKey(7, 1, 1), Type: core.TypeEpisode, Season: 1, Episode: 1},
				{Key: core.EpisodeKey(7, 1, 2), Type: core.TypeEpisode, Season: 1, Episode: 2},
			}, Total: 2}, true, nil
		},
		Scheduled: func(context.Context, core.RatingKey) ([]core.Title, error) {
			return []core.Title{future}, nil
		},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }},
	}}
	page, err := s.Children(context.Background(), &core.User{ID: 1}, core.Device{}, show, frontend.ItemQuery{Recursive: true, Season: &season, Limit: 50, Types: []core.MediaType{core.TypeEpisode}})
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.Items[2].Episode != 3 {
		t.Fatalf("tail %+v %v", page, err)
	}
}

func TestCustomEpisodeGroupStillUsesGrandchildren(t *testing.T) {
	show := core.ShowKey(7)
	season := 1
	loaded := false
	s := &Service{d: ServiceDeps{
		Active: func(context.Context, int64) (*core.User, error) {
			return &core.User{ID: 1, IsOwner: true}, nil
		},
		Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
			return &core.Title{Key: show, Type: core.TypeShow}, nil
		},
		Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			loaded = true
			return []core.Title{
				{Key: core.EpisodeKey(7, 1, 1), Type: core.TypeEpisode, Season: 1, Episode: 1},
				{Key: core.EpisodeKey(7, 1, 2), Type: core.TypeEpisode, Season: 1, Episode: 2},
				{Key: core.EpisodeKey(7, 1, 3), Type: core.TypeEpisode, Season: 1, Episode: 3},
			}, nil
		},
		SeasonEpisodes: func(context.Context, *core.User, core.RatingKey, int, int, int) (core.Page, bool, error) {
			return core.Page{}, false, nil
		},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }},
	}}
	page, err := s.Children(context.Background(), &core.User{ID: 1}, core.Device{}, show, frontend.ItemQuery{Recursive: true, Season: &season, Limit: 2, Types: []core.MediaType{core.TypeEpisode}})
	if err != nil || !loaded || page.Total != 3 || len(page.Items) != 2 {
		t.Fatalf("group %+v loaded %v %v", page, loaded, err)
	}
}
