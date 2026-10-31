package personalize

import (
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Plex count is a card window, not permission to remove otherwise full shelves.
func TestShelfIdentitySurvivesSmallCardWindows(t *testing.T) {
	for _, spec := range []struct {
		kind, id string
		minimum  int
	}{
		{KindTopRated, "toprated", 5}, {KindShowcase, "showcase.comedy", 6},
	} {
		t.Run(spec.id, func(t *testing.T) {
			cat := newFakeCatalog(testNow)
			var keys []core.RatingKey
			for i := 1; i <= 12; i++ {
				keys = append(keys, movieKey(0, i))
			}
			row := testRow(spec.kind, spec.id, items(cat, keys...))
			row.minItems = spec.minimum
			row.coldHome = true
			for _, count := range []int{12, 4, 1, 5, 6, 12} {
				t.Run(fmt.Sprint(count), func(t *testing.T) {
					rows := run(emptyModel(row), func(a *assembly) { a.limit = count; a.minRow = 12 })
					if len(rows) != 1 || rows[0].HubID != "home."+spec.id || len(rows[0].Items) != count {
						t.Fatalf("count=%d: rows=%v; want same shelf with %d cards", count, kindsOf(rows), count)
					}
				})
			}
			// A genuine shortage still suppresses the row at the normal window.
			row.items = row.items[:3]
			if rows := run(emptyModel(row), func(a *assembly) { a.limit = 12; a.minRow = 12 }); len(rows) != 0 {
				t.Fatalf("genuinely thin shelf retained: %v", kindsOf(rows))
			}
		})
	}
}

func TestOwnerCollectionRowExemptFromEngineMinRowItemsFloor(t *testing.T) {
	cat := newFakeCatalog(testNow)
	var keys []core.RatingKey
	for i := 1; i <= 5; i++ {
		keys = append(keys, movieKey(0, i))
	}
	cands := items(cat, keys...)
	hub := core.Hub{
		ID:       "collection.my-list",
		Title:    "My List",
		Style:    "shelf",
		Promoted: true,
	}
	ownerRow := &candRow{
		kind:      KindCollectionPrefix + "my-list",
		id:        hub.ID,
		title:     hub.Title,
		source:    core.RowSourceOwner,
		items:     cands,
		minItems:  3,
		keepOrder: true,
		scale:     1,
		hub:       &hub,
	}
	rows := run(emptyModel(), func(a *assembly) {
		a.owner = []*candRow{ownerRow}
		a.limit = 12
		a.minRow = 12
	})
	if len(rows) != 1 || rows[0].HubID != "collection.my-list" || len(rows[0].Items) != 5 {
		t.Fatalf("owner collection row dropped under minRow=12: %+v", rows)
	}
}

func TestPresentedShelfSurvivesWiderCountExpansion(t *testing.T) {
	cat := newFakeCatalog(testNow)
	var keys []core.RatingKey
	for g := 0; g < 2; g++ {
		for i := 1; i <= 12; i++ {
			keys = append(keys, movieKey(g, i))
		}
	}
	// At count=8, both rows get 8 items. At count=50, trending takes up to 16
	// items, leaving showcase with only its 8 previously presented items (<12).
	topKeys := keys[:16]
	showcaseKeys := keys[5:16] // 11 items total; 8 unique when trending only took 8
	top := testRow(KindTrending, "trending", items(cat, topKeys...))
	showcase := testRow(KindShowcase, "showcase.crime", items(cat, showcaseKeys...))
	showcase.minItems = 6
	showcase.coldOnly = true

	e := &Engine{opts: Options{Now: func() time.Time { return testNow }}}
	makeA := func(limit int, generic bool) *assembly {
		m := emptyModel(top, showcase)
		m.generic = generic
		return &assembly{
			model:   m,
			user:    core.User{ID: 1},
			hc:      hubContext(testNow, core.DeviceWeb),
			limit:   limit,
			maxRows: 20,
			minRow:  12,
			day:     dayOf(testNow, time.UTC),
		}
	}
	first := e.assemblePage(makeA(8, true), memoKey{user: 1, limit: 8, model: 1}, false)
	if len(first) != 2 {
		t.Fatalf("initial count=8 rows=%v; want 2", kindsOf(first))
	}
	// Subsequent /hubs/items fetch at count=50 after personal cold-start model finishes.
	second := e.assemblePage(makeA(50, false), memoKey{user: 1, limit: 50, model: 2}, false)
	var found bool
	for _, r := range second {
		if r.HubID == "home.showcase.crime" && len(r.Items) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("home.showcase.crime disappeared on count=50 expansion: %v", kindsOf(second))
	}
}
