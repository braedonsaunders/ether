package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

// PreviewRows composes the owner preview using the same source ordering and section projection.
type PreviewRows struct {
	Availability func(context.Context, int) []core.Hub
	Filter       func(context.Context, *core.User, []core.Title) ([]core.Title, error)
	Watchlist    func(context.Context, *core.User, int) ([]core.Hub, error)
	Personal     func(context.Context, *core.User, int) ([]core.Hub, error)
	Live         func(context.Context, *core.User, int) (core.Hub, error)
	LiveEnabled  func() bool
}

func (a PreviewRows) Rows(ctx context.Context, u *core.User, hc core.HubContext, section string, limit int) []core.Hub {
	var out []core.Hub
	if a.Availability != nil {
		if a.Filter != nil {
			out = append(out, PreviewAvailability(ctx, u, limit, a.Availability, a.Filter)...)
		} else {
			out = append(out, a.Availability(ctx, limit)...)
		}
	}
	if a.Watchlist != nil {
		rows, _ := a.Watchlist(ctx, u, limit)
		out = append(out, rows...)
	}
	if a.Personal != nil {
		rows, _ := a.Personal(ctx, u, limit)
		out = append(out, rows...)
	}
	if section == "home" && a.Live != nil && a.LiveEnabled != nil && a.LiveEnabled() {
		row, err := a.Live(ctx, u, limit)
		if err == nil && len(row.Items) > 0 {
			out = append(out, row)
		}
	}
	if section == "movies" || section == "shows" {
		typ := core.TypeMovie
		if section == "shows" {
			typ = core.TypeShow
		}
		filtered := out[:0]
		for _, h := range out {
			if v, ok := core.SectionView(h, typ); ok {
				filtered = append(filtered, v)
			}
		}
		out = filtered
	}
	return out
}

// PreviewAvailability filters suggested titles before the preview's row cap.
// A viewer with no exclusions retains the source's exact first response.
func PreviewAvailability(ctx context.Context, u *core.User, limit int, load func(context.Context, int) []core.Hub, filter func(context.Context, *core.User, []core.Title) ([]core.Title, error)) []core.Hub {
	if limit <= 0 {
		hubs := load(ctx, limit)
		out := make([]core.Hub, 0, len(hubs))
		for _, hub := range hubs {
			items, err := filter(ctx, u, hub.Items)
			if err != nil {
				return nil
			}
			hub.Items = items
			out = append(out, hub)
		}
		return out
	}
	fetch := limit
	for {
		hubs := load(ctx, fetch)
		out := make([]core.Hub, 0, len(hubs))
		removed, expand := false, false
		for _, hub := range hubs {
			items, err := filter(ctx, u, hub.Items)
			if err != nil {
				return nil
			}
			if len(items) < len(hub.Items) {
				removed = true
			}
			if len(items) < limit && len(hub.Items) >= fetch {
				expand = true
			}
			hub.Items = items
			out = append(out, hub)
		}
		if fetch == limit && !removed {
			return hubs
		}
		if !expand || fetch >= 1000 || ctx.Err() != nil {
			for i := range out {
				if len(out[i].Items) > limit {
					out[i].Items = out[i].Items[:limit]
				}
			}
			return out
		}
		fetch = min(fetch*2, 1000)
	}
}
