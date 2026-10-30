package experience

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRowViewCurrentAvailabilityUsesOneSnapshotBatch(t *testing.T) {
	db := store.OpenTest(t)
	for _, ddl := range []string{`CREATE TABLE IF NOT EXISTS personalize_home_layout(id INTEGER PRIMARY KEY,document TEXT)`, `CREATE TABLE IF NOT EXISTS favorites_mirror(viewer_id INTEGER PRIMARY KEY,updated_at INTEGER)`, `CREATE TABLE IF NOT EXISTS notinterested_revisions(viewer_id INTEGER PRIMARY KEY,revision INTEGER)`} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	receipts, err := NewReceipts(relayOptions(db, &now, 100, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	titles := map[core.RatingKey]core.Title{}
	var entries []frontend.RowEntry
	for i := 1; i <= 25; i++ {
		title := core.Title{Key: core.MovieKey(int64(i)), Type: core.TypeMovie, Title: fmt.Sprint(i), Released: now.Add(-time.Hour)}
		titles[title.Key] = title
		copy := title
		entries = append(entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &copy})
	}
	calls, size := 0, 0
	runtime := RuntimeRowViews{DB: db, Protocol: "jellyfin", Receipts: receipts, Active: func(context.Context, int64) (*core.User, error) { return &core.User{ID: 7, IsOwner: true}, nil },
		CachedGet: func(_ context.Context, key core.RatingKey) (*core.Title, error) {
			title := titles[key]
			return &title, nil
		},
		CachedGetMany: func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			out := map[core.RatingKey]core.Title{}
			for _, key := range keys {
				if title, ok := titles[key]; ok {
					out[key] = title
				}
			}
			return out, nil
		},
		CurrentAvailable: func(_ context.Context, batch []core.Title) []core.Title { calls++; size = len(batch); return batch },
		Situation:        func(frontend.RowRequest) core.HomeSituation { return core.HomeSituation{} },
		Snapshot: func(context.Context, frontend.RowRequest, core.HomeSituation) ([]frontend.Row, error) {
			return []frontend.Row{{ID: "r", Kind: "taste", Purpose: frontend.RowRecommendation, Entries: entries}}, nil
		},
	}
	service, err := NewRuntimeRowViews(runtime)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	viewer, device := &core.User{ID: 7, IsOwner: true}, core.Device{ID: "tv"}
	views, err := service.ListRowViews(ctx, frontend.RowViewRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", Placement: frontend.PlacementHome})
	if err != nil || len(views) != 1 {
		t.Fatalf("views=%v %v", views, err)
	}
	page, err := service.ResolveRowView(ctx, frontend.RowViewPageRequest{Viewer: viewer, Device: device, Protocol: "jellyfin", ID: views[0].ID, Limit: 5})
	if err != nil || len(page.Entries) != 5 {
		t.Fatalf("page=%+v %v", page, err)
	}
	if calls != 1 || size != 25 {
		t.Fatalf("fresh availability calls=%d batch=%d", calls, size)
	}
}
