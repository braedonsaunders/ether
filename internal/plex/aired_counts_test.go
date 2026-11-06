package plex

import (
	"encoding/json"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A show and its seasons count the episodes the viewer can list, not a
// stale or announced total, so watching every aired episode marks them
// watched: leafCount and viewedLeafCount come from the same list on the
// detail page, the season list and the All episodes entry.
func TestLeafCountsMatchListedEpisodes(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	show := breakingBad
	show.ChildCount, show.LeafCount = 3, 24 // e.g. TMDB's announced totals
	season := bbS1
	season.ChildCount = 12
	f.catalog.titles[show.Key] = show
	f.catalog.titles[season.Key] = season
	for _, k := range []core.RatingKey{bbE1.Key, bbE2.Key} {
		f.state.set(ownerUser.ID, core.WatchStatus{Key: k, ViewCount: 1, LastViewedAt: fixedNow, UpdatedAt: fixedNow})
	}

	type item struct {
		RatingKey       string `json:"ratingKey"`
		Title           string `json:"title"`
		LeafCount       *int   `json:"leafCount"`
		ViewedLeafCount *int   `json:"viewedLeafCount"`
		ViewCount       int    `json:"viewCount"`
	}
	var out struct {
		MediaContainer struct {
			Metadata  []item `json:"Metadata"`
			Directory []item `json:"Directory"`
		} `json:"MediaContainer"`
	}
	check := func(name string, it item) {
		t.Helper()
		if it.LeafCount == nil || it.ViewedLeafCount == nil {
			t.Fatalf("%s: leafCount or viewedLeafCount missing: %+v", name, it)
		}
		if *it.LeafCount != 2 || *it.ViewedLeafCount != 2 {
			t.Fatalf("%s: leafCount=%d viewedLeafCount=%d, want 2/2", name, *it.LeafCount, *it.ViewedLeafCount)
		}
	}
	get := func(path string) {
		t.Helper()
		rec := f.get(t, path, "Accept", "application/json")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		out.MediaContainer.Metadata, out.MediaContainer.Directory = nil, nil
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v %s", path, err, rec.Body.String())
		}
	}

	get("/library/metadata/" + show.Key.String())
	if len(out.MediaContainer.Metadata) != 1 {
		t.Fatalf("show detail: %+v", out.MediaContainer)
	}
	check("show detail", out.MediaContainer.Metadata[0])
	if out.MediaContainer.Metadata[0].ViewCount != 0 {
		t.Fatal("leaf totals fabricated an independent parent view count")
	}

	f.state.set(ownerUser.ID, core.WatchStatus{Key: show.Key, ViewCount: 7})
	get("/library/metadata/" + show.Key.String())
	if out.MediaContainer.Metadata[0].ViewCount != 7 {
		t.Fatal("parent count must be persisted independently of two watched leaves")
	}

	get("/library/metadata/" + show.Key.String() + "/children")
	if len(out.MediaContainer.Metadata) != 1 || len(out.MediaContainer.Directory) != 1 {
		t.Fatalf("show children: %+v", out.MediaContainer)
	}
	check("season row", out.MediaContainer.Metadata[0])
	check("All episodes", out.MediaContainer.Directory[0])

	get("/library/metadata/" + season.Key.String())
	if len(out.MediaContainer.Metadata) != 1 {
		t.Fatalf("season detail: %+v", out.MediaContainer)
	}
	check("season detail", out.MediaContainer.Metadata[0])
}

// A show's totals are its regular episodes on the detail page, as on every
// list (the catalogue leaves Specials out): an unwatched special neither
// changes the badge nor keeps a fully watched show unwatched.
func TestShowLeafCountsLeaveOutSpecials(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	specials := core.Title{Key: core.SeasonKey(1396, 0), Type: core.TypeSeason, ShowKey: breakingBad.Key, Season: 0, Title: "Specials", ChildCount: 1}
	special := core.Title{Key: core.EpisodeKey(1396, 0, 1), Type: core.TypeEpisode, ShowKey: breakingBad.Key, Season: 0, Episode: 1, Title: "Special", Released: bbE1.Released}
	f.catalog.titles[specials.Key] = specials
	f.catalog.titles[special.Key] = special
	f.catalog.children[breakingBad.Key] = append([]core.RatingKey{specials.Key}, f.catalog.children[breakingBad.Key]...)
	f.catalog.children[specials.Key] = []core.RatingKey{special.Key}
	for _, k := range []core.RatingKey{bbE1.Key, bbE2.Key} {
		f.state.set(ownerUser.ID, core.WatchStatus{Key: k, ViewCount: 1, LastViewedAt: fixedNow, UpdatedAt: fixedNow})
	}
	rec := f.get(t, "/library/metadata/"+breakingBad.Key.String(), "Accept", "application/json")
	var out struct {
		MediaContainer struct {
			Metadata []struct {
				LeafCount       *int `json:"leafCount"`
				ViewedLeafCount *int `json:"viewedLeafCount"`
				ViewCount       int  `json:"viewCount"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.MediaContainer.Metadata) != 1 {
		t.Fatalf("show detail: %v %s", err, rec.Body.String())
	}
	m := out.MediaContainer.Metadata[0]
	if m.LeafCount == nil || *m.LeafCount != 2 || m.ViewedLeafCount == nil || *m.ViewedLeafCount != 2 || m.ViewCount != 0 {
		t.Fatalf("show detail counts: %+v %s", m, rec.Body.String())
	}
}
