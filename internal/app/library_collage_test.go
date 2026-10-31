package app

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type collageState struct{ core.State }

type collageArt struct {
	img frontend.Image
}

func (collageArt) List(context.Context, frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	return nil, true, nil
}
func (collageArt) Read(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
	return nil, "", core.ErrNotFound
}
func (a collageArt) LibraryCollage(context.Context, *core.Viewer, core.Device, string, []core.Title) (frontend.Image, error) {
	return a.img, nil
}

func TestLibraryCollageDoesNotBrowseOnTheRequest(t *testing.T) {
	block := make(chan struct{})
	browsed := make(chan struct{}, 1)
	svc, err := experience.NewService(experience.ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			return &core.User{ID: id, IsOwner: true}, nil
		},
		Libraries: []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}},
		Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
			return nil, core.ErrNotFound
		},
		Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			return nil, nil
		},
		Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
			select {
			case browsed <- struct{}{}:
			default:
			}
			return core.Page{}, nil
		},
		State:    collageState{},
		Rows:     &experience.RowsService{},
		Protocol: "jellyfin",
		Visibility: experience.Visibility{
			SectionFor: func(core.MediaType) (int, bool) { return 1, true },
			GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
				return nil, core.ErrNotFound
			},
		},
		Search: experience.Search{Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
			return nil, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	viewID, err := ids.GUID(ids.View, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := frontend.LibraryCollagePrefix + "abc"
	s := &surfaceExperience{
		Service:    svc,
		artwork:    collageArt{img: frontend.Image{Kind: frontend.ImagePoster, Path: ref, Tag: ref, Subject: viewID}},
		catalogGen: func() uint64 { return 3 },
		posters: func(context.Context, core.MediaType, int) ([]core.Title, error) {
			<-block
			return []core.Title{{Key: core.MovieKey(9), Type: core.TypeMovie, PosterPath: "/p.jpg"}}, nil
		},
	}
	viewer := &core.User{ID: 1, IsOwner: true}
	start := time.Now()
	_, err = s.LibraryCollage(context.Background(), viewer, core.Device{ID: "tv"}, viewID)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond || err == nil {
		t.Fatalf("cold collage waited %s: %v", elapsed, err)
	}
	select {
	case <-browsed:
		t.Fatal("collage browsed the catalogue")
	default:
	}
	close(block)
	deadline := time.Now().Add(time.Second)
	for {
		img, err := s.LibraryCollage(context.Background(), viewer, core.Device{ID: "tv"}, viewID)
		if err == nil && img.Path == ref {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("collage was not cached: %v %+v", err, img)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-browsed:
		t.Fatal("cached collage browsed the catalogue")
	default:
	}
}
