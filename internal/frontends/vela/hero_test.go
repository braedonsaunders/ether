package vela

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http/httptest"
	"testing"
)

func TestFeaturedHeroesPreserveReceiptsAndDeduplicate(t *testing.T) {
	entry := func(id, receipt string) object {
		return object{"kind": "title", "title": object{"id": id, "name": id, "kind": "movie", "artwork": []object{{"kind": "backdrop", "sizes": []object{{"url": "/signed/" + id, "width": 1920, "height": 1080}}}}}, "receiptToken": receipt}
	}
	arbitrary := object{"id": "shelf", "style": "shelf", "continuation": "exhausted", "items": []object{entry("shelf-only", "issued-shelf")}}
	if heroes, _ := heroesFromRows([]object{arbitrary}); len(heroes) != 0 {
		t.Fatal("arbitrary shelf became featured", heroes)
	}
	featured := object{"id": "featured", "style": "hero", "purpose": frontend.RowRecentlyReleased, "continuation": "more", "nextCursor": "original-page-end", "items": []object{entry("missing-receipt", ""), entry("A", "issued-A"), entry("A", "duplicate-A"), entry("B", "issued-B"), entry("C", "issued-C"), entry("D", "issued-D"), entry("E", "issued-E"), entry("F", "issued-F")}}
	duplicateShelf := object{"id": "also", "continuation": "exhausted", "items": []object{entry("A", "shelf-A"), entry("G", "issued-G")}}
	rows := []object{arbitrary, featured, duplicateShelf}
	heroes, shelves := heroesFromRows(rows)
	if len(heroes) != 6 {
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

	seal, err := encryption(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	inst := &instance{seal: seal}
	r := httptest.NewRequest("GET", "/vela/v0/rows/featured", nil)
	r.Header.Set("X-Vela-Device-Id", "device")
	viewer := &frontend.Viewer{ID: 1}
	token := inst.rowToken(r, viewer, "featured", 50, []string{"A", "B", "C", "D", "E", "F"})
	cursor, err := inst.readRowToken(r, viewer, token)
	if err != nil || cursor.Offset != 50 || len(cursor.Exclude) != 6 {
		t.Fatal("hero pagination exclusion lost", cursor, err)
	}
	r.Header.Set("X-Vela-Device-Id", "another")
	if _, err := inst.readRowToken(r, viewer, token); err == nil {
		t.Fatal("hero exclusions crossed device scope")
	}
}

// Composition must preserve action lineage, real artwork and producer evidence
// while mixing ranked personal picks with public and recent sources.
func TestHeroCompositionProvenance(t *testing.T) {
	entry := func(id, kind string, offset float64) object {
		return object{"kind": "title", "receiptToken": "receipt-" + id, "title": object{"id": id, "kind": kind, "watch": object{"positionSeconds": offset}, "artwork": []object{{"kind": "backdrop", "sizes": []object{{"url": "/signed/" + id, "width": 0, "height": 0}}}}}}
	}
	resume := entry("resume", "movie", 123.25)
	next := entry("next", "episode", 0)
	next["title"].(object)["showId"] = "show"
	artless := entry("artless", "movie", 0)
	artless["title"].(object)["artwork"] = []object{}
	portrait := entry("portrait", "movie", 0)
	portrait["title"].(object)["artwork"].([]object)[0]["sizes"] = []object{{"url": "/signed/portrait", "width": 600, "height": 900}}
	rows := []object{
		{"id": "continue", "purpose": frontend.RowContinue, "previewOnFocus": true, "items": []object{entry("zero", "movie", 0), resume}},
		{"id": "next", "purpose": frontend.RowNextUp, "items": []object{next}},
		{"id": "personal", "items": []object{artless, portrait, entry("personal", "movie", 0), entry("personal2", "movie", 0)}},
		{"id": "trends", "items": []object{entry("trend", "show", 0), entry("trend2", "movie", 0)}},
		{"id": "recent", "purpose": frontend.RowRecentlyAdded, "items": []object{entry("recent", "movie", 0)}},
	}
	sources := map[string]heroSource{"personal": {kind: "top-10"}, "trends": {kind: "trending"}}
	heroes, _ := heroesFromRows(rows, sources)
	wants := []string{"resume", "next", "trend", "personal", "recent", "trend2"}
	reasons := []string{"resume", "nextEpisode", "trending", "forYou", "newRelease", "trending"}
	if len(heroes) != 6 {
		t.Fatal(heroes)
	}
	for n, hero := range heroes {
		id := hero["title"].(object)["id"]
		action := hero["action"].(object)
		if id != wants[n] || hero["reason"] != reasons[n] || action["titleId"] != id || hero["receiptToken"] != "receipt-"+wants[n] {
			t.Fatalf("slot %d: %#v", n, hero)
		}
		if id == "trend" && action["kind"] != "details" {
			t.Fatal("attempted to play a show")
		}
	}
	if heroes[0]["action"].(object)["positionSeconds"] != 123.25 || heroes[0]["previewOnFocus"] != true {
		t.Fatal("lost persisted resume/preview opt-in")
	}
	sources["personal"] = heroSource{kind: "top-10", pinned: true}
	heroes, _ = heroesFromRows(rows, sources)
	if heroes[0]["title"].(object)["id"] != "personal" || heroes[0]["reason"] != "forYou" {
		t.Fatal("owner order or honest reason lost")
	}
}
