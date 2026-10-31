package app

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// surfaceEditions exposes probed local copies only. A miss is an empty list,
// not a resolver search.
type surfaceEditions struct{ app *App }

func (s surfaceEditions) Editions(ctx context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) ([]frontend.Version, error) {
	if s.app == nil {
		return nil, nil
	}
	title, err := s.app.catalog.Get(ctx, key)
	if err != nil || title == nil {
		return nil, nil
	}
	variants, err := localEditionVariants(s.app.opts.DataDir, title)
	if err != nil || len(variants) == 0 {
		return nil, err
	}
	out := make([]frontend.Version, 0, len(variants))
	for _, variant := range variants {
		out = append(out, frontendVersion(title, variant))
	}
	return out, nil
}
