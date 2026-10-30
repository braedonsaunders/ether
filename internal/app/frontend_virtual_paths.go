package app

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type surfaceVirtualPaths struct {
	experience frontend.Experience
	base       func(context.Context, *http.Request) (string, error)
	enabled    func() bool
}

func (s surfaceVirtualPaths) Paths(ctx context.Context, viewer *core.Viewer, device core.Device, keys []core.RatingKey) (map[core.RatingKey]string, error) {
	if s.experience == nil || (s.enabled != nil && !s.enabled()) {
		return nil, core.ErrNotAvailable
	}
	return experience.VirtualPaths(ctx, s.virtualPathCatalog(), viewer, device, keys)
}

func (s surfaceVirtualPaths) Base(ctx context.Context, req *http.Request) (string, error) {
	if s.enabled == nil || !s.enabled() || s.base == nil {
		return "", core.ErrNotAvailable
	}
	return s.base(ctx, req)
}

func (s surfaceVirtualPaths) Path(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (string, error) {
	if s.experience == nil {
		return "", core.ErrNotAvailable
	}
	if s.enabled != nil && !s.enabled() {
		return "", core.ErrNotAvailable
	}
	detail, err := s.experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
	if err != nil {
		return "", err
	}
	title := detail.Title
	if title.Key != key {
		return "", core.ErrNotFound
	}
	return experience.VirtualPath(ctx, s.virtualPathCatalog(), viewer, device, title)
}

func (s surfaceVirtualPaths) virtualPathCatalog() experience.VirtualPathCatalog {
	exp := s.experience
	return experience.VirtualPathCatalog{
		Views: func(ctx context.Context, viewer *core.Viewer) ([]experience.VirtualPathView, error) {
			views, err := exp.Views(ctx, viewer)
			if err != nil {
				return nil, err
			}
			out := make([]experience.VirtualPathView, len(views))
			for i, view := range views {
				out[i] = experience.VirtualPathView{ID: view.ID, Type: view.Type}
			}
			return out, nil
		},
		LibraryItems: func(ctx context.Context, viewer *core.Viewer, device core.Device, id string, offset, limit int) (experience.VirtualPathPage, error) {
			page, err := exp.LibraryItems(ctx, viewer, device, id, frontend.ItemQuery{Offset: offset, Limit: limit})
			if err != nil {
				return experience.VirtualPathPage{}, err
			}
			return experience.VirtualPathPage{Items: page.Items, Total: page.Total}, nil
		},
		Children: func(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey, offset, limit int) (experience.VirtualPathPage, error) {
			page, err := exp.Children(ctx, viewer, device, key, frontend.ItemQuery{Offset: offset, Limit: limit})
			if err != nil {
				return experience.VirtualPathPage{}, err
			}
			return experience.VirtualPathPage{Items: page.Items, Total: page.Total}, nil
		},
	}
}
