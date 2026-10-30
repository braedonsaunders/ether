package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type activityState struct {
	core.State
	statuses []core.WatchStatus
}

func (s activityState) InProgress(context.Context, int64, int) ([]core.WatchStatus, error) {
	return s.statuses, nil
}

func TestLibraryActivityDoesNotDependOnHomeAndFiltersBeforeLimit(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1, MaxContentRating: "PG", AllowedSections: []int{1}}
	movie1 := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Hidden", ContentRating: "R"}
	movie2 := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Visible", ContentRating: "PG"}
	movie3 := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Older", ContentRating: "G"}
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow, ContentRating: "TV-MA"}
	episode := core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}
	titles := map[core.RatingKey]*core.Title{movie1.Key: &movie1, movie2.Key: &movie2, movie3.Key: &movie3, show.Key: &show, episode.Key: &episode}
	get := func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		if t := titles[key]; t != nil {
			return t, nil
		}
		return nil, core.ErrNotFound
	}
	var browseCalls int
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, CachedGet: get, State: activityState{statuses: []core.WatchStatus{{Key: movie1.Key}, {Key: episode.Key}, {Key: movie2.Key}, {Key: movie3.Key}}}, AddedTimes: func(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error) {
		return map[core.RatingKey]time.Time{movie2.Key: time.Unix(20, 0), movie3.Key: time.Unix(10, 0)}, nil
	}, Visibility: Visibility{GetTitle: get, SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeMovie {
			return 1, true
		}
		return 2, true
	}}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		browseCalls++
		if q.Filter.Type != core.TypeMovie || q.Sort != core.SortAdded || !q.Desc {
			t.Fatalf("unexpected browse %+v", q)
		}
		return core.Page{Items: []core.Title{movie1, movie2, movie3}, Total: 3}, nil
	}}}
	latest, err := s.LatestAdded(ctx, v, core.Device{}, "1", 1)
	if err != nil || len(latest) != 1 || latest[0].Title.Key != movie2.Key || latest[0].AddedAt == nil || latest[0].AddedAt.Unix() != 20 || browseCalls != 1 {
		t.Fatalf("latest %+v %v", latest, err)
	}
	resume, err := s.Resume(ctx, v, core.Device{}, "1", []core.MediaType{core.TypeMovie}, 0, 1, false)
	if err != nil || len(resume.Items) != 1 || resume.Items[0].Key != movie2.Key || !resume.More || resume.TotalExact {
		t.Fatalf("resume %+v %v", resume, err)
	}
	resume, err = s.Resume(ctx, v, core.Device{}, "1", []core.MediaType{core.TypeMovie}, 0, 1, true)
	if err != nil || !resume.TotalExact || resume.Total != 2 || !resume.More {
		t.Fatalf("exact resume count %+v %v", resume, err)
	}
	resume, err = s.Resume(ctx, v, core.Device{}, "1", []core.MediaType{core.TypeMovie}, 1, 1, false)
	if err != nil || len(resume.Items) != 1 || resume.Items[0].Key != movie3.Key || resume.More || !resume.TotalExact || resume.Total != 2 {
		t.Fatalf("resume next page %+v %v", resume, err)
	}
	if _, err = s.LatestAdded(ctx, v, core.Device{}, "2", 1); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("restricted view: %v", err)
	}
	if _, err = s.Resume(ctx, v, core.Device{}, "2", nil, 0, 1, false); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("restricted resume view: %v", err)
	}
	// The same child remains hidden by inherited show policy when the section is allowed.
	v.AllowedSections = []int{1, 2}
	resume, err = s.Resume(ctx, v, core.Device{}, "2", []core.MediaType{core.TypeEpisode}, 0, 5, false)
	if err != nil || len(resume.Items) != 0 || !resume.TotalExact {
		t.Fatalf("restricted parent leaked episode: %+v %v", resume, err)
	}
}

func TestLatestAddedGlobalMergesRealAddedTimes(t *testing.T) {
	v := &core.User{ID: 1}
	movie := core.Title{Key: core.MovieKey(4), Type: core.TypeMovie}
	show := core.Title{Key: core.ShowKey(5), Type: core.TypeShow}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeMovie {
			return 1, true
		}
		return 2, true
	}}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Filter.Type == core.TypeMovie {
			return core.Page{Items: []core.Title{movie}, Total: 1}, nil
		}
		return core.Page{Items: []core.Title{show}, Total: 1}, nil
	}, AddedTimes: func(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error) {
		return map[core.RatingKey]time.Time{movie.Key: time.Unix(10, 0), show.Key: time.Unix(20, 0)}, nil
	}}}
	got, err := s.LatestAdded(context.Background(), v, core.Device{}, "", 2)
	if err != nil || len(got) != 2 || got[0].Title.Key != show.Key || got[0].AddedAt == nil || got[0].AddedAt.Unix() != 20 || got[1].Title.Key != movie.Key {
		t.Fatalf("global latest %+v %v", got, err)
	}
}
