package admin

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
)

type NotInterestedEntry struct {
	Handle string `json:"handle"`
	Title  string `json:"title,omitempty"`
	Key    string `json:"key,omitempty"`
}
type NotInterestedPortal interface {
	ListNotInterested(context.Context, *core.User) ([]NotInterestedEntry, error)
	RestoreNotInterested(context.Context, *core.User, string) (bool, error)
}

func (h *Handler) portalNotInterested(w http.ResponseWriter, r *http.Request) {
	viewer, err := h.portalViewer(r)
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	if h.deps.NotInterested == nil {
		writeError(w, 503, "unavailable", "unavailable")
		return
	}
	if r.Method == http.MethodGet {
		entries, err := h.deps.NotInterested.ListNotInterested(r.Context(), viewer)
		if err != nil {
			writeError(w, 503, "unavailable", "unavailable")
			return
		}
		if entries == nil {
			entries = []NotInterestedEntry{}
		}
		writeJSON(w, 200, map[string]any{"items": entries})
		return
	}
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	var body struct {
		Handle string `json:"handle"`
	}
	if decodeJSON(r, &body) != nil || body.Handle == "" {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	restored, err := h.deps.NotInterested.RestoreNotInterested(r.Context(), viewer, body.Handle)
	if err != nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	writeJSON(w, 200, map[string]bool{"restored": restored})
}
