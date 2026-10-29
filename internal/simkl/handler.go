package simkl

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// HandlerDeps are explicitly provided by the owner-authenticated admin API.
// Viewer validates that an ID identifies a current viewer, never a caller-
// supplied arbitrary account. Authentication is enforced by the mounting API.
type HandlerDeps struct {
	Device  *DeviceManager
	Service func() Service
	Viewer  func(context.Context, int64) bool
	Enabled func() bool
}

func Handler(d HandlerDeps) http.Handler {
	mux := http.NewServeMux()
	run := func(w http.ResponseWriter, r *http.Request, op string) {
		user, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
		if err != nil || user <= 0 || d.Viewer == nil || !d.Viewer(r.Context(), user) {
			http.Error(w, "viewer not found", http.StatusNotFound)
			return
		}
		if d.Device == nil || d.Service == nil || d.Enabled == nil {
			http.Error(w, "integration unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var result any
		switch op {
		case "status":
			grant, loadErr := d.Device.Tokens.Load(r.Context(), user)
			err = loadErr
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
				result = map[string]any{"connected": false}
			} else if err == nil {
				var snapshot, warning string
				var queued int
				var backoff int64
				err = d.Device.Tokens.DB.QueryRowContext(r.Context(), `SELECT snapshot,last_warning,backoff_until,(SELECT COUNT(*) FROM simkl_outbox WHERE user_id=?) FROM simkl_tokens WHERE user_id=?`, user, user).Scan(&snapshot, &warning, &backoff, &queued)
				if err == nil {
					result = map[string]any{"connected": true, "needsReconnect": grant.ClientID != d.Service().Auth.ClientID || warning == "auth_revoked", "lastSync": snapshot, "warning": warning, "backoffUntil": backoff, "queued": queued}
				}
			}
		case "start":
			if !d.Enabled() {
				http.Error(w, "integration disabled", http.StatusConflict)
				return
			}
			result, err = d.Device.Start(r.Context(), user)
		case "poll":
			if !d.Enabled() {
				http.Error(w, "integration disabled", http.StatusConflict)
				return
			}
			var status string
			status, err = d.Device.Poll(r.Context(), user)
			result = map[string]string{"status": status}
		case "disconnect":
			err = d.Device.Disconnect(r.Context(), user)
			result = map[string]bool{"connected": false}
		case "sync":
			if !d.Enabled() {
				http.Error(w, "integration disabled", http.StatusConflict)
				return
			}
			result, err = d.Service().Sync(r.Context(), user)
		}
		if err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, ErrBusy) || errors.Is(err, ErrPollEarly) {
				code = http.StatusConflict
			}
			if errors.Is(err, ErrExpired) || errors.Is(err, ErrNoPending) {
				code = http.StatusGone
			}
			http.Error(w, http.StatusText(code), code)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
	mux.HandleFunc("GET /{user}/status", func(w http.ResponseWriter, r *http.Request) { run(w, r, "status") })
	mux.HandleFunc("POST /{user}/start", func(w http.ResponseWriter, r *http.Request) { run(w, r, "start") })
	mux.HandleFunc("POST /{user}/poll", func(w http.ResponseWriter, r *http.Request) { run(w, r, "poll") })
	mux.HandleFunc("POST /{user}/disconnect", func(w http.ResponseWriter, r *http.Request) { run(w, r, "disconnect") })
	mux.HandleFunc("POST /{user}/sync", func(w http.ResponseWriter, r *http.Request) { run(w, r, "sync") })
	return mux
}
