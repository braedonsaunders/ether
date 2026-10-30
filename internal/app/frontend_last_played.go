package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/state"
)

type surfaceLastPlayedSource struct {
	experience frontend.Experience
	state      *state.State
}

func (s surfaceLastPlayedSource) LastPlayedSource(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, key frontend.RatingKey) (*frontend.PlaybackSourceHistory, error) {
	if s.experience == nil || s.state == nil || viewer == nil {
		return nil, core.ErrUnauthorized
	}
	detail, err := s.experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
	if err != nil || detail.Title.Key != key {
		return nil, core.ErrNotFound
	}
	return s.state.LastSource(ctx, viewer.ID, key)
}
