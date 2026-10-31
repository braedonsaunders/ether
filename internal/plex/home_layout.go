package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// homeLayoutFacts is the renderer's layout report: identity, title, type,
// context and the painted item count. Metadata, keys and programme sidecars
// stay on the wire row. Callers record the report when they paint the row.
func homeLayoutFacts(row model.Hub) (core.Hub, core.NeutralHomeRow) {
	count := max(row.Size, len(row.Metadata)+len(row.Directory))
	hub := core.Hub{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Context: row.Context, ItemCount: count}
	neutral := core.NeutralHomeRow{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Purpose: experience.HomePurpose(core.Hub{ID: row.HubIdentifier, Type: core.MediaType(row.Type)}), ItemCount: count}
	return hub, neutral
}

// arrangeHomeRows reorders painted rows from facts captured at paint time.
// A missing provider or a layout error leaves the original rows in place.
// Empty and duplicate rows are dropped by the hubs handlers after this returns.
func (s *Server) arrangeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub, hubs []core.Hub, neutral []core.NeutralHomeRow) []model.Hub {
	if len(rows) != len(hubs) || len(rows) != len(neutral) {
		if s.log != nil {
			s.log.Warn("Home layout unavailable", "error", "paint facts diverged")
		}
		return rows
	}
	if _, ok := s.d.Recommender.(core.NeutralHomeLayoutSource); !ok {
		if _, layout := s.d.Recommender.(core.HomeLayoutSource); !layout {
			return rows
		}
	}
	indices, err := experience.ArrangeHome(ctx, s.d.Recommender, pr.User, hc, section, hubs, neutral)
	if err != nil {
		if s.log != nil {
			s.log.Warn("Home layout unavailable", "error", err)
		}
		return rows
	}
	out := make([]model.Hub, 0, len(indices))
	for _, i := range indices {
		if i < 0 || i >= len(rows) {
			if s.log != nil {
				s.log.Warn("Home layout unavailable", "error", "index out of range")
			}
			return rows
		}
		out = append(out, rows[i])
	}
	return out
}

// finalizeHomeRows is the wire-level entry used by layout tests. Production
// home and section handlers pass facts captured while painting.
func (s *Server) finalizeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub) []model.Hub {
	hubs := make([]core.Hub, len(rows))
	neutral := make([]core.NeutralHomeRow, len(rows))
	for i, row := range rows {
		hubs[i], neutral[i] = homeLayoutFacts(row)
	}
	return s.arrangeHomeRows(ctx, pr, hc, section, rows, hubs, neutral)
}

// stableHomeHubs drops rows a client would draw empty, and keeps the first
// copy when the same hub was produced twice (promoted and section builders,
// or a recommender repeating a row the front end already built).
func stableHomeHubs(rows []model.Hub) []model.Hub {
	seen := map[string]bool{}
	out := make([]model.Hub, 0, len(rows))
	for _, h := range rows {
		if len(h.Metadata) == 0 && len(h.Directory) == 0 {
			continue
		}
		if h.HubIdentifier != "" {
			if seen[h.HubIdentifier] {
				continue
			}
			seen[h.HubIdentifier] = true
		}
		out = append(out, h)
	}
	return out
}
