package catalog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraries"
	"github.com/braedonsaunders/ether/internal/store"
)

// Upgrade, source membership, first-match ordering and compound predicate
// conversion must agree on one title set, before counts and pagination.
func TestLibraryRulesUpgradeInvariant(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO libraries(id,key,name,source,media_type,filter,created_at,updated_at) VALUES(3,'anime','Anime Shows','filter','show','{"type":"show","group":{"operator":"and","children":[{"operator":"and","filter":{"genres":["Animation"]}}]} }',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	const source = "stremio:addon.example:popular"
	_, err = db.Exec(`INSERT INTO libraries(id,key,name,source,media_type,filter,sources,created_at,updated_at) VALUES
(4,'cinemeta','Cinemeta Popular','filter','show','{"type":"show"}',?,1,1),
(5,'empty-group','Empty group','filter','show','{"type":"show","group":{"operator":"and","children":[{"operator":"and","filter":{}}]}}',?,1,1)`, `["`+source+`"]`, `["`+source+`"]`)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := libraries.New(ctx, db, libraries.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := svc.Get(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if lib.Source != core.LibraryCatalog || lib.VisibilityDefault != "hide" || len(lib.Rules) != 1 || lib.Rules[0].Action != "show" || len(lib.Sources) != 1 || lib.Sources[0] != "tmdb-tv" {
		t.Fatalf("upgrade: %+v", lib)
	}
	if len(lib.Rules[0].Rules) != 1 || lib.Rules[0].Rules[0].Field != "genre" {
		t.Fatalf("single-child groups survived: %+v", lib.Rules)
	}
	for _, id := range []core.LibraryID{4, 5} {
		converted, err := svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(converted.Sources) != 1 || converted.Sources[0] != source || len(converted.Rules) != 0 || converted.VisibilityDefault != "show" {
			t.Fatalf("empty predicate/source upgrade: %+v", converted)
		}
	}
	if _, err = libraries.New(ctx, db, libraries.Options{}); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if _, err = db.Exec(`SELECT filter FROM libraries`); err == nil {
		t.Fatal("old storage column survived upgrade")
	}
	c, err := New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<5000) INSERT INTO catalog_titles(key,type,tmdb_id,title,sort_title,year,released_ts,popularity,rating,vote_count,runtime_sec,language,browsable,adult) SELECT ?+i,'show',i,'Show','Show',2000+i%20,946684800,i,7,100,3600,'ja',1,0 FROM seq`, int64(core.ShowKey(0)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO catalog_genres(key,genre) SELECT key,CASE WHEN tmdb_id<=4500 THEN 'Animation' ELSE 'Drama' END FROM catalog_titles`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO libraries_catalogue_members(source_id,key) SELECT ?,key FROM catalog_titles WHERE tmdb_id<=498`, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []core.LibraryID{4, 5} {
		lib, err := svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: lib.MediaType, CatalogueSources: lib.Sources, ExtraRules: lib.Rules, ExtraFallback: lib.VisibilityDefault}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 498 {
			t.Fatalf("source membership changed: library %d count %d", id, page.Total)
		}
	}
	predicates := []core.Filter{
		{Type: core.TypeShow},
		{Type: core.TypeShow, Genres: []string{"Animation"}},
		{Type: core.TypeShow, Genres: []string{"Animation"}, YearFrom: 2005, YearTo: 2010, ExcludeYears: []int{2007}, MinRating: 6, MaxRuntime: 2 * time.Hour},
		{Type: core.TypeShow, Group: &core.FilterGroup{Operator: "or", Children: []core.FilterGroup{{Filter: &core.Filter{YearTo: 2001}}, {Filter: &core.Filter{YearFrom: 2018}}}}},
	}
	for i, p := range predicates {
		original, err := c.Browse(ctx, core.BrowseQuery{Filter: p, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		conditions := core.LibraryPredicateConditions(p)
		var rules []core.LibraryExclusion
		fallback := "show"
		if len(conditions) > 0 {
			rules = []core.LibraryExclusion{{Action: "show", MediaType: "show", Match: "all", Rules: conditions}}
			fallback = "hide"
		}
		converted, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow, ExtraRules: rules, ExtraFallback: fallback}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(converted)
		if string(a) != string(b) {
			t.Fatalf("predicate %d changed membership: old=%d converted=%d", i, original.Total, converted.Total)
		}
		if i == 1 && converted.Total != 4500 {
			t.Fatalf("Anime count=%d", converted.Total)
		}
	}
}
