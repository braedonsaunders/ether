package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strconv"
	"strings"
)

func facetView(r *http.Request) string {
	values := route.Query(r.URL.Query(), "ParentId")
	if len(values) == 1 {
		if n, e := ids.ParseGUID(ids.View, values[0]); e == nil {
			return ids.Decimal(n)
		}
	}
	return ""
}
func (i *instance) facets(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, _, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		facets, err := i.host.Experience.Facets(r.Context(), v, device(r), facetView(r), kind)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		offset, limit := queryInt(r, "StartIndex", 0), queryInt(r, "Limit", 50)
		items := make([]any, 0)
		typeName := map[string]string{"genre": "Genre", "studio": "Studio", "person": "Person"}[kind]
		for _, facet := range paging.Slice(facets, offset, limit) {
			items = append(items, map[string]any{"Id": facet.ID, "Name": facet.Title, "Type": typeName, "ServerId": i.info.ID})
		}
		jsonReply(w, i.queryResult(items, len(facets), offset))
	}
}
func (i *instance) resolveFilters(r *http.Request, v *frontend.Viewer, q *frontend.ItemQuery) error {
	view := facetView(r)
	for _, field := range []struct{ name, kind string }{{"GenreIds", "genre"}, {"StudioIds", "studio"}, {"PersonIds", "person"}} {
		for _, value := range route.Query(r.URL.Query(), field.name) {
			for _, id := range strings.Split(value, ",") {
				id, err := i.resolveNamedFacetWireID(r.Context(), field.kind, id)
				if err != nil {
					return err
				}
				facet, e := i.host.Experience.ResolveFacet(r.Context(), v, device(r), view, field.kind, id)
				if e != nil {
					return e
				}
				switch field.kind {
				case "genre":
					q.Filter.Genres = append(q.Filter.Genres, facet.Key)
				case "studio":
					if q.Filter.Studio != "" && q.Filter.Studio != facet.Key {
						return strconv.ErrSyntax
					}
					q.Filter.Studio = facet.Key
				case "person":
					n, e := strconv.ParseInt(facet.Key, 10, 64)
					if e != nil || n <= 0 || (q.Filter.Person != 0 && q.Filter.Person != n) {
						return strconv.ErrSyntax
					}
					q.Filter.Person = n
				}
			}
		}
	}
	return nil
}
