package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type browseHelper struct {
	fakeHelper
	queries []core.BrowseQuery
}

func (b *browseHelper) BrowseTitles(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	b.queries = append(b.queries, q)
	return core.Page{Total: 107, Items: []core.Title{{Key: core.MovieKey(7), Type: core.TypeMovie, Title: "Seven", PosterPath: "/seven.jpg"}}}, nil
}

func TestCatalogBrowse(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	helper := &browseHelper{}
	h := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), CatalogHelper: helper})
	path := "/ether/api/catalog/browse?type=movie&q=Seven&genre=Drama&decade=1990&sort=title&offset=48&limit=48"
	if rec := serve(h, req(http.MethodGet, path, "")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", rec.Code)
	}
	rec := serve(h, authed(req(http.MethodGet, path, "")))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(helper.queries) != 1 {
		t.Fatalf("queries: %v", helper.queries)
	}
	q := helper.queries[0]
	if q.Filter.Type != core.TypeMovie || q.Filter.Query != "Seven" || len(q.Filter.Genres) != 1 || q.Filter.Genres[0] != "Drama" || q.Filter.YearFrom != 1990 || q.Filter.YearTo != 1999 || q.Sort != core.SortTitle || q.Desc || q.Offset != 48 || q.Limit != 48 {
		t.Fatalf("wrong query: %+v", q)
	}
	var body struct {
		Items []admin.TitleCard `json:"items"`
		Total int               `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 107 || len(body.Items) != 1 || body.Items[0].Title != "Seven" || body.Items[0].Poster != admin.ImageURL("/seven.jpg", 342) {
		t.Fatalf("wrong response: %+v", body)
	}
	for _, query := range []string{"limit=101", "offset=-1", "offset=nope", "limit=0", "decade=1997", "decade=nope", "sort=invalid"} {
		t.Run(query, func(t *testing.T) {
			rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/browse?"+query, "")))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
		})
	}
	if len(helper.queries) != 1 {
		t.Fatal("invalid requests reached the catalog")
	}
	rec = serve(h, authed(req(http.MethodGet, "/ether/api/catalog/browse", "")))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if q := helper.queries[1]; q.Limit != 48 || q.Offset != 0 || q.Sort != core.SortPopularity || !q.Desc {
		t.Fatalf("bad defaults: %+v", q)
	}
}

func TestCatalogBrowseWaitingFilter(t *testing.T) {
	store := newStore(t, func(c *config.Config) { seededConfig(c); c.SetupComplete = true })
	helper := &browseHelper{}
	waiting := []core.RatingKey{core.MovieKey(7), core.MovieKey(8)}
	h := admin.New(admin.Deps{
		Config:             store,
		IsOwnerToken:       storedOwner(store),
		CatalogHelper:      helper,
		WaitingReleaseKeys: func(context.Context) []core.RatingKey { return waiting },
	})
	rec := serve(h, authed(req(http.MethodGet, "/ether/api/catalog/browse?waiting=1", "")))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(helper.queries) != 1 {
		t.Fatalf("queries: %v", helper.queries)
	}
	q := helper.queries[0]
	if q.Filter.Type != core.TypeMovie || len(q.Filter.IncludeKeys) != 2 || q.Filter.IncludeKeys[0] != waiting[0] {
		t.Fatalf("waiting query: %+v", q)
	}
	empty := admin.New(admin.Deps{Config: store, IsOwnerToken: storedOwner(store), CatalogHelper: helper})
	rec = serve(empty, authed(req(http.MethodGet, "/ether/api/catalog/browse?waiting=1&type=show", "")))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	q = helper.queries[1]
	if q.Filter.Type != core.TypeMovie || q.Filter.IncludeKeys == nil || len(q.Filter.IncludeKeys) != 0 {
		t.Fatalf("empty waiting: %+v", q)
	}
}
