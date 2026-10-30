package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSectionDateSortRoutesUseAirDates(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	c, err := catalog.New(db, nil, config.Catalog{}, nil, catalog.WithNow(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatal(err)
	}
	date := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	a, b := core.ShowKey(901), core.ShowKey(902)
	season := core.SeasonKey(901, 1)
	items := []core.Title{
		{Key: a, Type: core.TypeShow, Title: "Alpha", Released: date(2000)},
		{Key: b, Type: core.TypeShow, Title: "Beta", Released: date(2010)},
		{Key: season, Type: core.TypeSeason, ShowKey: a, Season: 1, Title: "Season 1", Released: date(2001)},
		{Key: core.EpisodeKey(901, 1, 1), Type: core.TypeEpisode, ShowKey: a, Season: 1, Episode: 1, Title: "Aired", Released: date(2024)},
		{Key: core.EpisodeKey(901, 1, 2), Type: core.TypeEpisode, ShowKey: a, Season: 1, Episode: 2, Title: "Future", Released: date(2030)},
		{Key: core.EpisodeKey(902, 1, 1), Type: core.TypeEpisode, ShowKey: b, Season: 1, Episode: 1, Title: "Earlier", Released: date(2020)},
	}
	for _, item := range items {
		if err := c.UpsertTitle(ctx, item, item.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET fetched_at=CASE WHEN key=? THEN ? ELSE ? END WHERE key IN (?,?)`, int64(a), date(2021).Unix(), date(2025).Unix(), int64(a), int64(b)); err != nil {
		t.Fatal(err)
	}
	f := newFixture(func(d *Deps) { d.Catalog = c })
	defer f.srv.Close()
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) model.MediaContainer {
		t.Helper()
		var body struct{ MediaContainer model.MediaContainer }
		if rec.Code != 200 {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.MediaContainer
	}
	for _, tc := range []struct {
		path string
		keys []core.RatingKey
	}{
		{"/library/sections/2/recentlyAdded", []core.RatingKey{a, b}},
		{"/library/sections/2/all?sort=addedAt:asc", []core.RatingKey{b, a}},
		{"/library/sections/2/all?sort=addedAt:desc", []core.RatingKey{a, b}},
		{"/library/sections/2/all?sort=updatedAt:desc", []core.RatingKey{b, a}},
		{"/library/sections/2/all", []core.RatingKey{a, b}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			mc := decode(t, f.get(t, tc.path, jsonAccept...))
			if len(mc.Metadata) != len(tc.keys) {
				t.Fatalf("%s count=%d", tc.path, len(mc.Metadata))
			}
			for i, key := range tc.keys {
				if mc.Metadata[i].RatingKey != key.String() {
					t.Fatalf("%s order at %d got %s want %s", tc.path, i, mc.Metadata[i].RatingKey, key)
				}
			}
		})
	}
	rec := decode(t, f.get(t, fmt.Sprintf("/library/metadata/%s", season), jsonAccept...))
	if len(rec.Metadata) != 1 || rec.Metadata[0].AddedAt != date(2024).Unix() {
		t.Fatalf("season latest aired: %+v", rec.Metadata)
	}
}
