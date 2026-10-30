package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// finalizeHomeRows preserves producer wire sidecars. A missing provider or a
// layout error deliberately leaves the original rows in place, as Plex did
// before extraction; errors are logged once at this domain boundary.
func (s *Server) finalizeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub) []model.Hub {
	if _, neutral := s.d.Recommender.(core.NeutralHomeLayoutSource); !neutral {
		if _, layout := s.d.Recommender.(core.HomeLayoutSource); !layout {
			return rows
		}
	}
	raw := make([]core.Hub, len(rows))
	candidates := make([]core.NeutralHomeRow, len(rows))
	for i, row := range rows {
		raw[i] = core.Hub{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Context: row.Context, ItemCount: max(row.Size, len(row.Metadata)+len(row.Directory))}
		candidates[i] = core.NeutralHomeRow{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Purpose: homePurpose(core.Hub{ID: row.HubIdentifier, Type: core.MediaType(row.Type)}), ItemCount: max(row.Size, len(row.Metadata)+len(row.Directory))}
	}
	indices, err := experience.ArrangeHome(ctx, s.d.Recommender, pr.User, hc, section, raw, candidates)
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
