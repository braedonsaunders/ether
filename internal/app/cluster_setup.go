package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/startup"
	"github.com/braedonsaunders/ether/internal/updater"
)

var errClusterSetup = errors.New("cluster setup requested")

type clusterSetup struct {
	check   func(context.Context, clusterconfig.SetupRequest) error
	execute func(context.Context, string, clusterconfig.SetupRequest) error
	mu      sync.Mutex
	request *clusterconfig.SetupRequest
	state   string
	message string
	stop    chan struct{}
}

// Serve owns the complete application lifetime. It binds the port as soon
// as the data directory is locked, so the console can show a starting
// server's progress (startup.Gate), and keeps it across App instances. A UI
// cluster transition first stops HTTP and every background writer and
// closes SQLite, then migrates and starts a fresh App. A start that fails
// after the port is bound does not exit (a service manager would only
// restart it into the same failure): the status page explains the failure,
// offers a restore on a standalone server, and the start is retried with
// backoff. It works in the foreground and under every OS supervisor.
func Serve(ctx context.Context, opts Options) error {
	setup := &clusterSetup{state: "idle", stop: make(chan struct{}, 1)}
	opts.clusterSetup = setup
	if opts.Version == "" {
		opts.Version = updater.Version
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	front := newFrontDoor(startup.NewTracker(opts.Version, time.Now), log)
	defer front.close()
	opts.front = front
	failures := 0
	for {
		a, err := New(ctx, opts)
		if err != nil {
			if !front.bound() || ctx.Err() != nil || errors.Is(err, service.ErrRestartRequested) {
				return err
			}
			failures++
			wait := startRetryDelay(failures)
			front.tracker.Fail(failureOf(err, standaloneDataDir(opts.DataDir)), failures, wait)
			log.Error("Ether could not start; the console at /ether/ explains why. Retrying", "err", err, "attempt", failures, "retry", wait)
			front.recovery.Store(ptr(newRecovery(opts.DataDir, front, log).handler()))
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-t.C:
			case <-front.retry:
				t.Stop()
			}
			front.recovery.Store(nil)
			continue
		}
		failures = 0
		finish := func(bool) error { return nil }
		if opts.NotificationExecutable != "" {
			watch, watchErr := a.WatchNotifications(opts.NotificationExecutable)
			if watchErr != nil {
				a.log.Warn("crash notification watcher unavailable")
			} else {
				finish = watch
			}
		}
		err = a.Run(ctx)
		// A cluster transition deliberately replaces this App within the same
		// process. Close its watcher cleanly before starting the replacement.
		_ = finish(err == nil || errors.Is(err, errClusterSetup) || errors.Is(err, service.ErrRestartRequested))
		a.Close()
		if !errors.Is(err, errClusterSetup) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := setup.apply(ctx, a.opts.DataDir); err != nil {
			return err
		}
	}
}

// startRetryDelay is the wait before retrying a failed start: 15s,
// doubling, at most 10 minutes.
func startRetryDelay(failures int) time.Duration {
	return min(15*time.Second<<min(failures-1, 6), 10*time.Minute)
}

func ptr[T any](v T) *T { return &v }

func (s *clusterSetup) apply(ctx context.Context, dir string) error {
	s.mu.Lock()
	req := s.request
	s.request = nil
	s.state = "applying"
	s.mu.Unlock()
	if req == nil {
		return nil
	}
	// Preserve the local settings and encryption key together. SQLite stays intact.
	files := []string{"config.json", config.CredentialKeyFile}
	originals := make(map[string][]byte)
	var err error
	for _, name := range files {
		var b []byte
		b, err = os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			break
		}
		originals[name] = b
	}
	if err == nil {
		actx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		if s.execute != nil {
			err = s.execute(actx, dir, *req)
		} else if req.Mode == "create" {
			err = clusterconfig.Init(actx, clusterconfig.InitOptions{DataDir: dir, DatabaseURL: req.URL()})
		} else {
			_, err = clusterconfig.Join(actx, clusterconfig.JoinOptions{DataDir: dir, DatabaseURL: req.DatabaseURL, Token: req.Token, Force: req.Replace})
		}
		cancel()
	}
	// Never leave a failed join's credential key paired with the old settings.
	if err != nil {
		for name, b := range originals {
			path := filepath.Join(dir, name)
			if e := os.WriteFile(path+".restore", b, 0600); e != nil {
				return errors.New("cluster setup failed and local settings could not be restored; recover the automatic backup")
			}
			if e := os.Rename(path+".restore", path); e != nil {
				return errors.New("cluster setup failed and local settings could not be restored; recover the automatic backup")
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.state, s.message = "failed", clusterconfig.SetupError(err)
	} else {
		s.state, s.message = "complete", "Cluster setup completed."
	}
	return nil
}

func (a *App) clusterSetupHandler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int, message string) {
		reply(w, code, map[string]any{"error": map[string]string{"code": "cluster_setup", "message": message}})
	}
	s := a.opts.clusterSetup
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if s == nil {
			reply(w, 200, map[string]any{"available": false, "state": "idle"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(w, 200, map[string]any{"available": os.Getenv(config.DatabaseURLEnv) == "", "state": s.state, "message": s.message})
	})
	for _, path := range []string{"/setup/check", "/setup/apply"} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if s == nil || os.Getenv(config.DatabaseURLEnv) != "" {
				fail(w, 409, "This deployment fixes the database through its environment; change that deployment setting before using guided setup.")
				return
			}
			if a.clustered() {
				fail(w, 409, "This server already belongs to a cluster.")
				return
			}
			var req clusterconfig.SetupRequest
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
				fail(w, 400, "Invalid setup request.")
				return
			}
			if err := req.Validate(); err != nil {
				fail(w, 400, err.Error())
				return
			}
			if req.Mode == "create" && !a.cfg.Get().SetupComplete {
				fail(w, 403, "Finish this server's setup before creating a cluster.")
				return
			}
			if req.Mode == "join" && a.cfg.Get().SetupComplete && !req.Replace {
				fail(w, 409, "Confirm that this server will adopt the cluster's identity and settings. Its local data will be backed up.")
				return
			}
			s.mu.Lock()
			if s.state == "checking" || s.state == "restarting" || s.state == "applying" {
				s.mu.Unlock()
				fail(w, 409, "Cluster setup is already running.")
				return
			}
			s.state = "checking"
			s.mu.Unlock()
			queued := false
			defer func() {
				if !queued {
					s.mu.Lock()
					s.state = "idle"
					s.mu.Unlock()
				}
			}()
			cctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			check := s.check
			if check == nil {
				check = clusterconfig.CheckSetup
			}
			err := check(cctx, req)
			cancel()
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			if r.URL.Path == "/setup/check" {
				reply(w, 200, map[string]any{"ok": true})
				return
			}
			// Block admissions before checking active sessions or taking the snapshot.
			(&clusterUpdates{a: a}).SetDraining(true)
			defer func() {
				if !queued {
					(&clusterUpdates{a: a}).SetDraining(false)
				}
			}()
			if a.activeSessionCount() > 0 {
				fail(w, 409, "Stop active streams on this server before changing its cluster membership.")
				return
			}
			// The supported snapshot includes settings, the credential key and local data.
			bctx, bcancel := context.WithTimeout(r.Context(), 5*time.Minute)
			defer bcancel()
			if _, err := a.backups.Create(bctx); err != nil {
				fail(w, 500, "Could not create the automatic backup. Check the backup directory and try again.")
				return
			}
			s.mu.Lock()
			s.request, s.state, s.message = &req, "restarting", "Ether is applying cluster settings and will reconnect automatically."
			s.mu.Unlock()
			queued = true
			reply(w, http.StatusAccepted, map[string]any{"state": "restarting"})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			s.stop <- struct{}{}
		})
	}
	return mux
}
