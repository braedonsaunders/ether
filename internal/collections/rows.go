package collections

import (
	"context"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// rowItems is how many titles a collection row carries per media type.
const rowItems = 24

// rowPageKeys caps keys for a Home or section shelf. Mixed collections keep up
// to rowItems movies and up to rowItems shows so neither section's shelf is
// starved when the other media type clusters near the front of the list.
func rowPageKeys(keys []core.RatingKey, mediaType string) ([]core.RatingKey, bool) {
	if mediaType != MediaMixed {
		if len(keys) <= rowItems {
			return keys, false
		}
		return keys[:rowItems], true
	}
	var movies, shows int
	var dropped bool
	out := make([]core.RatingKey, 0, min(len(keys), rowItems*2))
	for _, k := range keys {
		switch k.Kind() {
		case core.KindMovie:
			if movies >= rowItems {
				dropped = true
				continue
			}
			movies++
		case core.KindShow:
			if shows >= rowItems {
				dropped = true
				continue
			}
			shows++
		default:
			if len(out) >= rowItems*2 {
				dropped = true
				continue
			}
		}
		out = append(out, k)
	}
	return out, dropped
}

// HomeRowsFor returns owner collections promoted to Home or section rows
// for a user in a context: enabled, targeted at the user, with the user's
// content-rating limit, personal rules, personal order and rotation
// applied. Each row says whether it belongs on Home, in section hubs, or
// both, and carries the collection's schedule, which the caller applies
// (a viewer's Home override may replace it); Hub.Type tells the caller
// which section ("" for mixed collections).
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
		page, dropped := rowPageKeys(keys, sp.MediaType)
		items, err := s.mirror.Titles(ctx, page)
		if err != nil || len(items) == 0 {
			continue
		}
		hub := core.Hub{
			ID: "collection." + row.publicID(), Title: sp.Name, Style: "shelf", PreviewOnFocus: sp.PreviewOnFocus, RankedTop10: sp.RankedTop10,
			Promoted: sp.Targets.HomeRow, Items: items, More: dropped || len(keys) > len(items), Key: row.key(),
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
			HomeRow: sp.Targets.HomeRow, SectionRow: sp.Targets.SectionRow, Schedule: sp.Targets.Schedule})
	}
	return out, nil
}

// LayoutRow serves a collection explicitly selected by the owner's Home
// template or viewer override, even when its default publication targets do
// not include Home. Private ownership and viewer content limits still apply.
// The layout policy supplies the publication scope; the row carries the
// collection's schedule for the caller to apply.
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
	page, dropped := rowPageKeys(keys, row.spec.MediaType)
	items, err := s.mirror.Titles(ctx, page)
	if err != nil {
		return nil, err
	}
	hub := core.Hub{ID: "collection." + row.publicID(), Title: row.spec.Name, Style: "shelf", PreviewOnFocus: row.spec.PreviewOnFocus, RankedTop10: row.spec.RankedTop10, Promoted: true, Items: items, More: dropped || count > len(items), Key: row.key(), Context: "hub.home.collection"}
	switch row.spec.MediaType {
	case MediaMovie:
		hub.Type = core.TypeMovie
	case MediaShow:
		hub.Type = core.TypeShow
	}
	return &core.CollectionRow{Hub: hub, CollectionID: row.publicID(), HomeRow: true, SectionRow: true, Schedule: row.spec.Targets.Schedule}, nil
}
