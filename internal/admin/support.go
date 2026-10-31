package admin

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/braedonsaunders/ether/internal/diagnostics"
)

var supportID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (h *Handler) support() *diagnostics.Recorder {
	return h.deps.Support
}

func (h *Handler) handleSupportDevices(w http.ResponseWriter, r *http.Request) {
	rec := h.support()
	if rec == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": rec.Devices()})
}

func (h *Handler) handleSupportCreate(w http.ResponseWriter, r *http.Request) {
	rec := h.support()
	if rec == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
		return
	}
	var in diagnostics.StartIn
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "the request body must be a JSON object")
		return
	}
	view, err := rec.Start(tokenFromRequest(r), in)
	if err != nil {
		writeSupportErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (h *Handler) handleSupportGet(w http.ResponseWriter, r *http.Request) {
	rec, id, ok := h.supportCapture(w, r)
	if !ok {
		return
	}
	view, err := rec.Get(tokenFromRequest(r), id)
	if err != nil {
		writeSupportErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) handleSupportDevice(w http.ResponseWriter, r *http.Request) {
	rec, id, ok := h.supportCapture(w, r)
	if !ok {
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "the request body must be a JSON object")
		return
	}
	view, err := rec.SelectDevice(tokenFromRequest(r), id, body.DeviceID)
	if err != nil {
		writeSupportErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) handleSupportFinish(w http.ResponseWriter, r *http.Request) {
	rec, id, ok := h.supportCapture(w, r)
	if !ok {
		return
	}
	var in diagnostics.FinishIn
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "the request body must be a JSON object")
		return
	}
	in.SetFlags = true
	view, err := rec.Finish(tokenFromRequest(r), id, in)
	if err != nil {
		writeSupportErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *Handler) handleSupportDiscard(w http.ResponseWriter, r *http.Request) {
	rec, id, ok := h.supportCapture(w, r)
	if !ok {
		return
	}
	if err := rec.Discard(tokenFromRequest(r), id); err != nil {
		writeSupportErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSupportArchive(w http.ResponseWriter, r *http.Request) {
	rec, id, ok := h.supportCapture(w, r)
	if !ok {
		return
	}
	blob, err := rec.Archive(tokenFromRequest(r), id)
	if err != nil {
		writeSupportErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="ether-support.zip"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (h *Handler) supportCapture(w http.ResponseWriter, r *http.Request) (*diagnostics.Recorder, string, bool) {
	rec := h.support()
	if rec == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
		return nil, "", false
	}
	id := r.PathValue("id")
	if !supportID.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return nil, "", false
	}
	return rec, id, true
}

func writeSupportErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, diagnostics.ErrStaleDevice):
		writeError(w, http.StatusConflict, "stale_device", "that device is not in the recent list")
	case errors.Is(err, diagnostics.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, "too_many", "too many support captures are already open")
	case errors.Is(err, diagnostics.ErrNotRecording):
		writeError(w, http.StatusConflict, "not_recording", "that capture is not recording")
	default:
		writeError(w, http.StatusNotFound, "not_found", "not found")
	}
}
