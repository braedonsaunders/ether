//go:build plexparity

package plex

import (
	"context"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
)

// handleParityWarmSettled is the harness hook. With no ratingKey it waits
// until every pending warm probe has finished or failed. With a ratingKey it
// starts that title's warm-up when none is recorded, then waits for it.
// Neither form renders media. The parity build is the only build that mounts it.
func (s *Server) handleParityWarmSettled(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	if pr == nil || pr.User == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("ratingKey") == "" {
		settle, ok := s.d.Playback.(interface {
			ParitySettlePending(context.Context) error
		})
		if !ok || s.d.Playback == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if err := settle.ParitySettlePending(r.Context()); err != nil {
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key, err := strconv.ParseInt(r.URL.Query().Get("ratingKey"), 10, 64)
	if err != nil || key <= 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	settle, ok := s.d.Playback.(interface {
		ParitySettleWarm(context.Context, *core.User, core.RatingKey, core.ClientCaps) error
	})
	if !ok || s.d.Playback == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if err := settle.ParitySettleWarm(r.Context(), pr.User, core.RatingKey(key), s.clientCaps(pr)); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleParityWarmProbe probes each requested title before returning.
// The parity build is the only build that mounts it.
func (s *Server) handleParityWarmProbe(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	if pr == nil || pr.User == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	keys := r.URL.Query()["ratingKey"]
	if len(keys) == 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	probe, ok := s.d.Playback.(interface {
		ParityProbeNow(context.Context, *core.User, core.RatingKey, core.ClientCaps) error
	})
	if !ok || s.d.Playback == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// clientCaps, not the registry helper: this file is copied onto the
	// parity baseline, which only has clientCaps.
	caps := s.clientCaps(pr)
	for _, raw := range keys {
		key, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || key <= 0 {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if err := probe.ParityProbeNow(r.Context(), pr.User, core.RatingKey(key), caps); err != nil {
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
