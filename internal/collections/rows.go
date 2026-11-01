package collections

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// rowItems is how many titles a collection row carries.
const rowItems = 24

// HomeRowsFor returns owner collections promoted to Home or section rows
// for a user in a context: enabled, targeted at the user, inside their
// schedule (days, hours, devices), with the user's content-rating limit,
// personal rules, personal order and rotation applied. Each row says
// whether it belongs on Home, in section hubs, or both; Hub.Type tells the
// caller which section ("" for mixed collections).
// homeIndexWait bounds how long a home waits for the catalogue index's
// first build before it skips the live rows that need it.
const homeIndexWait = 300 * time.Millisecond

// homeRowWait bounds how long a single collection row may block Home or
// section row assembly before its build finishes in the background.
const homeRowWait = 500 * time.Millisecond

func (s *Service) HomeRowsFor(ctx context.Context, user *core.User, hc core.HubContext) ([]core.CollectionRow, error) {
	rows, err := s.loadRows(ctx)
	if err != nil {
		return nil, err
	}
	now := hc.Now
	if now.IsZero() {
		now = s.now()
	}
	var out []core.CollectionRow
	var indexReady *bool
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		sp := row.spec
		if !visible(row, user) || (!sp.Targets.HomeRow && !sp.Targets.SectionRow) {
			continue
		}
		if !scheduleAllows(sp.Targets.Schedule, hc.Device, now) {
			continue
		}
		// A live row is evaluated on request, but a home does not wait out
		// the catalogue index's first build, which reads the whole
		// catalogue: past homeIndexWait it skips the row until it is ready.
		live := false
		if sp.Refresh == RefreshLive {
			if indexReady == nil {
				ready := s.idx.readyWithin(ctx, homeIndexWait) != nil
				indexReady = &ready
			}
			live = *indexReady
		}
		rctx, rcancel := context.WithTimeout(ctx, homeRowWait)
		keys, err := s.serve(rctx, row, user, live)
		rcancel()
		if err != nil {
			if !errors.Is(err, errNotBuilt) {
				s.log.Debug("collections: row", "id", row.publicID(), "err", err)
			}
			continue
		}
		if len(keys) == 0 {
			continue
		}
		page := keys
		if len(page) > rowItems {
			page = page[:rowItems]
		}
		items, err := loadTitles(ctx, s.db, page)
		if err != nil || len(items) == 0 {
			continue
		}
		hub := core.Hub{
			ID: "collection." + row.publicID(), Title: sp.Name, Style: "shelf",
			Promoted: sp.Targets.HomeRow, Items: items, More: len(keys) > len(items), Key: row.key(),
		}
		switch sp.MediaType {
		case MediaMovie:
			hub.Type, hub.Context = core.TypeMovie, "hub.movie.collection"
		case MediaShow:
			hub.Type, hub.Context = core.TypeShow, "hub.tv.collection"
		default:
			hub.Context = "hub.home.collection"
		}
		if sp.Targets.HomeRow {
			hub.Context = "hub.home.collection"
		}
		out = append(out, core.CollectionRow{Hub: hub, CollectionID: row.publicID(),
			HomeRow: sp.Targets.HomeRow, SectionRow: sp.Targets.SectionRow})
	}
	return out, nil
}

// scheduleAllows applies a row schedule. Days are 0 (Sunday) to 6; hours
// are local, FromHour inclusive, ToHour exclusive, wrapping past midnight.
// An unknown device passes a device filter.
func scheduleAllows(sc *core.CollectionSchedule, device string, now time.Time) bool {
	if sc == nil {
		return true
	}
	if len(sc.Days) > 0 {
		ok := false
		for _, d := range sc.Days {
			if d == int(now.Weekday()) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if sc.FromHour != nil || sc.ToHour != nil {
		from, to := 0, 24
		if sc.FromHour != nil {
			from = *sc.FromHour
		}
		if sc.ToHour != nil {
			to = *sc.ToHour
		}
		h := now.Hour()
		switch {
		case from == to:
			// All day.
		case from < to:
			if h < from || h >= to {
				return false
			}
		default: // wraps midnight, e.g. 22 → 2
			if h < from && h >= to {
				return false
			}
		}
	}
	if len(sc.Devices) > 0 && device != "" {
		ok := false
		for _, d := range sc.Devices {
			if d == device {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// LayoutRow serves a collection explicitly selected by the owner's Home
// template or viewer override, even when its default publication targets do
// not include Home. Private ownership and viewer content limits still apply.
// The layout policy supplies the publication scope and schedule.
func (s *Service) LayoutRow(ctx context.Context, user *core.User, hc core.HubContext, id string) (*core.CollectionRow, error) {
	row, err := s.rowByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !row.enabled || row.owner != 0 && (user == nil || row.owner != user.ID) {
		return nil, core.ErrNotFound
	}
	rctx, rcancel := context.WithTimeout(ctx, homeRowWait)
	keys, err := s.serve(rctx, row, user, true)
	rcancel()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	count := len(keys)
	if len(keys) > rowItems {
		keys = keys[:rowItems]
	}
	items, err := loadTitles(ctx, s.db, keys)
	if err != nil {
		return nil, err
	}
	hub := core.Hub{ID: "collection." + row.publicID(), Title: row.spec.Name, Style: "shelf", Promoted: true, Items: items, More: count > len(items), Key: row.key(), Context: "hub.home.collection"}
	switch row.spec.MediaType {
	case MediaMovie:
		hub.Type = core.TypeMovie
	case MediaShow:
		hub.Type = core.TypeShow
	}
	return &core.CollectionRow{Hub: hub, CollectionID: row.publicID(), HomeRow: true, SectionRow: true}, nil
}
