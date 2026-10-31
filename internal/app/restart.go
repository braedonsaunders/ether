package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/service"
)

// restartState is a restart of the running server: requested by an
// installed update, a guided restore or the owner. Run's context is
// cancelled, the server drains and stops, and Run returns
// service.ErrRestartRequested; the service manager (or, in a container or
// terminal, the process itself) starts a fresh one.
type restartState struct {
	mu     sync.Mutex
	ctx    context.Context    // Run's context
	cancel context.CancelFunc // cancels Run
	reason string             // set once a restart was requested
	// waiting is a restart held until nobody is watching.
	waiting *restartWait
	// active overrides activeSessionCount (tests).
	active func() int
}

// restartWait is a restart waiting for playback to end.
type restartWait struct {
	Reason    string    `json:"reason"`
	Requested time.Time `json:"requested"`
	stop      chan struct{}
}

// restartPoll is how often a held restart checks for viewers.
var restartPoll = 5 * time.Second

// restartActive counts what a restart would interrupt.
func (a *App) restartActive() int {
	if a.restart.active != nil {
		return a.restart.active()
	}
	return a.activeSessionCount()
}

// bindRestart lets restarts cancel this Run.
func (a *App) bindRestart(ctx context.Context, cancel context.CancelFunc) {
	a.restart.mu.Lock()
	a.restart.ctx, a.restart.cancel = ctx, cancel
	a.restart.mu.Unlock()
}

// requestRestart stops Run now so the server restarts.
func (a *App) requestRestart(reason string) {
	a.restart.mu.Lock()
	a.restart.reason = reason
	cancel := a.restart.cancel
	if w := a.restart.waiting; w != nil {
		close(w.stop)
		a.restart.waiting = nil
	}
	a.restart.mu.Unlock()
	a.log.Info("restarting Ether", "reason", reason)
	if cancel != nil {
		cancel()
	}
}

// scheduleRestart restarts now, or with whenIdle once playback, transcodes
// and live TV have ended (at once when nothing is playing).
func (a *App) scheduleRestart(reason string, whenIdle bool) error {
	if !whenIdle || a.restartActive() == 0 {
		a.requestRestart(reason)
		return nil
	}
	a.restart.mu.Lock()
	defer a.restart.mu.Unlock()
	if a.restart.ctx == nil {
		return fmt.Errorf("the server is not running")
	}
	if w := a.restart.waiting; w != nil {
		close(w.stop)
	}
	w := &restartWait{Reason: reason, Requested: time.Now().UTC(), stop: make(chan struct{})}
	a.restart.waiting = w
	ctx := a.restart.ctx
	a.log.Info("restart waiting until nobody is watching", "reason", reason)
	go a.restartWhenIdle(ctx, w)
	return nil
}

func (a *App) restartWhenIdle(ctx context.Context, w *restartWait) {
	t := time.NewTicker(restartPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-t.C:
			if a.restartActive() > 0 {
				continue
			}
			a.restart.mu.Lock()
			current := a.restart.waiting == w
			a.restart.mu.Unlock()
			if current {
				a.requestRestart(w.Reason)
			}
			return
		}
	}
}

// cancelRestart drops a held restart and the restore it was for. A restore
// left pending without a held restart (its restart could not start) is
// dropped too, unless the server is already restarting into it.
func (a *App) cancelRestart() bool {
	a.restart.mu.Lock()
	w := a.restart.waiting
	a.restart.waiting = nil
	restarting := a.restart.reason != ""
	a.restart.mu.Unlock()
	if w != nil {
		close(w.stop)
		a.log.Info("held restart cancelled", "reason", w.Reason)
	}
	if restarting {
		return w != nil
	}
	pending, _ := backup.ReadPending(a.opts.DataDir)
	_ = backup.CancelPending(a.opts.DataDir)
	return w != nil || pending != nil
}

// runResult is Run's return value after a graceful shutdown.
func (a *App) runResult() error {
	a.restart.mu.Lock()
	defer a.restart.mu.Unlock()
	if a.restart.reason != "" {
		return fmt.Errorf("%s: %w", a.restart.reason, service.ErrRestartRequested)
	}
	return nil
}

// restartStatus is what the console shows before the owner restarts.
type restartStatus struct {
	service.Restart
	// Active counts what a restart interrupts; Sessions lists the playback
	// sessions (who is watching what).
	Active   int          `json:"active"`
	Sessions any          `json:"sessions"`
	Waiting  *restartWait `json:"waiting,omitempty"`
	// Clustered nodes hand their work to another node while restarting.
	Clustered bool `json:"clustered"`
}

func (a *App) restartStatus() restartStatus {
	st := restartStatus{Restart: service.DetectRestart(), Active: a.restartActive(), Clustered: a.clustered()}
	st.Sessions = (ownerPlayback{Playback: a.playback, app: a}).Sessions()
	a.restart.mu.Lock()
	if w := a.restart.waiting; w != nil {
		cp := *w
		st.Waiting = &cp
	}
	a.restart.mu.Unlock()
	return st
}

// restartHandler serves /ether/api/server/restart/ (owner auth, LAN and
// CSRF are enforced by the admin handler):
//
//	GET    /  how a restart happens here and who would be interrupted
//	POST   /  {"whenIdle": bool} restart now, or once nobody is watching
//	DELETE /  cancel a restart waiting for viewers
func (a *App) restartHandler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	owner := func(w http.ResponseWriter, r *http.Request) bool {
		if acc, ok := admin.RequestAccess(r.Context()); ok && (acc.Owner || !acc.SetupComplete) {
			return true
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "owner_required", "message": "sign in as the owner to restart Ether"}})
		return false
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if owner(w, r) {
			writeJSON(w, http.StatusOK, a.restartStatus())
		}
	})
	mux.HandleFunc("POST /{$}", func(w http.ResponseWriter, r *http.Request) {
		if !owner(w, r) {
			return
		}
		var body struct {
			WhenIdle bool `json:"whenIdle"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "bad_request", "message": "invalid JSON body"}})
				return
			}
		}
		if err := a.scheduleRestart("restart requested from the console", body.WhenIdle); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "restart_failed", "message": err.Error()}})
			return
		}
		writeJSON(w, http.StatusAccepted, a.restartStatus())
	})
	mux.HandleFunc("DELETE /{$}", func(w http.ResponseWriter, r *http.Request) {
		if owner(w, r) {
			writeJSON(w, http.StatusOK, map[string]bool{"cancelled": a.cancelRestart()})
		}
	})
	return mux
}
