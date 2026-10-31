package experience

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
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
	Situation  func(context.Context, frontend.RowRequest) (core.HomeSituation, error)
	// HomeFallback supplies bounded library rows when a neutral Home has no
	// discovery shelves left after policy filtering. Plex does not use it.
	HomeFallback func(context.Context, frontend.RowRequest) ([]frontend.Row, error)
	// RankedSource disables retries when the source ranks policy-filtered candidates once.
	RankedSource bool
}

// RowsService produces viewer-scoped, policy-checked rows.
type RowsService struct {
	d RowServiceDeps
}

func NewRowsService(d RowServiceDeps) (*RowsService, error) {
	if d.Source == nil || d.Active == nil || d.AllowRow == nil || d.AllowEntry == nil || d.Receipts == nil || d.Situation == nil || d.Visibility.GetTitle == nil || d.Visibility.SectionFor == nil {
		return nil, errors.New("experience: incomplete row dependencies")
	}
	return &RowsService{d: d}, nil
}

// Rows filters each tagged entry before limiting and never accepts client-made
// interaction identifiers. Candidate requests expand to a bounded prefix when
// filtering would otherwise leave a row short.
func (s *RowsService) Rows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol) ([]frontend.Row, error) {
	return s.rows(ctx, req, protocol, true)
}

// SnapshotRows builds and authorizes the same ranked rows without issuing
// impressions; a paged row view issues receipts only for delivered entries.
func (s *RowsService) SnapshotRows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol) ([]frontend.Row, error) {
	return s.rows(ctx, req, protocol, false)
}

func (s *RowsService) rows(ctx context.Context, req frontend.RowRequest, protocol core.Protocol, issue bool) ([]frontend.Row, error) {
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
		if req.Purpose != "" {
			kept := make([]frontend.Row, 0, len(candidates))
			for _, row := range candidates {
				if row.Purpose == req.Purpose {
					kept = append(kept, row)
				}
			}
			candidates = kept
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
		if s.d.RankedSource || !expand || fetch >= 1000 || ctx.Err() != nil {
			break
		}
		fetch = min(fetch*2, 1000)
	}
	if req.Purpose == "" {
		out, err = s.appendHomeFallback(ctx, req, protocol, out, limit)
		if err != nil {
			return nil, err
		}
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

	situation, err := s.d.Situation(ctx, req)
	if err != nil {
		return nil, err
	}

	// Layout only sees permitted rows; reject injected or altered rows rather than
	// trusting a layout provider to manufacture authorization decisions.
	var subjects []ReceiptSubject
	var handles []*string
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
		if !issue {
			continue
		}
		impression := &core.NeutralImpression{RowID: row.ID, Kind: row.Kind, Title: row.Title, Purpose: row.Purpose, Rank: row.Rank, Situation: situation, Collection: row.Collection, Item: req.Item, Placement: string(req.Placement), ViewID: req.ViewID}
		for _, entry := range row.Entries {
			impression.Entries = append(impression.Entries, core.NeutralEntryRef{Kind: string(entry.Kind), ID: entryIdentity(entry)})
			if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
				impression.Items = append(impression.Items, entry.Title.Key)
			}
		}
		subjects = append(subjects, ReceiptSubject{Kind: "row", ID: row.ID, Purpose: row.Purpose, Rank: row.Rank, Impression: impression})
		handles = append(handles, &row.Handle)
		for j := range row.Entries {
			entry := &row.Entries[j]
			id := row.ID + "/" + string(entry.Kind) + "/" + entryIdentity(*entry)
			subjects = append(subjects, ReceiptSubject{Kind: string(entry.Kind), ID: id, Purpose: row.Purpose, Rank: row.Rank, Impression: impression})
			handles = append(handles, &entry.Handle)
		}
	}
	if len(subjects) > 0 {
		tokens, e := s.d.Receipts.IssueBatch(ctx, req.Viewer, req.Device, protocol, subjects)
		// IssueBatch already retries one overlapping policy write. A second
		// catalog write can land between those attempts; read the epoch again.
		if errors.Is(e, viewerpolicy.ErrChanged) {
			tokens, e = s.d.Receipts.IssueBatch(ctx, req.Viewer, req.Device, protocol, subjects)
		}
		if e != nil {
			return nil, e
		}
		for i := range handles {
			*handles[i] = tokens[i]
		}
	}
	return out, nil
}

// appendHomeFallback adds policy-checked library shelves when Home is only
// Continue Watching and Next Up. Plex Home is assembled by its own adapter
// and stays byte-identical, so this never runs for that protocol.
func (s *RowsService) appendHomeFallback(ctx context.Context, req frontend.RowRequest, protocol core.Protocol, out []frontend.Row, limit int) ([]frontend.Row, error) {
	if req.Placement != frontend.PlacementHome || protocol == "plex" || s.d.HomeFallback == nil {
		return out, nil
	}
	for _, row := range out {
		if row.Purpose != frontend.RowContinue && row.Purpose != frontend.RowNextUp {
			return out, nil
		}
	}
	fallback, err := s.d.HomeFallback(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, row := range fallback {
		if !s.d.AllowRow(ctx, req.Viewer, row) {
			continue
		}
		entries := make([]frontend.RowEntry, 0, min(len(row.Entries), limit))
		seen := map[string]bool{}
		for _, entry := range row.Entries {
			id := string(entry.Kind) + ":" + entryIdentity(entry)
			if entryIdentity(entry) == "" || seen[id] || !s.permitted(ctx, req.Viewer, entry) {
				continue
			}
			seen[id] = true
			entry.Handle = ""
			if len(entries) == limit {
				row.More = true
				break
			}
			entries = append(entries, entry)
		}
		row.Items, row.Handle, row.ImpressionID, row.Entries = nil, "", "", entries
		if len(entries) > 0 {
			out = append(out, row)
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

// RowEntryIdentity returns the stable neutral identity of a well-formed entry.
func RowEntryIdentity(e frontend.RowEntry) string { return entryIdentity(e) }
