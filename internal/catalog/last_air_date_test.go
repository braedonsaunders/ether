package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

func TestShowLastAirDateStoredAndPreserved(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	detail := &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: 51, Name: "Show", FirstAirDate: "2001-01-01"}, Status: "Ended", LastAirDate: "2020-02-03"}
	title := c.tvDetailsToTitle(ctx, detail)
	want := time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)
	if !title.LastAirDate.Equal(want) || title.Status != "Ended" {
		t.Fatalf("detail conversion: %+v", title)
	}
	if err := c.upsert(ctx, title, upsertOpts{browsable: true, detail: true}); err != nil {
		t.Fatal(err)
	}
	// A search/sync summary has no last-air date and must not erase detail.
	if err := c.UpsertTitle(ctx, core.Title{Key: title.Key, Type: core.TypeShow, Title: "Show"}, true); err != nil {
		t.Fatal(err)
	}
	got, err := c.CachedGet(ctx, title.Key)
	if err != nil || !got.LastAirDate.Equal(want) || got.Status != "Ended" {
		t.Fatalf("cached after summary: %+v %v", got, err)
	}
	m := NewMirror(c.db, time.Now)
	items, err := m.Titles(ctx, []core.RatingKey{title.Key})
	if err != nil || len(items) != 1 || !items[0].LastAirDate.Equal(want) {
		t.Fatalf("mirror: %+v %v", items, err)
	}
	for _, date := range []string{"", "not-a-date", "2020-02-31"} {
		detail.LastAirDate = date
		unknown := c.tvDetailsToTitle(ctx, detail)
		if !unknown.LastAirDate.IsZero() {
			t.Fatalf("invalid date %q: %v", date, unknown.LastAirDate)
		}
	}
}
