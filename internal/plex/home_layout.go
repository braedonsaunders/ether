package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Final placement preserves each producer's actual Plex metadata and tune URLs.
// Only the order/visibility of rows is returned by the layout engine.
func (s *Server) finalizeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub) []model.Hub {
	engine, ok := s.d.Recommender.(core.HomeLayoutSource)
	if !ok {
		return rows
	}
	candidates := make([]experience.LayoutCandidate, 0, len(rows))
	for _, row := range rows {
		candidates = append(candidates, experience.LayoutCandidate{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Purpose: homePurpose(core.Hub{ID: row.HubIdentifier, Type: core.MediaType(row.Type)}), ItemCount: max(row.Size, len(row.Metadata)+len(row.Directory))})
	}
	indices, err := experience.ArrangeLayout(candidates, func(candidates []experience.LayoutCandidate) ([]string, error) {
		raw := make([]core.Hub, 0, len(candidates))
		for i, candidate := range candidates {
			raw = append(raw, core.Hub{ID: candidate.ID, Title: candidate.Title, Type: candidate.Type, Context: rows[i].Context, ItemCount: candidate.ItemCount})
		}
		ordered, err := engine.FinalizeHome(ctx, pr.User, hc, section, raw)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(ordered))
		for _, row := range ordered {
			ids = append(ids, row.ID)
		}
		return ids, nil
	})
	if err != nil {
		s.log.Warn("Home layout unavailable", "error", err)
		return rows
	}
	out := make([]model.Hub, 0, len(indices))
	for _, i := range indices {
		out = append(out, rows[i])
	}
	return out
}
