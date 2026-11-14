package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"strings"
)

// CandidateRows projects already-authorized neutral selection data;
// source-specific Context is deliberately absent from the candidate contract.
func CandidateRows(candidates []RankedCandidate) []frontend.Row {
	out := make([]frontend.Row, 0, len(candidates))
	for _, c := range candidates {
		row := frontend.Row{ID: c.ID, Title: c.Title, PreviewOnFocus: c.PreviewOnFocus, RankedTop10: c.RankedTop10, Purpose: c.Purpose, Style: c.Style, Promoted: c.Promoted, More: c.More, Collection: c.Key}
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

// HomePurpose lives in internal/frontend (host_sessions_experience.go).
var HomePurpose = frontend.HomePurpose
