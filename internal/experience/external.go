package experience

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ResolveExternal resolves an IMDb ID to a currently visible catalogue item.
// Episode suffixes use canonical TMDB numbering, regardless of viewer display order.
func (s *Service) ResolveExternal(ctx context.Context, viewer *core.Viewer, _ core.Device, req frontend.ExternalRequest) (core.RatingKey, error) {
	if req.Source != "imdb" || len(req.ID) < 3 || len(req.ID) > 18 || req.ID[:2] != "tt" {
		return 0, core.ErrNotFound
	}
	for _, ch := range req.ID[2:] {
		if ch < '0' || ch > '9' {
			return 0, core.ErrNotFound
		}
	}
	if (req.Season == nil) != (req.Episode == nil) {
		return 0, core.ErrNotFound
	}
	if req.Type != "" && req.Type != core.TypeMovie && req.Type != core.TypeShow && req.Type != core.TypeEpisode {
		return 0, core.ErrNotFound
	}
	if req.Season != nil && (*req.Season < 0 || *req.Season > 999 || *req.Episode < 0 || *req.Episode > 9999) {
		return 0, core.ErrNotFound
	}
	active, err := s.viewer(ctx, viewer)
	if err != nil {
		return 0, core.ErrNotFound
	}
	if s.d.ByExternalID == nil {
		return 0, core.ErrNotAvailable
	}
	t, err := s.d.ByExternalID(ctx, "imdb", req.ID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return 0, core.ErrNotFound
		}
		return 0, err
	}
	if t == nil || !t.Key.Valid() {
		return 0, core.ErrNotFound
	}
	switch t.Type {
	case core.TypeMovie:
		if t.Key.Kind() != core.KindMovie {
			return 0, core.ErrNotFound
		}
	case core.TypeShow:
		if t.Key.Kind() != core.KindShow {
			return 0, core.ErrNotFound
		}
	case core.TypeEpisode:
		if t.Key.Kind() != core.KindEpisode {
			return 0, core.ErrNotFound
		}
	default:
		return 0, core.ErrNotFound
	}
	if req.Type != "" && t.Type != req.Type {
		return 0, core.ErrNotFound
	}
	key := t.Key
	if req.Season != nil {
		if t.Type != core.TypeShow || t.Key.TMDB() <= 0 || t.Key.TMDB() >= 100000000 {
			return 0, core.ErrNotFound
		}
		key = core.EpisodeKey(t.Key.TMDB(), *req.Season, *req.Episode)
	}
	result, err := s.item(ctx, active, key)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, core.ErrNotFound
	}
	if req.Season != nil && (result.Type != core.TypeEpisode || result.ShowKey != t.Key || result.Season != *req.Season || result.Episode != *req.Episode) {
		return 0, core.ErrNotFound
	}
	if req.Season == nil && result.Type != t.Type {
		return 0, core.ErrNotFound
	}
	return key, nil
}
