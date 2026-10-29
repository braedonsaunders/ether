package plex

import (
	"context"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// homeProjection keeps Plex wire state request-local alongside neutral title
// candidates. The experience service must authorize candidates before exposure.
type homeProjection struct {
	Row      frontend.Row
	Spec     hubSpec
	Rendered model.Hub
}

func (s *Server) projectHomeHub(ctx context.Context, pr *Request, spec hubSpec) homeProjection {
	rendered := s.renderHub(ctx, pr, spec, hubCtx{})
	row := frontend.Row{ID: spec.ID, Title: spec.Title, Purpose: homePurpose(spec.Hub), Style: spec.Style, Promoted: spec.Promoted, More: spec.More, Collection: spec.Key}
	for i := range spec.Items {
		title := spec.Items[i]
		row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &title})
	}
	return homeProjection{Row: row, Spec: spec, Rendered: rendered}
}
