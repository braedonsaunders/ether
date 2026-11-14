package vela

import "testing"

func TestFeaturedHeroesPreserveReceiptsAndDeduplicate(t *testing.T) {
	entry := func(id, receipt string) object {
		return object{"kind": "title", "title": object{"id": id, "name": id}, "receiptToken": receipt}
	}
	arbitrary := object{"id": "shelf", "style": "shelf", "continuation": "exhausted", "items": []object{entry("shelf-only", "issued-shelf")}}
	if heroes, _ := heroesFromRows([]object{arbitrary}); len(heroes) != 0 {
		t.Fatal("arbitrary shelf became featured", heroes)
	}
	featured := object{"id": "featured", "style": "hero", "continuation": "more", "nextCursor": "original-page-end", "items": []object{entry("missing-receipt", ""), entry("A", "issued-A"), entry("A", "duplicate-A"), entry("B", "issued-B"), entry("C", "issued-C"), entry("D", "issued-D"), entry("E", "issued-E"), entry("F", "issued-F")}}
	duplicateShelf := object{"id": "also", "continuation": "exhausted", "items": []object{entry("A", "shelf-A"), entry("G", "issued-G")}}
	rows := []object{arbitrary, featured, duplicateShelf}
	heroes, shelves := heroesFromRows(rows)
	if len(heroes) != 5 {
		t.Fatal("feature cap/order lost", heroes)
	}
	chosen := map[string]bool{}
	for n, hero := range heroes {
		id := hero["title"].(object)["id"].(string)
		want := string(rune('A' + n))
		if id != want || hero["receiptToken"] != "issued-"+id {
			t.Fatal("identity/order/receipt changed", hero)
		}
		if _, invented := hero["trailer"]; invented {
			t.Fatal("uncached trailer invented")
		}
		chosen[id] = true
	}
	for _, row := range shelves {
		if row["id"] == "featured" && row["nextCursor"] != "original-page-end" {
			t.Fatal("dedup altered issued page offset")
		}
		for _, entry := range row["items"].([]object) {
			if title, ok := entry["title"].(object); ok && chosen[title["id"].(string)] {
				t.Fatal("featured title repeated in rows", entry)
			}
		}
	}
	if len(featured["items"].([]object)) != 8 {
		t.Fatal("dedup mutated source visit")
	}
}
