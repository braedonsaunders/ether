package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
)

// FavoritesMirror is viewer presentation state, not the favorites themselves.
type FavoritesMirror interface {
	MirrorEnabled(context.Context, *core.User) (bool, error)
	SetMirror(context.Context, *core.User, bool) error
}

func (h *Handler) favoritesMirror(w http.ResponseWriter, r *http.Request, viewer *core.Viewer) {
	if h.deps.FavoritesMirror == nil {
		writeError(w, 503, "unavailable", "favourites unavailable")
		return
	}
	if r.Method == http.MethodGet {
		enabled, err := h.deps.FavoritesMirror.MirrorEnabled(r.Context(), viewer)
		if err != nil {
			writeError(w, 404, "not_found", "viewer unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"enabled": enabled})
		return
	}
	if !csrfOK(r) {
		writeError(w, 403, "csrf", "request rejected")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if decodeJSON(r, &body) != nil || body.Enabled == nil {
		writeError(w, 400, "invalid", "invalid request")
		return
	}
	if err := h.deps.FavoritesMirror.SetMirror(r.Context(), viewer, *body.Enabled); err != nil {
		writeError(w, 400, "unavailable", "could not update favourites")
		return
	}
	writeJSON(w, 200, map[string]bool{"enabled": *body.Enabled})
}
func (h *Handler) portalFavoritesMirror(w http.ResponseWriter, r *http.Request) {
	viewer, err := h.portalViewer(r)
	if err != nil {
		writeError(w, 401, "unauthorized", "sign in required")
		return
	}
	h.favoritesMirror(w, r, viewer)
}
func (h *Handler) ownerFavoritesMirror(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.ActiveViewer == nil {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid", "invalid viewer")
		return
	}
	viewer, err := h.deps.ActiveViewer(r.Context(), id)
	if err != nil || viewer == nil {
		writeError(w, 404, "not_found", "viewer unavailable")
		return
	}
	h.favoritesMirror(w, r, viewer)
}
