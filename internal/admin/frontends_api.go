package admin

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// FrontendAdmin is supplied by the app's protocol lifecycle manager.
type FrontendAdmin interface {
	List(context.Context) ([]FrontendInfo, error)
	Settings(context.Context, frontend.Protocol) (frontend.Settings, error)
	PutSettings(context.Context, frontend.Protocol, frontend.Settings) error
	Toggle(context.Context, frontend.Protocol, bool) error
}
type FrontendInfo struct {
	Descriptor frontend.Descriptor `json:"descriptor"`
	Enabled    bool                `json:"enabled"`
	Status     frontend.Status     `json:"status"`
}

func (h *Handler) frontendAvailable(w http.ResponseWriter) bool {
	if h.deps.Frontends == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Front-end management is unavailable")
		return false
	}
	return true
}
func (h *Handler) frontendList(w http.ResponseWriter, r *http.Request) {
	if !h.frontendAvailable(w) {
		return
	}
	list, err := h.deps.Frontends.List(r.Context())
	if err != nil {
		writeError(w, 500, "frontend_error", "Could not list front ends")
		return
	}
	writeJSON(w, 200, list)
}
func (h *Handler) frontendStatus(w http.ResponseWriter, r *http.Request) {
	if !h.frontendAvailable(w) {
		return
	}
	list, err := h.deps.Frontends.List(r.Context())
	if err != nil {
		writeError(w, 500, "frontend_error", "Could not read front-end status")
		return
	}
	for _, item := range list {
		if string(item.Descriptor.ID) == r.PathValue("id") {
			writeJSON(w, 200, item.Status)
			return
		}
	}
	writeError(w, 404, "not_found", "Front end not found")
}
func (h *Handler) frontendSettings(w http.ResponseWriter, r *http.Request) {
	if !h.frontendAvailable(w) {
		return
	}
	id := frontend.Protocol(r.PathValue("id"))
	if r.Method == http.MethodGet {
		settings, err := h.deps.Frontends.Settings(r.Context(), id)
		if err != nil {
			writeError(w, 404, "not_found", "Front end not found")
			return
		}
		writeJSON(w, 200, settings)
		return
	}
	var settings frontend.Settings
	if err := decodeJSON(r, &settings); err != nil || settings == nil {
		writeError(w, 400, "invalid_json", "Invalid front-end settings")
		return
	}
	if err := h.deps.Frontends.PutSettings(r.Context(), id, settings); err != nil {
		writeError(w, 400, "frontend_error", "Could not update front end")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (h *Handler) frontendToggle(w http.ResponseWriter, r *http.Request) {
	if !h.frontendAvailable(w) {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Enabled == nil {
		writeError(w, 400, "invalid_json", "Enabled is required")
		return
	}
	if err := h.deps.Frontends.Toggle(r.Context(), frontend.Protocol(r.PathValue("id")), *body.Enabled); err != nil {
		writeError(w, 400, "frontend_error", "Could not update front end")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// frontendHealth reports readiness on this node, including disabled protocols.
// It uses the owner API so probes do not expose listeners or failure details.
func (h *Handler) frontendHealth(w http.ResponseWriter, r *http.Request) {
	if !h.frontendAvailable(w) {
		return
	}
	list, err := h.deps.Frontends.List(r.Context())
	if err != nil {
		writeError(w, 503, "unavailable", "Could not read front-end health")
		return
	}
	for _, item := range list {
		if string(item.Descriptor.ID) != r.PathValue("id") {
			continue
		}
		healthy := item.Enabled && item.Status.Mounted && item.Status.Error == ""
		status := http.StatusOK
		if !healthy {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"protocol": item.Descriptor.ID, "healthy": healthy, "enabled": item.Enabled, "mounted": item.Status.Mounted})
		return
	}
	writeError(w, 404, "not_found", "Front end not found")
}
