//go:build plexparity

package plex

import (
	"context"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
)

// handleParityWarmSettled is the harness hook before tracks:before. It waits
// until this viewer's warm probe for the title has finished or failed, and it
// does not render media. The parity build is the only build that mounts it.
func (s *Server) handleParityWarmSettled(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	if pr == nil || pr.User == nil {
		writeHTTPError(w, http.StatusBadRequest)
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
