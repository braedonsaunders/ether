package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestRelatedArtworkUsesAuthorizedOwnersAndCredits(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	episode := core.EpisodeKey(11, 2, 3)
	season, show := episode.Parent(), episode.Parent().Parent()
	hidden := map[core.RatingKey]bool{}
	service, err := NewRelatedArtworkService(RelatedArtworkDeps{
		Authorize: func(_ context.Context, v *core.User, _ core.Device, k core.RatingKey) (*core.User, error) {
			if hidden[k] || v == nil || v.ID != viewer.ID {
				return nil, core.ErrNotFound
			}
			return viewer, nil
		},
		List: func(_ context.Context, _ *core.User, _ core.Device, k core.RatingKey) ([]frontend.Image, bool, error) {
			if k == episode {
				return []frontend.Image{{Kind: frontend.ImagePerson, Subject: "tmdb:123", Path: "person"}, {Kind: frontend.ImagePoster, Path: "episode"}, {Kind: frontend.ImagePerson, Path: "unidentified"}}, true, nil
			}
			return []frontend.Image{{Kind: frontend.ImagePoster, Path: k.String()}}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.RelatedArtwork(ctx, viewer, core.Device{ID: "tv"}, episode)
	if err != nil || !got.PeopleAuthoritative || len(got.People) != 1 || got.People[0].Subject != "tmdb:123" || got.Season == nil || got.Season.Item != season || got.Series == nil || got.Series.Item != show {
		t.Fatalf("related artwork %+v: %v", got, err)
	}
	hidden[season] = true
	got, err = service.RelatedArtwork(ctx, viewer, core.Device{}, episode)
	if err != nil || got.Season != nil || got.Series == nil {
		t.Fatalf("hidden season %+v: %v", got, err)
	}
	hidden[episode] = true
	if _, err = service.RelatedArtwork(ctx, viewer, core.Device{}, episode); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden child admitted: %v", err)
	}
}

func TestRelatedArtworkRechecksChildBeforeReturn(t *testing.T) {
	viewer := &core.User{ID: 1}
	episode := core.EpisodeKey(5, 1, 1)
	active := true
	service, err := NewRelatedArtworkService(RelatedArtworkDeps{
		Authorize: func(_ context.Context, v *core.User, _ core.Device, key core.RatingKey) (*core.User, error) {
			if !active && key == episode {
				return nil, core.ErrNotFound
			}
			return v, nil
		},
		List: func(_ context.Context, _ *core.User, _ core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
			if key == episode.Parent().Parent() {
				active = false
			}
			return []frontend.Image{{Kind: frontend.ImagePerson, Subject: "tmdb:1"}}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.RelatedArtwork(context.Background(), viewer, core.Device{}, episode)
	if !errors.Is(err, core.ErrNotFound) || len(got.People) != 0 || got.Season != nil || got.Series != nil {
		t.Fatalf("revoked child returned artwork: %+v %v", got, err)
	}
}

func TestRelatedArtworkRechecksParentBeforeReturn(t *testing.T) {
	viewer := &core.User{ID: 1}
	episode := core.EpisodeKey(5, 1, 1)
	seasonVisible := true
	service, err := NewRelatedArtworkService(RelatedArtworkDeps{
		Authorize: func(_ context.Context, v *core.User, _ core.Device, key core.RatingKey) (*core.User, error) {
			if !seasonVisible && key == episode.Parent() {
				return nil, core.ErrNotFound
			}
			return v, nil
		},
		List: func(_ context.Context, _ *core.User, _ core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
			if key == episode.Parent().Parent() {
				seasonVisible = false
			}
			return []frontend.Image{{Kind: frontend.ImagePoster, Path: "opaque"}}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.RelatedArtwork(context.Background(), viewer, core.Device{}, episode)
	if !errors.Is(err, core.ErrNotFound) || got.Season != nil || got.Series != nil {
		t.Fatalf("revoked season returned artwork: %+v %v", got, err)
	}
}

func TestRelatedArtworkRejectsEpisodeWhenShowIsRestricted(t *testing.T) {
	viewer := &core.User{ID: 1, MaxContentRating: "TV-PG", AllowedSections: []int{2}}
	episode := core.EpisodeKey(5, 1, 1)
	show := episode.Parent().Parent()
	titles := map[core.RatingKey]*core.Title{
		episode: {Key: episode, Type: core.TypeEpisode, ShowKey: show, ContentRating: "TV-PG"},
		show:    {Key: show, Type: core.TypeShow, ContentRating: "TV-MA"},
	}
	get := func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		if title := titles[key]; title != nil {
			return title, nil
		}
		return nil, core.ErrNotFound
	}
	experience := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, CachedGet: get, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }}}}
	lists := 0
	service, err := NewRelatedArtworkService(RelatedArtworkDeps{Authorize: experience.AuthorizeArtwork, List: func(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]frontend.Image, bool, error) {
		lists++
		return nil, true, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.RelatedArtwork(context.Background(), viewer, core.Device{}, episode); !errors.Is(err, core.ErrNotFound) || lists != 0 {
		t.Fatalf("restricted parent leaked artwork: lists=%d err=%v", lists, err)
	}
}
