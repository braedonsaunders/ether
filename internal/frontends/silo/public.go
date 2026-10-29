package silo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func (i *instance) systemInfo(w http.ResponseWriter, r *http.Request) {
	doc := i.openAPIDocument()
	sum := sha256.Sum256(doc)
	data := map[string]any{"server_version": "Ether", "api_major": 2, "contract_digest": hex.EncodeToString(sum[:]), "links": map[string]any{"openapi": "/api/v2/openapi.json", "capabilities": "/api/v2/capabilities", "identity": "/api/v2/system/identity"}}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if origin, err := url.Parse(scheme + "://" + r.Host); err == nil && origin.Host != "" && origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" {
		data["request_origin"] = origin.String()
	}
	reply(w, 200, data)
}

// openAPIDocument describes only Ether's implemented routes and never claims
// to be the upstream server's OpenAPI contract.
func (i *instance) openAPIDocument() []byte {
	paths := map[string]map[string]any{}
	for _, route := range i.router.Routes() {
		if !strings.HasPrefix(route.Pattern, "/api/v2/") {
			continue
		}
		if paths[route.Pattern] == nil {
			paths[route.Pattern] = map[string]any{}
		}
		paths[route.Pattern][strings.ToLower(route.Method)] = map[string]any{"responses": map[string]any{"default": map[string]any{"description": "Response"}}}
	}
	doc, _ := json.Marshal(map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Ether Silo surface", "version": "2"}, "paths": paths})
	return doc
}
func (i *instance) openAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(i.openAPIDocument())
}
func (i *instance) systemIdentity(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"server_id": i.id})
}
func (i *instance) setup(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"needs_setup": false, "wizard_completed": true})
}
func (i *instance) account(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	reply(w, 200, user(v))
}
func (i *instance) requestStatus(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	reply(w, 200, map[string]any{"requests_enabled": false, "rating_restrictions_enforced": false, "follow_supported": false, "season_requests_supported": false, "missing_seasons_requestable": false, "download_progress_supported": false, "watchlist_titles_supported": false, "watchlist_requests": false, "discover_rows_supported": false, "revision": "ether-v1", "state": "unsupported", "allowed": false})
}
func (i *instance) publicRoutes() error {
	if err := i.router.Handle("PATCH", "/api/v2/profiles/{id}", http.HandlerFunc(i.patchProfile)); err != nil {
		return err
	}
	for _, e := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/api/v2/openapi.json", i.openAPI}, {"/api/v2/system/identity", i.systemIdentity}, {"/api/v2/requests/status", i.requestStatus}, {"/api/v2/system/info", i.systemInfo}, {"/api/v2/system/setup", i.setup}, {"/api/v2/account/me", i.account}, {"/api/v2/profiles", i.profiles}} {
		if err := i.router.Handle("GET", e.path, e.h); err != nil {
			return err
		}
	}
	return nil
}
