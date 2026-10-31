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
	if err := settle.ParitySettleWarm(r.Context(), pr.User, core.RatingKey(key), pr.ClientCaps(s.cfg().Transcode.RemoteMaxBitrateKbps)); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
