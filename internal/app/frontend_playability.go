package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func detailCanPlay(ctx context.Context, req frontend.ItemRequest, protocol core.Protocol, capabilities frontend.Capabilities, broker experience.EstimatePlanner) *bool {
	return experience.DetailCanPlay(ctx, req, protocol, capabilities, broker)
}
