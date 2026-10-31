package silo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
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
func (i *instance) branding(w http.ResponseWriter, _ *http.Request) {
	var settings frontend.Settings
	if i.host.Config != nil {
		settings = i.host.Config()
	}
	reply(w, http.StatusOK, map[string]any{"server_name": frontend.AdvertisedName(settings), "login_subtitle": "", "storage_available": false})
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
	reply(w, 200, map[string]any{"requests_enabled": false, "rating_restrictions_enforced": false, "follow_supported": false, "season_requests_supported": false, "missing_seasons_requestable": false, "download_progress_supported": false, "watchlist_titles_supported": true, "watchlist_requests": false, "discover_rows_supported": false, "revision": "ether-v1", "state": "unsupported", "allowed": false})
}

func (i *instance) aiCapability(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/metadata-ai") {
		reply(w, http.StatusOK, map[string]any{"revision": "ether-metadata-ai-v1", "state": "unsupported", "allowed": false, "on_view": "off"})
		return
	}
	reply(w, http.StatusOK, map[string]any{"revision": "ether-subtitle-ai-v1", "state": "unsupported", "allowed": false, "enabled": false, "transcribe_enabled": false})
}

func (i *instance) optionalCapability(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	base := map[string]any{"revision": "ether-unsupported-v1", "state": "unsupported", "allowed": false}
	switch r.URL.Path {
	case "/api/v2/shuffles/capabilities":
		base["scope_kinds"] = []string{}
	case "/api/v2/subtitles/providers/status":
		base["schema_version"], base["enabled"], base["providers"] = 1, false, []string{}
	case "/api/v2/watch-together/capabilities":
		for _, key := range []string{"staged_selection", "lobby_ready", "connection_replaced", "selection_mode_switch", "member_state", "picker", "vote_host_override", "stop_playback"} {
			base[key] = false
		}
		base["max_member_state_ids"], base["socket_protocol"] = 0, ""
	}
	reply(w, http.StatusOK, base)
}
func (i *instance) publicRoutes() error {
	if err := i.router.Handle("PATCH", "/api/v2/profiles/{id}", http.HandlerFunc(i.patchProfile)); err != nil {
		return err
	}
	for _, path := range []string{"/api/v2/capabilities/metadata-ai", "/api/v2/subtitles/ai/status"} {
		if err := i.router.Handle("GET", path, http.HandlerFunc(i.aiCapability)); err != nil {
			return err
		}
	}
	for _, path := range []string{"/api/v2/shuffles/capabilities", "/api/v2/subtitles/providers/status", "/api/v2/watch-together/capabilities"} {
		if err := i.router.Handle("GET", path, http.HandlerFunc(i.optionalCapability)); err != nil {
			return err
		}
	}
	for _, e := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/api/v2/openapi.json", i.openAPI}, {"/api/v2/theme/branding", i.branding}, {"/api/v2/system/identity", i.systemIdentity}, {"/api/v2/requests/status", i.requestStatus}, {"/api/v2/system/info", i.systemInfo}, {"/api/v2/system/setup", i.setup}, {"/api/v2/account/me", i.account}, {"/api/v2/profiles", i.profiles}} {
		if err := i.router.Handle("GET", e.path, e.h); err != nil {
			return err
		}
	}
	return nil
}
