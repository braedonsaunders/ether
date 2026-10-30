package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/updater"
)

// updateState wires the self-update manager into the app lifecycle.
type updateState struct {
	boot updater.StartupResult
	mgr  *updater.Manager

	mu      sync.Mutex
	cancel  context.CancelFunc // cancels Run
	restart string             // reason, once a restart was requested
}

func (a *App) updatesDir() string { return filepath.Join(a.opts.DataDir, "updates") }

// confirmUpdateStartup runs before anything a bad release could break
// (config, migrations): it counts starts of a just-installed version and
// restores the previous binary after a crash loop. A rollback returns an
// error wrapping service.ErrRestartRequested so the service manager starts
// the restored binary.
func (a *App) confirmUpdateStartup() error {
	a.updates = &updateState{}
	res, err := updater.ConfirmStartup(updater.StartupOptions{
		Dir:            a.updatesDir(),
		CurrentVersion: a.opts.Version,
		Logger:         a.log,
	})
	if err != nil {
		a.log.Warn("update startup check", "err", err)
	}
	a.updates.boot = res
	if res.RestartRequired {
		return fmt.Errorf("rolled back update %s: %w", res.RolledBack.Version, service.ErrRestartRequested)
	}
	return nil
}

// initUpdates builds the manager. Without a compiled-in release key the
// client cannot be built and checks stay disabled, but the manager still
// confirms a pending self-update.
func (a *App) initUpdates() {
	if a.updates == nil {
		a.updates = &updateState{}
	}
	exe, _ := os.Executable()
	if exe != "" {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	selfApply, why := updater.SelfApplyCapability(updater.Environment{
		GOOS:        runtime.GOOS,
		Target:      exe,
		Dev:         updater.IsDev() || a.opts.Version == "dev",
		InContainer: updater.InContainer(),
		Supervised:  !service.Interactive(),
	})
	log := a.log.With("component", "updater")
	// A clustered node that installs its own updates takes its turn in a
	// rolling update (rolling.go); notify-only nodes just report.
	var coord updater.Coordinator
	if a.clustered() && selfApply {
		coord = a.newClusterUpdates()
	}
	a.updates.mgr = updater.NewManager(updater.ManagerOptions{
		Coordinator: coord,
		NodeName:    a.cluster.Self().Name,
		NewClient: func(u config.Updates) (updater.Checker, error) {
			return updater.NewClient(updater.Options{
				FeedURL:        u.FeedURL,
				Channel:        u.Channel,
				CurrentVersion: a.opts.Version,
				Logger:         log,
			})
		},
		Config:         func() config.Updates { return a.cfg.Get().Updates },
		CurrentVersion: a.opts.Version,
		Dir:            a.updatesDir(),
		Target:         exe,
		SelfApply:      selfApply,
		NotifyReason:   why,
		ActiveSessions: a.activeSessionCount,
		RequestRestart: a.requestRestart,
		Boot:           a.updates.boot,
		Logger:         log,
		OnAvailable: func(version string) error {
			if a.notifications == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Once per version across restarts and cluster nodes; an error
			// is retried at the next check.
			return a.notifications.UpdateAvailable(ctx, a.opts.Version, version)
		},
	})
	if !a.updates.mgr.Enabled() {
		log.Debug("update checks disabled", "reason", a.updates.mgr.Status().DisabledReason)
	}
}

// activeSessionCount is everything an update restart would interrupt:
// playback sessions, transcodes and live TV viewers.
func (a *App) activeSessionCount() int {
	n := len(a.playback.Sessions())
	if a.transcoder != nil {
		n += len(a.transcoder.Sessions())
	}
	if a.livetv != nil {
		n += len(a.livetv.ViewingSessions())
	}
	return n
}

// runUpdates drives the manager for the lifetime of Run; cancel stops Run
// when an installed update needs a restart.
func (a *App) runUpdates(ctx context.Context, cancel context.CancelFunc) {
	if a.updates == nil || a.updates.mgr == nil {
		return
	}
	a.updates.mu.Lock()
	a.updates.cancel = cancel
	a.updates.mu.Unlock()
	a.updates.mgr.Run(ctx)
}

func (a *App) requestRestart(reason string) {
	a.updates.mu.Lock()
	a.updates.restart = reason
	cancel := a.updates.cancel
	a.updates.mu.Unlock()
	a.log.Info("restarting to finish update", "reason", reason)
	if cancel != nil {
		cancel()
	}
}

// runResult is Run's return value after a graceful shutdown.
func (a *App) runResult() error {
	if a.updates == nil {
		return nil
	}
	a.updates.mu.Lock()
	defer a.updates.mu.Unlock()
	if a.updates.restart != "" {
		return fmt.Errorf("%s: %w", a.updates.restart, service.ErrRestartRequested)
	}
	return nil
}

func (a *App) updateStatus(context.Context) any {
	if a.updates == nil || a.updates.mgr == nil {
		return map[string]any{"enabled": false, "capability": updater.CapabilityDisabled, "state": updater.StateIdle}
	}
	return a.updates.mgr.Status()
}

// updatesHandler serves /ether/api/updates/ (owner auth, LAN and CSRF are
// enforced by the admin handler):
//
//	GET  /       status
//	POST /apply  {"force": bool} — install as soon as idle (force: now)
func (a *App) updatesHandler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	writeError := func(w http.ResponseWriter, code int, kind, msg string) {
		writeJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.updateStatus(r.Context()))
	})
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Force bool `json:"force"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
				return
			}
		}
		if a.updates == nil || a.updates.mgr == nil {
			writeError(w, http.StatusServiceUnavailable, "updates_disabled", updater.ErrUpdatesDisabled.Error())
			return
		}
		if err := a.updates.mgr.UpdateNow(body.Force); err != nil {
			code, kind := http.StatusConflict, "not_self_applicable"
			if errors.Is(err, updater.ErrUpdatesDisabled) {
				code, kind = http.StatusServiceUnavailable, "updates_disabled"
			}
			writeError(w, code, kind, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, a.updates.mgr.Status())
	})
	return mux
}
