package experience

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RowSource supplies neutral candidate rows for one viewer and device.
type RowSource interface {
	Rows(context.Context, frontend.RowRequest) ([]frontend.Row, error)
}

// RowServiceDeps are host-owned dependencies. AllowRow and AllowEntry must
// recheck current membership and visibility, including non-title entries.
type RowServiceDeps struct {
	Source     RowSource
	Visibility Visibility
	Active     func(context.Context, *core.User) bool
	AllowRow   func(context.Context, *core.User, frontend.Row) bool
	AllowEntry func(context.Context, *core.User, frontend.RowEntry) bool
	Layout     func(context.Context, frontend.RowRequest, []frontend.Row) ([]frontend.Row, error)
	Receipts   *Receipts
}

// RowsService produces viewer-scoped rows for non-Plex surfaces.
type PlexRowDeps struct {
	Source     RowSource
	Active     func(context.Context, *core.User) bool
	AllowRow   func(context.Context, *core.User, frontend.Row) bool
	AllowEntry func(context.Context, *core.User, frontend.RowEntry) bool
	Layout     func(context.Context, frontend.RowRequest, []frontend.Row) ([]frontend.Row, error)
}

type RowsService struct {
	d    RowServiceDeps
	plex *PlexRowDeps
}

// NewPlexRowsService requires authorization even for host-supplied projections.
func NewPlexRowsService(d PlexRowDeps) (*RowsService, error) {
	if d.Source == nil || d.Active == nil || d.AllowRow == nil || d.AllowEntry == nil || d.Layout == nil {
		return nil, errors.New("experience: incomplete Plex row dependencies")
	}
	return &RowsService{plex: &d}, nil
}

func NewRowsService(d RowServiceDeps) (*RowsService, error) {
	if d.Source == nil || d.Active == nil || d.AllowRow == nil || d.AllowEntry == nil || d.Receipts == nil || d.Visibility.GetTitle == nil || d.Visibility.SectionFor == nil {
		return nil, errors.New("experience: incomplete row dependencies")
	}
	return &RowsService{d: d}, nil
}

// Rows filters each tagged entry before limiting and never accepts client-made
// interaction identifiers. Candidate requests expand to a bounded prefix when
// filtering would otherwise leave a row short.
func (s *RowsService) Rows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol) ([]frontend.Row, error) {
	if s.plex != nil {
		if protocol != core.Protocol("plex") || req.Viewer == nil || !s.plex.Active(ctx, req.Viewer) {
			return nil, errors.New("experience: invalid Plex viewer")
		}
		check := func(rows []frontend.Row) error {
			for _, row := range rows {
				if !s.plex.AllowRow(ctx, req.Viewer, row) {
					return errors.New("experience: unauthorized Plex row")
				}
				for _, entry := range row.Entries {
					if entryIdentity(entry) == "" || !s.plex.AllowEntry(ctx, req.Viewer, entry) {
						return errors.New("experience: unauthorized Plex entry")
					}
				}
			}
			return nil
		}
		rows, err := s.plex.Source.Rows(ctx, req)
		if err != nil {
			return nil, err
		}
		if err = check(rows); err != nil {
			return nil, err
		}
		rows, err = s.plex.Layout(ctx, req, rows)
		if err != nil {
			return nil, err
		}
		if err = check(rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	if req.Viewer == nil || req.Device.ID == "" || protocol == "" || !s.d.Active(ctx, req.Viewer) {
		return nil, errors.New("experience: invalid row viewer")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	fetch := limit
	var out []frontend.Row
	var err error
	for {
		sourceReq := req
		sourceReq.Limit = fetch
		candidates, e := s.d.Source.Rows(ctx, sourceReq)
		if e != nil {
			return nil, e
		}
		out = make([]frontend.Row, 0, len(candidates))
		expand := false
		for _, row := range candidates {
			if !s.d.AllowRow(ctx, req.Viewer, row) {
				continue
			}
			entries := make([]frontend.RowEntry, 0, len(row.Entries))
			seen := make(map[string]bool, len(row.Entries))
			for _, entry := range row.Entries {
				entry.Handle = ""
				id := entryIdentity(entry)
				key := string(entry.Kind) + ":" + id
				if id != "" && !seen[key] && s.permitted(ctx, req.Viewer, entry) {
					seen[key] = true
					entries = append(entries, entry)
				}
			}
			if len(entries) < limit && len(row.Entries) >= fetch {
				expand = true
			}
			row.Items = nil
			row.Handle, row.ImpressionID = "", ""
			if len(entries) > limit {
				entries, row.More = entries[:limit], true
			}
			row.Entries = entries
			if len(entries) > 0 {
				out = append(out, row)
			}
		}
		if !expand || fetch >= 1000 || ctx.Err() != nil {
			break
		}
		fetch = min(fetch*2, 1000)
	}
	if s.d.Layout != nil {
		candidates := make([]LayoutCandidate, len(out))
		layoutInput := make([]frontend.Row, len(out))
		for i, row := range out {
			candidates[i] = LayoutCandidate{ID: row.ID, Title: row.Title, Purpose: row.Purpose, ItemCount: len(row.Entries)}
			layoutInput[i] = row
			layoutInput[i].Entries = append([]frontend.RowEntry(nil), row.Entries...)
		}
		var placements []frontend.Row
		indices, e := ArrangeLayout(candidates, func(_ []LayoutCandidate) ([]string, error) {
			var err error
			placements, err = s.d.Layout(ctx, req, layoutInput)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(placements))
			for _, placement := range placements {
				ids = append(ids, placement.ID)
			}
			return ids, nil
		})
		if e != nil {
			return nil, e
		}
		byID := make(map[string]frontend.Row, len(placements))
		for _, placement := range placements {
			byID[placement.ID] = placement
		}
		original := out
		out = make([]frontend.Row, 0, len(indices))
		for _, index := range indices {
			row := original[index]
			placement := byID[row.ID]
			row.Layout, row.Style, row.Rank = placement.Layout, placement.Style, placement.Rank
			out = append(out, row)
		}
	}

	// Layout only sees permitted rows; reject injected or altered rows rather than
	// trusting a layout provider to manufacture authorization decisions.
	for i := range out {
		row := &out[i]
		if !s.d.AllowRow(ctx, req.Viewer, *row) {
			return nil, errors.New("experience: layout returned unauthorized row")
		}
		row.Items = nil
		for j := range row.Entries {
			if entryIdentity(row.Entries[j]) == "" || !s.permitted(ctx, req.Viewer, row.Entries[j]) {
				return nil, errors.New("experience: layout returned unauthorized entry")
			}
		}
		row.Handle, err = s.d.Receipts.Issue(ctx, req.Viewer, req.Device, protocol, ReceiptSubject{Kind: "row", ID: row.ID, Purpose: row.Purpose, Rank: row.Rank})
		if err != nil {
			return nil, err
		}
		for j := range row.Entries {
			entry := &row.Entries[j]
			id := row.ID + "/" + string(entry.Kind) + "/" + entryIdentity(*entry)
			entry.Handle, err = s.d.Receipts.Issue(ctx, req.Viewer, req.Device, protocol, ReceiptSubject{Kind: string(entry.Kind), ID: id, Purpose: row.Purpose, Rank: row.Rank})
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (s *RowsService) permitted(ctx context.Context, viewer *core.User, entry frontend.RowEntry) bool {
	if !s.d.AllowEntry(ctx, viewer, entry) {
		return false
	}
	switch entry.Kind {
	case frontend.RowEntryTitle:
		return s.d.Visibility.Visible(ctx, viewer, entry.Title)
	case frontend.RowEntryCollection:
		return s.d.Visibility.CollectionAllowed(viewer, entry.Collection)
	case frontend.RowEntryPlaylist:
		return entry.Playlist.UserID == viewer.ID
	default:
		return true
	}
}

func entryIdentity(e frontend.RowEntry) string {
	n := 0
	for _, present := range []bool{e.Title != nil, e.Collection != nil, e.Playlist != nil, e.LiveChannel != nil, e.Directory != nil} {
		if present {
			n++
		}
	}
	if n != 1 {
		return ""
	}
	switch e.Kind {
	case frontend.RowEntryTitle:
		if e.Title != nil && e.Title.Key != 0 {
			return e.Title.Key.String()
		}
	case frontend.RowEntryCollection:
		if e.Collection != nil && e.Collection.Key != 0 {
			return e.Collection.Key.String()
		}
	case frontend.RowEntryPlaylist:
		if e.Playlist != nil && e.Playlist.Key != 0 {
			return e.Playlist.Key.String()
		}
	case frontend.RowEntryLiveChannel:
		if e.LiveChannel != nil {
			return e.LiveChannel.ID
		}
	case frontend.RowEntryDirectory:
		if e.Directory != nil {
			return e.Directory.ID
		}
	}
	return ""
}
