package app

import (
	"context"
	"net/http"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type surfaceVirtualPaths struct {
	experience frontend.Experience
	base       func(context.Context, *http.Request) (string, error)
	enabled    func() bool
	// stamp changes when the catalogue or the viewer's restrictions change.
	// A nil stamp keeps every request on the live walk, which tests rely on.
	stamp func(*core.Viewer) string
	index *pathIndex
}

type pathIndex struct {
	mu    sync.Mutex
	stamp string
	paths map[core.RatingKey]string
}

func (s surfaceVirtualPaths) Paths(ctx context.Context, viewer *core.Viewer, device core.Device, keys []core.RatingKey) (map[core.RatingKey]string, error) {
	if s.experience == nil || (s.enabled != nil && !s.enabled()) {
		return nil, core.ErrNotAvailable
	}
	if s.index != nil && s.stamp != nil && viewer != nil {
		stamp := s.stamp(viewer)
		if stamp == "" {
			return experience.VirtualPaths(ctx, s.virtualPathCatalog(), viewer, device, keys)
		}
		s.index.mu.Lock()
		if s.index.paths != nil && s.index.stamp == stamp {
			out := pathLookup(s.index.paths, keys)
			s.index.mu.Unlock()
			return out, nil
		}
		s.index.mu.Unlock()
		full, err := experience.IndexVirtualPaths(ctx, s.virtualPathCatalog(), viewer, device)
		if err != nil {
			return nil, err
		}
		s.index.mu.Lock()
		s.index.stamp = stamp
		s.index.paths = full
		out := pathLookup(full, keys)
		s.index.mu.Unlock()
		return out, nil
	}
	return experience.VirtualPaths(ctx, s.virtualPathCatalog(), viewer, device, keys)
}

func pathLookup(paths map[core.RatingKey]string, keys []core.RatingKey) map[core.RatingKey]string {
	out := make(map[core.RatingKey]string, len(keys))
	for _, key := range keys {
		if p, ok := paths[key]; ok {
			out[key] = p
		}
	}
	return out
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
