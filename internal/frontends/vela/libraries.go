package vela

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func libraryWire(view frontend.View) object {
	kind := "mixed"
	root := ""
	switch view.Type {
	case frontend.TypeMovie:
		kind, root = "movies", "movie"
	case frontend.TypeShow:
		kind, root = "shows", "show"
	}
	sorts := []object{{"id": "server", "directions": []string{"asc"}}}
	filters := object{}
	defaultSort := "server"
	if root != "" {
		filters["kind"] = []string{root}
	}
	// Plugin instances preserve their existing store/source order. Their
	// schema-specific filters are not inferred from a native presentation name.
	if view.PluginID == "" && view.KindKey == "" {
		defaultSort = "title"
		for _, sort := range []string{"title", "added", "released", "year", "rating", "duration", "popularity"} {
			sorts = append(sorts, object{"id": sort, "directions": []string{"asc", "desc"}})
		}
		filters["genre"], filters["year"], filters["favorite"] = true, true, true
		if root == "movie" {
			filters["played"] = true
		}
	}
	return object{"id": view.ID, "name": view.Name, "kind": kind, "artwork": []object{}, "browse": object{"sorts": sorts, "defaultSort": defaultSort, "defaultDirection": "asc", "filters": filters}}
}

func (i *instance) libraries(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil {
		fail(w, err)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		fail(w, err)
		return
	}
	scope, err := i.livePageScope(r, v, token)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := i.readLiveCursor(r, scope, hashJSON([]any{"libraries", limit(r)}))
	if err != nil {
		fail(w, err)
		return
	}
	snapshot := hashJSON(views)
	if c.Snapshot != "" && c.Snapshot != snapshot {
		fail(w, frontend.ErrLiveGuideStale)
		return
	}
	start := min(c.Offset, len(views))
	end := min(start+limit(r), len(views))
	items := []object{}
	for _, view := range views[start:end] {
		items = append(items, libraryWire(view))
	}
	page := object{"items": items, "continuation": "exhausted"}
	if end < len(views) {
		next, e := i.nextLiveCursor(c, end, snapshot)
		if e != nil {
			fail(w, e)
			return
		}
		page["continuation"], page["nextCursor"] = "more", next
	}
	cache(w, r, page)
}

func libraryQuery(r *http.Request, library object) (frontend.ItemQuery, string, string, bool) {
	q := frontend.ItemQuery{Limit: limit(r), NoTotal: true}
	options := library["browse"].(object)
	sort, direction := r.URL.Query().Get("sort"), r.URL.Query().Get("direction")
	if sort == "" {
		sort = options["defaultSort"].(string)
	}
	if direction == "" {
		direction = options["defaultDirection"].(string)
	}
	allowed := false
	for _, candidate := range options["sorts"].([]object) {
		if candidate["id"] == sort {
			for _, d := range candidate["directions"].([]string) {
				if direction == d {
					allowed = true
				}
			}
		}
	}
	if !allowed {
		return q, sort, direction, false
	}
	if sort != "server" {
		q.Sort = frontend.SortOrder(sort)
	}
	q.Desc = direction == "desc"
	filters := options["filters"].(object)
	values := r.URL.Query()
	// Repeated values are ambiguous, rather than an implicit OR filter.
	for name, entries := range values {
		switch name {
		case "cursor", "limit", "sort", "direction", "kind", "genre", "year", "played", "favorite":
		default:
			return q, sort, direction, false
		}
		if len(entries) != 1 {
			return q, sort, direction, false
		}
	}
	if kind := values.Get("kind"); kind != "" {
		ok := false
		kinds, _ := filters["kind"].([]string)
		for _, offered := range kinds {
			if kind == offered {
				ok = true
			}
		}
		if !ok {
			return q, sort, direction, false
		}
		q.Types = []frontend.MediaType{frontend.MediaType(kind)}
	}
	if genre := values.Get("genre"); genre != "" {
		if filters["genre"] != true || len(genre) > 256 {
			return q, sort, direction, false
		}
		q.Filter.Genres = []string{genre}
	}
	if year := values.Get("year"); year != "" {
		n, e := strconv.Atoi(year)
		if filters["year"] != true || e != nil || n < 1 || n > 9999 {
			return q, sort, direction, false
		}
		q.Filter.Years = []int{n}
	}
	for _, name := range []string{"played", "favorite"} {
		if raw, exists := values[name]; exists {
			if filters[name] != true || (raw[0] != "true" && raw[0] != "false") {
				return q, sort, direction, false
			}
			yes := raw[0] == "true"
			if name == "played" {
				q.Watched = &yes
			} else {
				q.Favorite = &yes
			}
		}
	}
	return q, sort, direction, true
}

func (i *instance) libraryItems(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil {
		fail(w, err)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	var view frontend.View
	found := false
	for _, candidate := range views {
		if candidate.ID == id {
			view = candidate
			found = true
			break
		}
	}
	// Even collection libraries must be published before Vela will browse them.
	if !found {
		problem(w, 404, "not-found", "not_found")
		return
	}
	library := libraryWire(view)
	q, sort, direction, ok := libraryQuery(r, library)
	if !ok {
		problem(w, 400, "request", "unsupported_library_query")
		return
	}
	scope, err := i.livePageScope(r, v, token)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := i.readLiveCursor(r, scope, hashJSON([]any{"library-items", id, q}))
	if err != nil {
		fail(w, err)
		return
	}
	snapshot := hashJSON(views)
	if c.Offset > 10000 || (c.Snapshot != "" && c.Snapshot != snapshot) {
		fail(w, frontend.ErrLiveGuideStale)
		return
	}
	q.Offset = c.Offset
	page, err := i.host.Experience.LibraryItems(r.Context(), v, requestDevice(r), id, q)
	if err != nil {
		fail(w, err)
		return
	}
	if len(page.Items) > q.Limit {
		page.Items = page.Items[:q.Limit]
	}
	converted, err := i.titles(r, v, token, page.Items)
	if err != nil {
		fail(w, err)
		return
	}
	entries := []object{}
	for _, title := range converted {
		entries = append(entries, object{"id": title["id"], "kind": "title", "title": title, "receiptToken": ""})
	}
	out := object{"library": library, "items": entries, "sort": sort, "direction": direction, "continuation": "exhausted"}
	if len(page.Items) == q.Limit {
		// Ask for one next item only after the page establishes its keyset end.
		// This preserves neutral paging caches and avoids counts or hydration walks.
		nextQuery := q
		nextQuery.Offset += len(page.Items)
		nextQuery.Limit = 1
		more, e := i.host.Experience.LibraryItems(r.Context(), v, requestDevice(r), id, nextQuery)
		if e != nil {
			fail(w, e)
			return
		}
		if len(more.Items) > 0 {
			next, e := i.nextLiveCursor(c, nextQuery.Offset, snapshot)
			if e != nil {
				fail(w, e)
				return
			}
			out["continuation"], out["nextCursor"] = "more", next
		}
	}
	cache(w, r, out)
}
