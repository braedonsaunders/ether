// Package stremio presents Ether's viewer-specific Stremio addon.
package stremio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// Frontend is mounted on the main listener under /stremio.
type Frontend struct{}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "stremio", Name: "Stremio", Version: "1.0.0", Clients: []string{"Stremio"}}
}

func (Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if host.Identity == nil || host.Experience == nil || host.Sessions == nil {
		return nil, errors.New("stremio requires identity, experience and sessions")
	}
	i := &instance{host: host, router: &route.Router{}}
	if err := i.router.Handle(http.MethodGet, "/{credential}/manifest.json", http.HandlerFunc(i.manifest)); err != nil {
		return nil, err
	}
	for _, pattern := range []string{"/{credential}/catalog/{type}/{id}", "/{credential}/catalog/{type}/{id}/{extra}"} {
		if err := i.router.Handle(http.MethodGet, pattern, http.HandlerFunc(i.catalog)); err != nil {
			return nil, err
		}
	}
	if err := i.router.Handle(http.MethodGet, "/{credential}/meta/{type}/{id}", http.HandlerFunc(i.meta)); err != nil {
		return nil, err
	}
	if err := i.router.Handle(http.MethodGet, "/image/{secret}", http.HandlerFunc(i.image)); err != nil {
		return nil, err
	}
	if err := i.router.Handle(http.MethodGet, "/{credential}/stream/{type}/{id}", http.HandlerFunc(i.stream)); err != nil {
		return nil, err
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if err := i.router.Handle(method, "/sessions/lazy/{rest...}", http.HandlerFunc(i.serveLazy)); err != nil {
			return nil, err
		}
	}
	return i, nil
}

type instance struct {
	host   frontend.Host
	router *route.Router
}

func (i *instance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/stremio/") {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, "/stremio")
			r.URL = &u
		}
		i.router.ServeHTTP(w, r)
	})
}
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/stremio"}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (*instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "1.0", Conformance: "partial"}
}
func (*instance) Close(context.Context) error { return nil }
func (i *instance) Routes() []route.Route     { return i.router.Routes() }

func (i *instance) manifest(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	catalogs := []any{
		map[string]any{"type": "movie", "id": "ether-movies", "name": "Ether Movies", "extra": []any{map[string]any{"name": "search"}, map[string]any{"name": "skip"}, map[string]any{"name": "genre"}}},
		map[string]any{"type": "series", "id": "ether-series", "name": "Ether Series", "extra": []any{map[string]any{"name": "search"}, map[string]any{"name": "skip"}, map[string]any{"name": "genre"}}},
	}
	for _, typ := range []string{"movie", "series"} {
		for _, row := range []struct{ id, name string }{{"continue", "Continue Watching"}, {"recent", "Recently Released"}, {"recommendations", "Recommendations"}} {
			catalogs = append(catalogs, map[string]any{"type": typ, "id": "ether-" + row.id, "name": row.name, "extra": []any{map[string]any{"name": "skip"}}})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "org.ether.viewer", "version": "1.0.0", "name": "Ether",
		"types": []string{"movie", "series"}, "resources": []string{"catalog", "meta", "stream"},
		"idPrefixes": []string{"tmdb:", "tt"},
		"catalogs":   catalogs,
	})
}
