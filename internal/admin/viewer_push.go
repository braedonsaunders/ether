package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/notify"
)

// ViewerPushService holds browser destinations under the current viewer.
type ViewerPushService interface {
	PublicKey(context.Context, *core.User) (string, error)
	Register(context.Context, *core.User, notify.PushSubscription, string) error
	Unregister(context.Context, *core.User, string) error
	Registered(context.Context, *core.User, string) (bool, error)
}

// viewerPushRoutes is installed in the viewer portal's authenticated mux.
func (h *Handler) viewerPushRoutes(mux *http.ServeMux, push ViewerPushService) {
	mux.HandleFunc("GET /api/me/alerts/push/key", func(w http.ResponseWriter, r *http.Request) {
		viewer, err := h.portalViewer(r)
		if err != nil {
			writeError(w, 401, "unauthorized", "sign in required")
			return
		}
		if push == nil {
			writeError(w, 503, "unavailable", "push unavailable")
			return
		}
		key, err := push.PublicKey(r.Context(), viewer)
		if err != nil {
			writeError(w, 503, "unavailable", "push unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]string{"publicKey": key})
	})
	mutate := func(w http.ResponseWriter, r *http.Request) (*core.User, bool) {
		viewer, err := h.portalViewer(r)
		if err != nil {
			writeError(w, 401, "unauthorized", "sign in required")
			return nil, false
		}
		if !csrfOK(r) {
			writeError(w, 403, "csrf", "request rejected")
			return nil, false
		}
		if push == nil {
			writeError(w, 503, "unavailable", "push unavailable")
			return nil, false
		}
		w.Header().Set("Cache-Control", "no-store")
		return viewer, true
	}
	mux.HandleFunc("PUT /api/me/alerts/push/subscription", func(w http.ResponseWriter, r *http.Request) {
		viewer, ok := mutate(w, r)
		if !ok {
			return
		}
		var sub notify.PushSubscription
		sum := sha256.Sum256([]byte(tokenFromRequest(r)))
		if decodeJSON(r, &sub) != nil || push.Register(r.Context(), viewer, sub, hex.EncodeToString(sum[:])) != nil {
			writeError(w, 400, "invalid", "could not register device")
			return
		}
		writeJSON(w, 200, map[string]bool{"registered": true})
	})
	for _, route := range []string{"DELETE /api/me/alerts/push/subscription", "POST /api/me/alerts/push/status"} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			viewer, ok := mutate(w, r)
			if !ok {
				return
			}
			var body struct {
				Endpoint string `json:"endpoint"`
			}
			if decodeJSON(r, &body) != nil || body.Endpoint == "" || len(body.Endpoint) > 4096 {
				writeError(w, 400, "invalid", "invalid device")
				return
			}
			if r.Method == http.MethodDelete {
				if push.Unregister(r.Context(), viewer, body.Endpoint) != nil {
					writeError(w, 400, "invalid", "could not unregister device")
					return
				}
				writeJSON(w, 200, map[string]bool{"registered": false})
				return
			}
			registered, err := push.Registered(r.Context(), viewer, body.Endpoint)
			if err != nil {
				writeError(w, 503, "unavailable", "push unavailable")
				return
			}
			writeJSON(w, 200, map[string]bool{"registered": registered})
		})
	}
}
