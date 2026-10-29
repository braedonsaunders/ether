// Package mediabrowser implements the Jellyfin and Emby protocol surfaces.
package mediabrowser

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// PublicInfo is supplied by the host, so the server identity remains stable
// across restarts and is distinct for each protocol surface.
type PublicInfo struct {
	ID, ServerName, LocalAddress, Version, ProductName string
}

// PublicRoutes registers connection endpoints that do not require a grant.
// A user picker is intentionally not exposed until the host can supply its
// authorized list of visible viewers.
func PublicRoutes(r *route.Router, info PublicInfo) error {
	handlers := []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "/System/Info/Public", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(struct {
				ID                     string `json:"Id"`
				ServerName             string `json:"ServerName"`
				LocalAddress           string `json:"LocalAddress"`
				Version                string `json:"Version"`
				ProductName            string `json:"ProductName"`
				OperatingSystem        string `json:"OperatingSystem"`
				StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
			}{info.ID, info.ServerName, info.LocalAddress, info.Version, info.ProductName, "", true})
		}},
		{"GET", "/Users/Public", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte("[]"))
		}},
		{"GET", "/System/Ping", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode("Jellyfin Server")
		}},
	}
	for _, h := range handlers {
		if err := r.Handle(h.method, h.path, h.handler); err != nil {
			return err
		}
	}
	return nil
}

// StripPrefix accepts the optional protocol prefix while preserving the rest
// of the request for the router. It does not mutate the incoming request.
func StripPrefix(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= len(prefix) && strings.EqualFold(r.URL.Path[:len(prefix)], prefix) && (len(r.URL.Path) == len(prefix) || r.URL.Path[len(prefix)] == '/') {
			clone := r.Clone(r.Context())
			u := *r.URL
			clone.URL = &u
			clone.URL.Path = r.URL.Path[len(prefix):]
			if clone.URL.Path == "" {
				clone.URL.Path = "/"
			}
			clone.URL.RawPath = ""
			r = clone
		}
		next.ServeHTTP(w, r)
	})
}
