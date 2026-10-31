package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
)

type premiereNoNetwork struct{ t *testing.T }

func (f premiereNoNetwork) FeedSources() []core.ReleaseFeed {
	return []core.ReleaseFeed{{ID: "f", Name: "Fake", Generation: "one"}}
}
func (f premiereNoNetwork) Recent(context.Context, string, int, int) ([]core.Release, error) {
	f.t.Fatal("unexpected feed request")
	return nil, nil
}

func TestPremiereVisibilityBeforePaginationAndSearch(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults(t.TempDir())
	cfg.Usenet.Providers = []config.UsenetProvider{{Host: "fake", Enabled: true}}
	c, err := catalog.New(db, nil, cfg.Catalog, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"A Empty", "B Future Child", "C Unknown Child", "D Premiere", "E Classic"} {
		date := now.Add(-time.Hour)
		if i == 4 {
			date = now.Add(-48 * time.Hour)
		}
		title := core.Title{Key: core.ShowKey(int64(i + 1)), Type: core.TypeShow, Title: name, Released: date, Language: "en"}
		if err = c.UpsertTitle(t.Context(), title, true); err != nil {
			t.Fatal(err)
		}
	}
	// Preserve unknown/future children in the local metadata-only query;
	// neither may be treated as an unmirrored S01E01 premiere.
	for _, id := range []int64{2, 3, 4} {
		date := now.Add(-time.Hour)
		if id == 2 {
			date = now.Add(time.Hour)
		}
		if id == 3 {
			date = time.Time{}
		}
		ep := core.Title{Key: core.EpisodeKey(id, 1, 1), Type: core.TypeEpisode, ShowKey: core.ShowKey(id), ShowTitle: []string{"", "", "B Future Child", "C Unknown Child", "D Premiere"}[id], Season: 1, Episode: 1, Released: date}
		if err = c.UpsertTitle(t.Context(), ep, false); err != nil {
			t.Fatal(err)
		}
	}
	r := resolver.New(nil, nil, nil, cfg, resolver.Options{})
	defer r.Close()
	a, err := availability.New(availability.Deps{DB: db, Config: func() config.Config { return cfg }, Feeds: premiereNoNetwork{t}, Catalog: c, Validator: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := c.CachedTitle(t.Context(), core.EpisodeKey(4, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	a.RecordSearch(t.Context(), ep, []core.Release{{ID: "proof", Name: "D.Premiere.S01E01.1080p.WEB-DL", Indexer: "Fake"}})
	wrapped := availabilityCatalog{c, a}
	for offset, want := range []core.RatingKey{core.ShowKey(4), core.ShowKey(5)} {
		p, e := wrapped.Browse(t.Context(), core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow}, Sort: core.SortTitle, Limit: 1, Offset: offset})
		if e != nil || p.Total != 2 || len(p.Items) != 1 || p.Items[0].Key != want {
			t.Fatalf("page %d: %+v %v", offset, p, e)
		}
	}
	got, err := wrapped.Search(t.Context(), "Empty", []core.MediaType{core.TypeShow}, 20)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty premiere in search: %+v %v", got, err)
	}
	children, err := c.CachedDiscoveryEpisodes(t.Context(), []core.RatingKey{core.ShowKey(2), core.ShowKey(3)})
	if err != nil || len(children[core.ShowKey(2)]) != 1 || len(children[core.ShowKey(3)]) != 1 {
		t.Fatalf("lost unreleased metadata: %+v %v", children, err)
	}
}

func TestAvailabilityCatalogFirstCharacterMatchesBrowse(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults(t.TempDir())
	cfg.Usenet.Providers = []config.UsenetProvider{{Host: "fake", Enabled: true}}
	c, err := catalog.New(db, nil, cfg.Catalog, slog.New(slog.DiscardHandler), catalog.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	oldRelease := now.Add(-120 * 24 * time.Hour)
	recentUnobserved := now.Add(-10 * 24 * time.Hour)
	var nextID int64 = 1
	add := func(title string, released time.Time) {
		id := nextID
		nextID++
		ti := core.Title{
			Key:      core.MovieKey(id),
			Type:     core.TypeMovie,
			Title:    title,
			Released: released,
			Language: "en",
		}
		if err := c.UpsertTitle(ctx, ti, true); err != nil {
			t.Fatal(err)
		}
	}
	// 6 visible "#" movies (outside the 60-day evidence window).
	for _, title := range []string{"#Alive", "(500) Days of Summer", "[REC]", "[REC]²", "[REC]³ Genesis", "[REC]⁴ Apocalypse"} {
		add(title, oldRelease)
	}
	// 6 hidden "#" movies (inside the 60-day evidence window without release observations).
	for _, title := range []string{"100 Days", "2 Valid IDs", "28 Kegs Later", "29 Seconds to Midnight", "The 3 Killer Pigs", "58th"} {
		add(title, recentUnobserved)
	}
	// First 7 visible "A" movies + 1 hidden "A" movie.
	for _, title := range []string{"A", "A", "The A-Team", "A-X-L", "A. Rimbaud", "A.C.A.B. - All Cops Are Bastards", "A.C.O.D."} {
		add(title, oldRelease)
	}
	add("A Hidden Recent Movie", recentUnobserved)
	for ch := byte('B'); ch <= 'Z'; ch++ {
		letter := string(ch)
		add(letter+" Visible Movie", oldRelease)
		add(letter+" Unobserved Recent", recentUnobserved)
	}

	r := resolver.New(nil, nil, nil, cfg, resolver.Options{})
	defer r.Close()
	a, err := availability.New(availability.Deps{DB: db, Config: func() config.Config { return cfg }, Feeds: premiereNoNetwork{t}, Catalog: c, Validator: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &availabilityCatalog{Catalog: c, availability: a}

	facets, err := wrapped.Facet(ctx, core.TypeMovie, "firstCharacter")
	if err != nil {
		t.Fatal(err)
	}
	page, err := wrapped.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortTitle, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	for _, fv := range facets {
		if fv.Key == "~" {
			continue
		}
		if offset >= len(page.Items) {
			t.Fatalf("bucket %s offset %d out of range (%d)", fv.Title, offset, len(page.Items))
		}
		first := page.Items[offset]
		if fv.Title == "#" && fv.Count != 6 {
			t.Fatalf("expected # count=6 (excluding 6 hidden recent movies), got %d", fv.Count)
		}
		if fv.Title == "A" && (offset != 6 || first.Title != "A") {
			t.Fatalf("expected A at offset 6 landing on \"A\", got offset %d title %q", offset, first.Title)
		}
		offset += fv.Count
	}
	if offset != page.Total {
		t.Fatalf("sum of firstCharacter counts (%d) != Browse total (%d)", offset, page.Total)
	}

	t.Run("LabCatalogReadOnly", func(t *testing.T) {
		const labPath = "/Users/braedonsaunders/.bb/thread-storage/thr_sm7nri5zya/local-prism/data/ether.db"
		if _, err := os.Stat(labPath); err != nil {
			t.Skipf("lab db not present: %v", err)
		}
		roDB, err := sql.Open("sqlite", "file:"+labPath+"?mode=ro&_query_only=1")
		if err != nil {
			t.Fatal(err)
		}
		defer roDB.Close()

		labCfg := config.Defaults(t.TempDir())
		labCfg.Catalog = config.Catalog{
			Exclusions: []core.LibraryExclusion{{
				Name:      "Foreign",
				MediaType: "movie",
				Match:     "all",
				Rules: []core.CollectionRule{
					{Field: "country", Op: "in", Value: json.RawMessage(`["IN","JP","CN"]`)},
				},
			}},
		}
		labCfg.Usenet.Providers = []config.UsenetProvider{{Host: "fake", Enabled: true}}
		labCat, err := catalog.New(roDB, nil, labCfg.Catalog, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		labAvail, err := availability.New(availability.Deps{
			DB:        roDB,
			Config:    func() config.Config { return labCfg },
			Feeds:     premiereNoNetwork{t},
			Catalog:   labCat,
			Validator: r,
		})
		if err != nil {
			t.Fatal(err)
		}
		labWrapped := &availabilityCatalog{Catalog: labCat, availability: labAvail}

		fc, err := labWrapped.FacetFilter(ctx, core.Filter{Type: core.TypeMovie}, "firstCharacter")
		if err != nil {
			t.Fatal(err)
		}
		cum := 0
		for _, b := range fc {
			if b.Key == "~" {
				continue
			}
			p, err := labWrapped.Browse(ctx, core.BrowseQuery{
				Filter: core.Filter{Type: core.TypeMovie},
				Sort:   core.SortTitle,
				Offset: cum,
				Limit:  1,
			})
			if err != nil || len(p.Items) != 1 {
				t.Fatalf("bucket %s offset %d: %v (items=%d)", b.Title, cum, err, len(p.Items))
			}
			got := p.Items[0]
			if b.Title == "A" && got.Title != "A" {
				t.Fatalf("bucket A at offset %d landed on %q (want \"A\")", cum, got.Title)
			}
			if b.Title != "#" && !strings.HasPrefix(strings.ToLower(got.SortTitle), strings.ToLower(b.Title)) {
				t.Fatalf("bucket %s at offset %d landed on %q (sort %q)", b.Title, cum, got.Title, got.SortTitle)
			}
			cum += b.Count
		}
	})
}
