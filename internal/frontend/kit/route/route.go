// Package route provides an enumerable case-insensitive protocol router.
package route

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/conform"
)

type Route = conform.Route
type registered struct {
	route   Route
	parts   []string
	handler http.Handler
}
type Router struct {
	mu      sync.RWMutex
	routes  []registered
	ordered []registered
}

// Handle registers a literal/parameter route. Parameters use {name}; a final
// {name...} captures the remaining path. Duplicate patterns are rejected.
func (r *Router) Handle(method, pattern string, h http.Handler) error {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" || !strings.HasPrefix(pattern, "/") || h == nil {
		return fmt.Errorf("invalid route")
	}
	parts := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	seen := map[string]bool{}
	for i, p := range parts {
		if strings.ContainsAny(p, "{}") {
			if !strings.HasPrefix(p, "{") || !strings.HasSuffix(p, "}") {
				return fmt.Errorf("invalid parameter")
			}
			name := p[1 : len(p)-1]
			tail := strings.HasSuffix(name, "...")
			name = strings.TrimSuffix(name, "...")
			if name == "" || strings.ContainsAny(name, "{}") || seen[name] || (tail && i != len(parts)-1) {
				return fmt.Errorf("invalid parameter")
			}
			seen[name] = true
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.routes {
		if e.route.Method == method && strings.EqualFold(e.route.Pattern, pattern) {
			return fmt.Errorf("duplicate route")
		}
	}
	r.routes = append(r.routes, registered{Route{Method: method, Pattern: pattern}, parts, h})
	r.ordered = append([]registered(nil), r.routes...)
	sort.SliceStable(r.ordered, func(i, j int) bool { return moreSpecific(r.ordered[i].parts, r.ordered[j].parts) })
	return nil
}

// moreSpecific ranks literals ahead of parameters and tail captures.
// Registration order breaks ties; Routes retains registration order.
func moreSpecific(a, b []string) bool {
	rank := func(part string) int {
		if !strings.HasPrefix(part, "{") {
			return 3
		}
		if strings.HasSuffix(part, "...}") {
			return 1
		}
		return 2
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if ar, br := rank(a[i]), rank(b[i]); ar != br {
			return ar > br
		}
	}
	return len(a) > len(b)
}
func (r *Router) Routes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Route, len(r.routes))
	for i, e := range r.routes {
		out[i] = e.route
	}
	return out
}
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	r.mu.RLock()
	entries := append([]registered(nil), r.ordered...)
	r.mu.RUnlock()
	methodMismatch := false
	for _, e := range entries {
		values := map[string]string{}
		matched := true
		j := 0
		for i, p := range e.parts {
			if strings.HasPrefix(p, "{") {
				name := p[1 : len(p)-1]
				if strings.HasSuffix(name, "...") {
					name = strings.TrimSuffix(name, "...")
					values[name] = strings.Join(parts[j:], "/")
					j = len(parts)
					break
				}
				if j >= len(parts) || parts[j] == "" {
					matched = false
					break
				}
				values[name] = parts[j]
			} else if j >= len(parts) || !strings.EqualFold(p, parts[j]) {
				matched = false
				break
			}
			j++
			_ = i
		}
		if !matched || j != len(parts) {
			continue
		}
		if e.route.Method != req.Method {
			methodMismatch = true
			continue
		}
		for k, v := range values {
			req.SetPathValue(k, v)
		}
		e.handler.ServeHTTP(w, req)
		return
	}
	if methodMismatch {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(w, req)
}

// Query returns all values for keys matching name case-insensitively.
func Query(values url.Values, name string) []string {
	var result []string
	for k, v := range values {
		if strings.EqualFold(k, name) {
			result = append(result, v...)
		}
	}
	return result
}
