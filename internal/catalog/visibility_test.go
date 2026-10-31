package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestVisibilityExclusionsPrecedePaginationAndCount(t *testing.T) {
	c, err := New(store.OpenTest(t), nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 7; i++ {
		title := core.Title{Key: core.MovieKey(i), Type: core.TypeMovie, Title: fmt.Sprint(i), SortTitle: fmt.Sprint(i), Released: time.Now().Add(-time.Hour)}
		if err := c.upsert(t.Context(), title, upsertOpts{browsable: true}); err != nil {
			t.Fatal(err)
		}
	}
	q := core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, ExcludeKeys: []core.RatingKey{core.MovieKey(1), core.MovieKey(3)}}, Sort: core.SortTitle, Limit: 2, Offset: 1}
	for range 2 {
		p, err := c.Browse(t.Context(), q)
		if err != nil || p.Total != 5 || len(p.Items) != 2 || p.Items[0].Key != core.MovieKey(4) || p.Items[1].Key != core.MovieKey(5) {
			t.Fatalf("%+v %v", p, err)
		}
	}
	q.Filter.ExcludeKeys = nil
	p, err := c.Browse(t.Context(), q)
	if err != nil || p.Total != 7 || p.Items[0].Key != core.MovieKey(2) {
		t.Fatal("visibility polluted unfiltered cache", p, err)
	}
}

func TestFirstCharacterJumpbarOffsetsMatchTitleSortEveryLetter(t *testing.T) {
	cfg := config.Catalog{
		Exclusions: []core.LibraryExclusion{
			{
				Name:      "English/French only",
				MediaType: "movie",
				Match:     "all",
				Rules: []core.CollectionRule{
					{Field: "language", Op: "notIn", Value: json.RawMessage(`["en","fr"]`)},
				},
			},
		},
	}
	c, err := New(store.OpenTest(t), nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	rel := time.Now().Add(-time.Hour)
	var nextID int64 = 1
	add := func(title, lang string) {
		id := nextID
		nextID++
		ti := core.Title{
			Key:       core.MovieKey(id),
			Type:      core.TypeMovie,
			Title:     title,
			SortTitle: sortTitle(title),
			Language:  lang,
			Released:  rel,
		}
		if err := c.upsert(ctx, ti, upsertOpts{browsable: true}); err != nil {
			t.Fatal(err)
		}
	}

	// Leading "#" (digits and ASCII punctuation).
	add("!Three Amigos!", "en")
	add("1917", "en")
	add("2001: A Space Odyssey", "en")
	add("The 39 Steps", "en") // article stripped -> "39 steps" -> "#"

	// Every letter A..Z, including leading articles ("The ", "A ", "An ").
	for ch := byte('A'); ch <= 'Z'; ch++ {
		letter := string(ch)
		add(letter+"lpha "+letter, "en")
		add("The "+letter+"ravo "+letter, "en")
		add("A "+letter+"harlie "+letter, "en")
		add("An "+letter+"elta "+letter, "en")
		// Hidden titles in excluded languages must not inflate firstCharacter counts.
		add(letter+" Hidden Hindi", "hi")
	}

	// Non-ASCII sort_title (> 'z'): visible French title and hidden Japanese/Russian titles.
	add("Été 85", "fr")
	add("七人の侍", "ja")
	add("Солярис", "ru")

	facets, err := c.Facet(ctx, core.TypeMovie, "firstCharacter")
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.Browse(ctx, core.BrowseQuery{
		Filter: core.Filter{Type: core.TypeMovie},
		Sort:   core.SortTitle,
		Limit:  500,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Expected buckets: "#", "A".."Z", "~" (trailing non-ASCII sort_title).
	if len(facets) != 28 {
		t.Fatalf("expected 28 firstCharacter buckets (#, A..Z, ~), got %d: %+v", len(facets), facets)
	}
	if facets[0].Key != "#" || facets[0].Title != "#" || facets[0].Count != 4 {
		t.Fatalf("unexpected leading # bucket: %+v", facets[0])
	}
	if facets[len(facets)-1].Key != "~" || facets[len(facets)-1].Title != "#" || facets[len(facets)-1].Count != 1 {
		t.Fatalf("unexpected trailing non-ASCII bucket: %+v", facets[len(facets)-1])
	}

	offset := 0
	for _, b := range facets {
		// Jumpbar offset: paging Browse at `offset` must land on the exact first item of bucket `b`.
		page, err := c.Browse(ctx, core.BrowseQuery{
			Filter: core.Filter{Type: core.TypeMovie},
			Sort:   core.SortTitle,
			Offset: offset,
			Limit:  b.Count,
		})
		if err != nil {
			t.Fatalf("browse bucket %s at offset %d: %v", b.Key, offset, err)
		}
		if len(page.Items) != b.Count {
			t.Fatalf("bucket %s: expected %d items at offset %d, got %d", b.Key, b.Count, offset, len(page.Items))
		}
		// Direct filter by FirstCharacter must return the exact same slice.
		filtered, err := c.Browse(ctx, core.BrowseQuery{
			Filter: core.Filter{Type: core.TypeMovie, FirstCharacter: b.Key},
			Sort:   core.SortTitle,
			Limit:  500,
		})
		if err != nil {
			t.Fatalf("filter FirstCharacter=%s: %v", b.Key, err)
		}
		if filtered.Total != b.Count || len(filtered.Items) != b.Count {
			t.Fatalf("filter FirstCharacter=%s: expected %d items, got total=%d len=%d", b.Key, b.Count, filtered.Total, len(filtered.Items))
		}
		for i, item := range page.Items {
			if filtered.Items[i].Key != item.Key {
				t.Fatalf("bucket %s item %d mismatch: jumpbar=%s filtered=%s", b.Key, i, item.Title, filtered.Items[i].Title)
			}
			switch b.Key {
			case "#":
				if item.SortTitle >= "a" {
					t.Fatalf("bucket # contained %q (sort_title=%q)", item.Title, item.SortTitle)
				}
			case "~":
				if item.SortTitle < "{" {
					t.Fatalf("bucket ~ contained %q (sort_title=%q)", item.Title, item.SortTitle)
				}
			default:
				wantPrefix := strings.ToLower(b.Key)
				if !strings.HasPrefix(item.SortTitle, wantPrefix) {
					t.Fatalf("bucket %s at offset %d landed on %q (sort_title=%q)", b.Key, offset+i, item.Title, item.SortTitle)
				}
			}
		}
		offset += b.Count
	}
	if offset != all.Total {
		t.Fatalf("sum of firstCharacter counts (%d) != Browse total (%d)", offset, all.Total)
	}
}
