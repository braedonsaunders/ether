//go:build scorecard && !plexparity

package app

import (
	"github.com/braedonsaunders/ether/internal/core"
)

func localEditionVariants(dir string, t *core.Title) ([]core.ReleaseVariant, error) {
	if t == nil || (t.Type != core.TypeMovie && t.Type != core.TypeEpisode) {
		return nil, nil
	}
	ids := t.IDs
	if t.Type == core.TypeEpisode {
		ids = t.ShowIDs
	}
	src := scorecardSource{dir}
	rels, err := src.Search(nil, core.SearchRequest{Type: t.Type, IDs: ids, Title: t.Title, Year: t.Year, Season: t.Season, Episode: t.Episode})
	if err != nil {
		return nil, err
	}
	out := make([]core.ReleaseVariant, 0, len(rels))
	for _, rel := range rels {
		entry, err := src.lookup(rel.ID)
		if err != nil {
			return nil, err
		}
		info := core.MediaInfo{Size: rel.Size}
		if entry.Media != nil {
			info = *entry.Media
		}
		out = append(out, core.ReleaseVariant{ID: rel.ID, Label: rel.Name, Release: rel, Info: info})
	}
	return out, nil
}
