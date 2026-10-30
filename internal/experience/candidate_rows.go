package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"strings"
)

// HubRows converts trusted recommender candidates to neutral rows;
// wire contexts stay on the producer side. Authorization and pagination are
// performed by experience.RowsService before returning these candidates.
func HubRows(hubs []core.Hub) []frontend.Row {
	out := make([]frontend.Row, 0, len(hubs))
	for _, h := range hubs {
		purpose := CandidatePurpose(h)
		row := frontend.Row{ID: h.ID, Title: h.Title, Purpose: purpose, Style: h.Style, Promoted: h.Promoted, More: h.More, Collection: h.Key}
		for i := range h.Items {
			item := h.Items[i]
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
		}
		out = append(out, row)
	}
	return out
}

// CandidateRows projects already-authorized neutral selection data;
// source-specific Context is deliberately absent from the candidate contract.
func CandidateRows(candidates []RankedCandidate) []frontend.Row {
	out := make([]frontend.Row, 0, len(candidates))
	for _, c := range candidates {
		row := frontend.Row{ID: c.ID, Title: c.Title, Purpose: c.Purpose, Style: c.Style, Promoted: c.Promoted, More: c.More, Collection: c.Key}
		for i := range c.Items {
			item := c.Items[i]
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
		}
		out = append(out, row)
	}
	return out
}

func CandidatePurpose(h core.Hub) core.RowPurpose {
	purpose := frontend.RowRecommendation
	id := strings.ToLower(h.ID)
	switch {
	case strings.Contains(id, "continue") || strings.Contains(id, "inprogress"):
		purpose = frontend.RowContinue
	case strings.Contains(id, "upnext") || strings.Contains(id, "ondeck"):
		purpose = frontend.RowNextUp
	case strings.Contains(id, "watchlist"):
		purpose = frontend.RowWatchlist
	case h.Key != 0 || strings.Contains(id, "collection"):
		purpose = frontend.RowCollection
	case strings.Contains(id, "recentlyreleased"):
		purpose = frontend.RowRecentlyReleased
	case strings.Contains(id, "livetv"):
		purpose = frontend.RowLive
	}
	return purpose
}

type CandidateRowSource struct {
	Load       func(context.Context, frontend.RowRequest) ([]RankedCandidate, error)
	Additional func(context.Context, frontend.RowRequest) ([]frontend.Row, error)
	Kind       func(string) string
}

func (s CandidateRowSource) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	candidates, err := s.Load(ctx, req)
	if err != nil {
		return nil, err
	}
	rows := CandidateRows(candidates)
	if s.Additional != nil {
		extra, err := s.Additional(ctx, req)
		if err != nil {
			return nil, err
		}
		rows = append(rows, extra...)
	}
	for i := range rows {
		if s.Kind != nil {
			rows[i].Kind = s.Kind(rows[i].ID)
		}
		rows[i].Rank = i
		rows[i].Layout.Style = rows[i].Style
	}
	return rows, nil
}

// homePurpose translates producer identifiers at the Plex boundary; no wire
// context or hub identifier is included in the neutral candidate.
func HomePurpose(h core.Hub) core.RowPurpose {
	id := strings.ToLower(h.ID)
	switch {
	case id == "home.continue":
		return "continue"
	case strings.Contains(id, "upnext") || strings.Contains(id, "ondeck"):
		return "nextUp"
	case strings.Contains(id, "watchlist"):
		return "watchlist"
	case h.Key != 0 || strings.Contains(id, "collection"):
		return "collection"
	case strings.Contains(id, "recentlyreleased") || strings.Contains(id, "recently.released"):
		return "recentlyReleased"
	case strings.Contains(id, "livetv"):
		return "live"
	default:
		return "recommendation"
	}
}
