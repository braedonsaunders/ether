package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// legacyHomeLayout adapts external providers that have not yet implemented the
// neutral contract. It only translates inputs and output identities.
type legacyHomeLayout struct {
	source   core.HomeLayoutSource
	context  core.HubContext
	sidecars []model.Hub
}

func (a legacyHomeLayout) FinalizeNeutralHome(ctx context.Context, viewer *core.User, _ core.HomeSituation, section string, rows []core.NeutralHomeRow) ([]core.NeutralHomeRow, error) {
	raw := make([]core.Hub, len(rows))
	for i, row := range rows {
		raw[i] = core.Hub{ID: row.ID, Title: row.Title, Type: row.Type, Context: a.sidecars[i].Context, ItemCount: row.ItemCount}
	}
	ordered, err := a.source.FinalizeHome(ctx, viewer, a.context, section, raw)
	if err != nil {
		return nil, err
	}
	out := make([]core.NeutralHomeRow, len(ordered))
	for i, row := range ordered {
		out[i] = core.NeutralHomeRow{ID: row.ID}
	}
	return out, nil
}

// finalizeHomeRows preserves producer wire sidecars. A missing provider or a
// layout error deliberately leaves the original rows in place, as Plex did
// before extraction; errors are logged once at this domain boundary.
func (s *Server) finalizeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub) []model.Hub {
	engine, ok := s.d.Recommender.(core.NeutralHomeLayoutSource)
	if !ok {
		legacy, available := s.d.Recommender.(core.HomeLayoutSource)
		if !available {
			return rows
		}
		engine = legacyHomeLayout{source: legacy, context: hc, sidecars: rows}
	}
	candidates := make([]core.NeutralHomeRow, len(rows))
	for i, row := range rows {
		candidates[i] = core.NeutralHomeRow{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Purpose: homePurpose(core.Hub{ID: row.HubIdentifier, Type: core.MediaType(row.Type)}), ItemCount: max(row.Size, len(row.Metadata)+len(row.Directory))}
	}
	situation := core.HomeSituation{Device: hc.Device, Daypart: hc.Daypart, Weekend: hc.Weekend, Now: hc.Now, LibraryID: hc.SectionID, Product: hc.Product, Platform: hc.Platform, SessionID: hc.SessionID, ClientID: hc.ClientID}
	indices, err := experience.ArrangeNeutralHome(ctx, engine, pr.User, situation, section, candidates)
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
