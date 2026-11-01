package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// DiscoveryRowSource returns context-free row candidates for one viewer.
type DiscoveryRowSource interface {
	Rows(context.Context, *core.User, int) ([]RankedCandidate, error)
}

// AdditionalRows composes viewer-owned playlists, discovery and live guide rows.
// Sources are invoked in that order; a failure stops subsequent sources.
type AdditionalRows struct {
	Playlists  PlaylistRowSource
	Watchlist  DiscoveryRowSource
	Personal   DiscoveryRowSource
	Live       LiveGuideSource
	Visibility Visibility
	Restricted func(*core.User) bool
	Now        func() time.Time
}

func (a AdditionalRows) Rows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	if req.Placement != frontend.PlacementHome || req.Viewer == nil {
		return nil, nil
	}
	var rows []frontend.Row
	if a.Playlists != nil {
		entries, more, err := PlaylistEntries(ctx, req.Viewer, a.Playlists, req.Limit)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "playlists", Title: "Recent Playlists", Purpose: core.RowPurpose("playlist"), Entries: entries, More: more})
		}
	}
	restricted := !req.Viewer.IsOwner && (req.Viewer.MaxContentRating != "" || len(req.Viewer.AllowedSections) > 0 || a.Restricted == nil || a.Restricted(req.Viewer))
	for _, source := range []DiscoveryRowSource{a.Watchlist, a.Personal} {
		if source == nil {
			continue
		}
		candidates, err := a.discovery(ctx, req.Viewer, req.Limit, restricted, source)
		if err != nil {
			return nil, err
		}
		for _, c := range candidates {
			row := frontend.Row{ID: c.ID, Title: c.Title, Purpose: c.Purpose, Style: c.Style, Promoted: c.Promoted, More: c.More, Collection: c.Key}
			for i := range c.Items {
				item := c.Items[i]
				row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
			}
			rows = append(rows, row)
		}
	}
	if a.Live != nil {
		now := a.Now()
		capN := req.Limit
		if capN <= 0 {
			capN = 50
		}
		entries, err := liveEntries(ctx, req.Viewer, a.Live, now, now.Add(24*time.Hour), nil, capN+1)
		if err != nil {
			return nil, err
		}
		more := len(entries) > capN
		if more {
			entries = entries[:capN]
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "live", Title: "On Now", Purpose: frontend.RowLive, Entries: entries, More: more})
		}
	}
	return rows, nil
}

func (a AdditionalRows) discovery(ctx context.Context, viewer *core.User, limit int, restricted bool, source DiscoveryRowSource) ([]RankedCandidate, error) {
	if !restricted {
		return source.Rows(ctx, viewer, limit)
	}
	var candidates []RankedCandidate
	var loadErr error
	rows := LoadDiscovery(ctx, viewer, a.Visibility, limit, func(ctx context.Context, fetch int) []DiscoveryCandidate {
		candidates, loadErr = source.Rows(ctx, viewer, fetch)
		if loadErr != nil {
			return nil
		}
		out := make([]DiscoveryCandidate, len(candidates))
		for i, c := range candidates {
			out[i] = DiscoveryCandidate{Index: i, Items: c.Items}
		}
		return out
	})
	if loadErr != nil {
		return nil, loadErr
	}
	out := make([]RankedCandidate, 0, len(rows))
	for _, row := range rows {
		c := candidates[row.Index]
		c.Items = row.Items
		out = append(out, c)
	}
	return out, nil
}
