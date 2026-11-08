package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// PlexWatchStatus is the viewer-facing connection. It never includes a token.
// Problem is "", "token" (connect again), "rate_limited" or "network".
type PlexWatchStatus struct {
	Connected      bool   `json:"connected"`
	PlexID         int64  `json:"plexId,omitempty"`
	Username       string `json:"username,omitempty"`
	Direction      int    `json:"direction"`
	ImportInterval int    `json:"importInterval"`
	LastImport     int64  `json:"lastImport"`
	LastFullImport int64  `json:"lastFullImport"`
	LastSend       int64  `json:"lastSend"`
	NextImport     int64  `json:"nextImport"`
	Problem        string `json:"problem"`
	NeedsReconnect bool   `json:"needsReconnect"`
	Pending        int    `json:"pending"`
	Failed         int    `json:"failed"`
	Restricted     bool   `json:"restricted"`
	PINCode        string `json:"pinCode,omitempty"`
	PINURL         string `json:"pinUrl,omitempty"`
	PINExpires     string `json:"pinExpires,omitempty"`
	AccountMatch   bool   `json:"accountMatch"`
}

// Sentinels returned by the app adapter so this package does not import
// the watch client.
var (
	ErrPlexAccountMismatch   = errors.New("plex account does not belong to this viewer")
	ErrPlexWatchRevoked      = errors.New("plex rejected this viewer's token")
	ErrPlexWatchNotConnected = errors.New("this viewer has not connected a Plex account")
	ErrPlexWatchDirectionOff = errors.New("that sync direction is off for this viewer")
)

// PlexWatch is the per-viewer account watch-state integration. Handlers
// always pass the stored viewer's Plex account id, never the owner's.
type PlexWatch interface {
	Status(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
	BeginPIN(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
	FinishPIN(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
	SetDirection(ctx context.Context, userID, plexID int64, direction, interval int) (PlexWatchStatus, error)
	Disconnect(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
	Retry(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
	ExportCurrent(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
	ImportCurrent(ctx context.Context, userID, plexID int64) (PlexWatchStatus, error)
}

func (h *Handler) plexWatchViewer(w http.ResponseWriter, r *http.Request) (*ViewerRecord, bool) {
	if h.deps.PlexWatch == nil {
		writeError(w, http.StatusNotFound, "not_found", "plex account watch state is not available")
		return nil, false
	}
	v, ok := h.viewerByID(w, r)
	if !ok {
		return nil, false
	}
	if v.PlexID <= 0 {
		writeError(w, http.StatusBadRequest, "no_plex_id", "this viewer has no Plex account id")
		return nil, false
	}
	return v, true
}

func (h *Handler) writePlexWatch(w http.ResponseWriter, st PlexWatchStatus, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, st)
		return
	}
	switch {
	case errors.Is(err, ErrPlexAccountMismatch):
		writeError(w, http.StatusConflict, "account_mismatch", "the Plex account that approved the code is not this viewer")
	case errors.Is(err, ErrPlexWatchRevoked):
		writeError(w, http.StatusUnauthorized, "revoked", "Plex rejected this viewer's token")
	case errors.Is(err, ErrPlexWatchNotConnected):
		writeError(w, http.StatusConflict, "not_connected", "this viewer has not connected a Plex account")
	case errors.Is(err, ErrPlexWatchDirectionOff):
		writeError(w, http.StatusConflict, "direction_off", "that sync direction is off for this viewer")
	default:
		writeError(w, http.StatusBadRequest, "plex_watch", "plex watch state could not be updated")
	}
}

// plexWatchCall runs one viewer action with the viewer's stored Plex id.
func (h *Handler) plexWatchCall(w http.ResponseWriter, r *http.Request, call func(PlexWatch, context.Context, int64, int64) (PlexWatchStatus, error)) {
	v, ok := h.plexWatchViewer(w, r)
	if !ok {
		return
	}
	st, err := call(h.deps.PlexWatch, r.Context(), v.ID, v.PlexID)
	h.writePlexWatch(w, st, err)
}

func (h *Handler) handlePlexWatchStatus(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.Status)
}

func (h *Handler) handlePlexWatchPIN(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.BeginPIN)
}

func (h *Handler) handlePlexWatchPINFinish(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.FinishPIN)
}

func (h *Handler) handlePlexWatchDisconnect(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.Disconnect)
}

func (h *Handler) handlePlexWatchRetry(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.Retry)
}

func (h *Handler) handlePlexWatchExport(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.ExportCurrent)
}

func (h *Handler) handlePlexWatchImport(w http.ResponseWriter, r *http.Request) {
	h.plexWatchCall(w, r, PlexWatch.ImportCurrent)
}

func (h *Handler) handlePlexWatchEnabled(w http.ResponseWriter, r *http.Request) {
	v, ok := h.plexWatchViewer(w, r)
	if !ok {
		return
	}
	var body struct {
		Direction int `json:"direction"`
		Interval  int `json:"interval"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "expected a JSON object with direction and interval")
		return
	}
	st, err := h.deps.PlexWatch.SetDirection(r.Context(), v.ID, v.PlexID, body.Direction, body.Interval)
	h.writePlexWatch(w, st, err)
}
