package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// PostPlaySelection preserves the established owner selection semantics while
// restricted viewers use inherited visibility before deduplication and limits.
// Unrestricted is a trusted host policy decision, never a request option.
type PostPlaySelection struct {
	Visibility   Visibility
	Unrestricted bool
	Picks        func(context.Context, int) []core.PostPlayPick
	Related      func(context.Context, int) ([]core.Hub, error)
}

func (s PostPlaySelection) Row(ctx context.Context, viewer *core.User, count int, seen map[core.RatingKey]bool) PostPlayRow {
	if !s.Unrestricted {
		return LoadPostPlay(ctx, viewer, s.Visibility, count, seen, s.Picks)
	}
	picks := s.Picks(ctx, count)
	items := make([]core.Title, 0, len(picks))
	for _, pick := range picks {
		if !seen[pick.Title.Key] {
			seen[pick.Title.Key] = true
			items = append(items, pick.Title)
		}
	}
	kept := items[:0:0]
	for _, item := range items {
		if s.Visibility.TitleAllowed(viewer, &item) {
			kept = append(kept, item)
		}
	}
	return PostPlayRow{Purpose: "recommendation", Items: kept}
}

func (s PostPlaySelection) RelatedRows(ctx context.Context, viewer *core.User, count int, seen map[core.RatingKey]bool) ([]core.Hub, error) {
	if s.Related == nil {
		return nil, nil
	}
	if s.Unrestricted {
		hubs, err := s.Related(ctx, count)
		if err != nil {
			return nil, err
		}
		var out []core.Hub
		for _, hub := range hubs {
			items := hub.Items[:0:0]
			for _, item := range hub.Items {
				if !seen[item.Key] {
					seen[item.Key] = true
					items = append(items, item)
				}
			}
			if len(items) > 0 {
				hub.Items = items
				out = append(out, hub)
			}
		}
		return out, nil
	}
	var hubs []core.Hub
	rows, err := LoadRelated(ctx, viewer, s.Visibility, count, seen, func(ctx context.Context, fetch int) ([]RelatedCandidate, error) {
		var err error
		hubs, err = s.Related(ctx, fetch)
		if err != nil {
			return nil, err
		}
		rows := make([]RelatedCandidate, len(hubs))
		for i, hub := range hubs {
			rows[i] = RelatedCandidate{Index: i, Items: hub.Items}
		}
		return rows, nil
	})
	if err != nil {
		return nil, err
	}
	var out []core.Hub
	for _, row := range rows {
		hub := hubs[row.Index]
		hub.Items = row.Items
		out = append(out, hub)
	}
	return out, nil
}

// AssemblePostPlay retains next-up, personal picks and related row ordering.
// Rendering runs at each original boundary, before fetching the next source.
func AssemblePostPlay[P any](ctx context.Context, selection PostPlaySelection, viewer *core.User, key core.RatingKey, count int,
	next func() (core.RatingKey, P, bool), picks func(PostPlayRow) (P, bool), render func(core.Hub) P) ([]P, error) {
	var out []P
	seen := map[core.RatingKey]bool{key: true}
	if key, row, ok := next(); ok {
		out = append(out, row)
		seen[key] = true
	}
	if row := selection.Row(ctx, viewer, count, seen); len(row.Items) > 0 {
		if projected, ok := picks(row); ok {
			out = append(out, projected)
		}
	}
	related, err := selection.RelatedRows(ctx, viewer, count, seen)
	if err != nil {
		return out, err
	}
	for _, row := range related {
		out = append(out, render(row))
	}
	return out, nil
}
