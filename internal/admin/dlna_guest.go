package admin

import "net/http"

func (h *Handler) ownerCreateDLNAGuest(w http.ResponseWriter, r *http.Request) {
	access, ok := RequestAccess(r.Context())
	if !ok || !access.Owner || !h.isOwner(r, tokenFromRequest(r)) {
		writeError(w, 403, "forbidden", "owner required")
		return
	}
	if h.deps.CreateDLNAGuest == nil {
		writeError(w, 503, "unavailable", "guest creation unavailable")
		return
	}
	viewer, err := h.deps.CreateDLNAGuest(r.Context())
	if err != nil || viewer == nil {
		writeError(w, 500, "unavailable", "could not create guest")
		return
	}
	writeJSON(w, http.StatusCreated, viewer)
}
