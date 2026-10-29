package plex

import (
	"net/http"
	"sort"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/go-chi/chi/v5"
)

// RouteInventory enumerates the same registration tree used by the server,
// without starting background workers or needing runtime dependencies.
func RouteInventory() conform.RouteInventory {
	r := (&Server{}).routes()
	routes := make([]conform.Route, 0)
	_ = chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, conform.Route{Method: method, Pattern: pattern})
		return nil
	})
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Pattern == routes[j].Pattern {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Pattern < routes[j].Pattern
	})
	return conform.RouteInventory{Available: true, Routes: routes}
}
