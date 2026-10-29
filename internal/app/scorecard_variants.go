//go:build scorecard && !plexparity

package app

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
)

func scorecardVariants(ctx context.Context, a *App, _ *core.Viewer, t *core.Title, _ core.ClientCaps) ([]core.ReleaseVariant, error) {
	ids := t.IDs
	if t.Type == core.TypeEpisode {
		ids = t.ShowIDs
	}
	src := scorecardSource{a.opts.DataDir}
	rels, e := src.Search(ctx, core.SearchRequest{Type: t.Type, IDs: ids, Title: t.Title, Year: t.Year, Season: t.Season, Episode: t.Episode})
	if e != nil {
		return nil, e
	}
	out := make([]core.ReleaseVariant, 0, len(rels))
	for _, rel := range rels {
		entry, e := src.lookup(rel.ID)
		if e != nil {
			return nil, e
		}
		if entry.Media == nil {
			return nil, fmt.Errorf("scorecard media metadata not prepared")
		}
		out = append(out, core.ReleaseVariant{ID: rel.ID, Label: rel.Name, Release: rel, Info: *entry.Media})
	}
	return out, nil
}
