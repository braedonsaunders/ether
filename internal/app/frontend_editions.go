package app

import (
	"context"
	"path/filepath"
	"strings"

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
		version := frontendVersion(title, variant)
		if name := probedFileName(variant.Label); name != "" {
			version.FileName = name
		}
		out = append(out, version)
	}
	return out, nil
}

// probedFileName accepts a local edition label only when it is the probed
// file's basename. Release titles and indexer names are not file names.
func probedFileName(label string) string {
	if label == "" || strings.ContainsAny(label, `/\`) {
		return ""
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(label), ".")) {
	case "mkv", "mp4", "m4v", "avi", "mov", "wmv", "webm", "mpg", "mpeg", "ts", "m2ts", "mpegts":
		return label
	default:
		return ""
	}
}
