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

	// Accented and mixed-case Latin titles fold into A..Z and sort in folded order.
	add("À Nous la Liberté", "fr")
	add("Æon Flux", "en")
	add("Ça brûle", "fr")
	add("Český Honza", "fr")
	add("Été 85", "fr")
	add("Léolo", "fr")
	add("Léon Morin, Priest", "fr")
	add("Léon: The Professional", "en")
	add("Líneas cruzadas", "fr")
	add("Lögnens pris", "fr")
	add("Lymelife", "en")
	add("Ödipussi", "fr")
	add("Yucatán", "fr")
	add("Yummy", "en")
	add("Yuppies", "en")
	add("Yves Saint Laurent", "fr")
	// Mixed-case explicit SortTitle must also fold into its base ASCII bucket.
	if err := c.upsert(ctx, core.Title{
		Key:       core.MovieKey(nextID),
		Type:      core.TypeMovie,
		Title:     "L'Éclisse",
		SortTitle: "ÉCLISSE",
		Language:  "fr",
		Released:  rel,
	}, upsertOpts{browsable: true}); err != nil {
		t.Fatal(err)
	}
	nextID++

	// Non-Latin / symbol sort_title (>= '{'): visible symbol title and hidden Japanese/Russian titles.
	add("½ Revolution", "en")
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

	// Verify accented L titles ("Léon...", "Léolo", "Líneas cruzadas", "Lögnens pris")
	// sort inside bucket L before "Lymelife", never after "Lymelife".
	lBucket, err := c.Browse(ctx, core.BrowseQuery{
		Filter: core.Filter{Type: core.TypeMovie, FirstCharacter: "L"},
		Sort:   core.SortTitle,
		Limit:  500,
	})
	if err != nil {
		t.Fatal(err)
	}
	idxOf := func(items []core.Title, title string) int {
		for i, it := range items {
			if it.Title == title {
				return i
			}
		}
		return -1
	}
	lymelifeIdx := idxOf(lBucket.Items, "Lymelife")
	if lymelifeIdx < 0 {
		t.Fatal("Lymelife not found in L bucket")
	}
	for _, acc := range []string{"Léolo", "Léon Morin, Priest", "Léon: The Professional", "Líneas cruzadas", "Lögnens pris"} {
		idx := idxOf(lBucket.Items, acc)
		if idx < 0 || idx >= lymelifeIdx {
			t.Fatalf("expected %q (idx=%d) to sort before Lymelife (idx=%d) in bucket L", acc, idx, lymelifeIdx)
		}
	}

	// Pre-existing unfolded sort_title rows in SQLite are folded when catalog.New opens the DB.
	db2 := store.OpenTest(t)
	c2, err := New(db2, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range []struct{ title, rawSort string }{
		{"Lymelife", "lymelife"},
		{"Léolo", "léolo"},
		{"Léon Morin, Priest", "léon morin, priest"},
		{"Léon: The Professional", "léon: the professional"},
		{"Líneas cruzadas", "líneas cruzadas"},
		{"Lögnens pris", "lögnens pris"},
		{"M", "m"},
		{"Été 85", "été 85"},
		{"\u200e7 Hours to Win Your Heart", "\u200e7 hours to win your heart"},
	} {
		if _, err := db2.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult,detail) VALUES(?,?,?,?,?,1,0,1)`,
			int64(core.MovieKey(int64(i+1))), "movie", row.title, row.rawSort, rel.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	_ = c2
	cReopened, err := New(db2, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fc2, err := cReopened.Facet(ctx, core.TypeMovie, "firstCharacter")
	if err != nil {
		t.Fatal(err)
	}
	all2, err := cReopened.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortTitle, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	off2 := 0
	for _, b := range fc2 {
		if got := all2.Items[off2]; b.Key == "M" && got.Title != "M" {
			t.Fatalf("bucket M at offset %d landed on %q (want M)", off2, got.Title)
		}
		off2 += b.Count
	}
}
