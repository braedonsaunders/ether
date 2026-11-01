package experience

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestScheduledEpisodesStayUnplayableAndGroupedShowsSkipThem(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	show := core.Title{Key: core.ShowKey(7), Type: core.TypeShow, Title: "Show"}
	season := core.Title{Key: core.SeasonKey(7, 1), Type: core.TypeSeason, ShowKey: show.Key, Season: 1}
	future := core.Title{Key: core.EpisodeKey(7, 1, 3), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 3, Title: "Later", Released: time.Now().Add(48 * time.Hour)}
	adult := core.Title{Key: core.EpisodeKey(7, 1, 4), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 4, Adult: true, Released: time.Now().Add(72 * time.Hour)}
	get := func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		switch k {
		case show.Key:
			return &show, nil
		case season.Key:
			return &season, nil
		default:
			return nil, core.ErrNotFound
		}
	}
	grouped := false
	s := &Service{d: ServiceDeps{
		Active: func(context.Context, int64) (*core.User, error) { return v, nil },
		Get:    get,
		Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			return nil, nil
		},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }},
		Scheduled: func(context.Context, core.RatingKey) ([]core.Title, error) {
			return []core.Title{future, adult}, nil
		},
		Grouped: func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error) {
			return map[core.RatingKey]bool{show.Key: grouped}, nil
		},
	}}
	page, err := s.Children(ctx, v, core.Device{}, season.Key, frontend.ItemQuery{})
	if err != nil || page.Total != 1 || page.Items[0].Key != future.Key {
		t.Fatalf("season %+v %v", page, err)
	}
	detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Key: future.Key})
	if err != nil || detail.CanPlay == nil || *detail.CanPlay || detail.Title.Key != future.Key {
		t.Fatalf("detail %+v %v", detail, err)
	}
	grouped = true
	page, err = s.Children(ctx, v, core.Device{}, season.Key, frontend.ItemQuery{})
	if err != nil || page.Total != 0 {
		t.Fatalf("grouped %+v %v", page, err)
	}
}

func TestUpcomingCollectionKeepsFutureMembers(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	now := time.Now()
	future := core.Title{Key: core.MovieKey(8), Type: core.TypeMovie, Title: "Soon", Released: now.Add(48 * time.Hour)}
	past := core.Title{Key: core.MovieKey(9), Type: core.TypeMovie, Title: "Gone", Released: now.Add(-48 * time.Hour)}
	adult := core.Title{Key: core.MovieKey(10), Type: core.TypeMovie, Title: "Adult", Adult: true, Released: now.Add(48 * time.Hour)}
	key := core.CollectionKey(3)
	s := &Service{d: ServiceDeps{
		Active:      func(context.Context, int64) (*core.User, error) { return v, nil },
		Collections: serviceCollections{collection: core.Collection{Key: key, Type: core.TypeMovie, Upcoming: true}, titles: []core.Title{future, past, adult}},
		Visibility:  Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }, FilterAvailable: func(context.Context, []core.Title) []core.Title { return nil }},
	}}
	page, err := s.Children(ctx, v, core.Device{}, key, frontend.ItemQuery{})
	if err != nil || page.Total != 1 || page.Items[0].Key != future.Key {
		t.Fatalf("upcoming %+v %v", page, err)
	}
}
