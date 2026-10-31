package startup

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// StatusPath is the public, read-only startup status endpoint.
const StatusPath = "/ether/api/startup-status"

// RecoveryPrefix is where a failed start's recovery API is mounted.
const RecoveryPrefix = "/ether/api/startup/recovery/"

// retryAfter is the Retry-After (seconds) of a 503 while starting.
const retryAfter = "5"

// Gate is the server's root handler for its whole life. Until Open it
// serves only the startup status, the console shell and (while a start has
// failed) the recovery API; everything else gets 503. After Open it passes
// requests to the server, and Close stops doing so and waits for requests
// in flight, so a restart drains before the server shuts down.
type Gate struct {
	// Tracker supplies the status.
	Tracker *Tracker
	// Shell serves the console's static files (GET /ether/...).
	Shell http.Handler
	// Recovery, when set, serves RecoveryPrefix while the start has failed.
	Recovery http.Handler
	// Console wraps every /ether/ handler the gate serves itself (the
	// owner console's network policy). Nil leaves them as they are.
	Console func(http.Handler) http.Handler

	// Serving costs a ready server two atomic adds and a load per request.
	app    atomic.Pointer[http.Handler]
	active atomic.Int64
	idle   atomic.Pointer[idleSignal]
}

// idleSignal is closed once when the last in-flight request finishes.
type idleSignal struct {
	ch   chan struct{}
	once sync.Once
}

// Open routes every request to h: the server is ready.
func (g *Gate) Open(h http.Handler) {
	g.app.Store(&h)
	g.Tracker.Set(PhaseReady)
}

// Close stops routing requests to the server (new ones get 503 and the
// stopping status) and waits until the requests in flight have finished or
// ctx ends.
func (g *Gate) Close(ctx context.Context) {
	g.Tracker.Set(PhaseStopping)
	sig := &idleSignal{ch: make(chan struct{})}
	g.idle.Store(sig)
	g.app.Store(nil)
	if g.active.Load() == 0 {
		return
	}
	select {
	case <-sig.ch:
	case <-ctx.Done():
	}
}

// IsOpen reports whether the server is handling requests.
func (g *Gate) IsOpen() bool { return g.app.Load() != nil }

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == StatusPath {
		g.console(http.HandlerFunc(g.serveStatus)).ServeHTTP(w, r)
		return
	}
	// Counted before the handler is read, so Close never misses a request
	// that is about to reach the server.
	g.active.Add(1)
	defer g.done()
	if app := g.app.Load(); app != nil {
		(*app).ServeHTTP(w, r)
		return
	}
	g.serveStarting(w, r)
}

func (g *Gate) done() {
	if g.active.Add(-1) == 0 {
		if sig := g.idle.Load(); sig != nil {
			sig.once.Do(func() { close(sig.ch) })
		}
	}
}

func (g *Gate) console(h http.Handler) http.Handler {
	if g.Console != nil {
		return g.Console(h)
	}
	return h
}

func (g *Gate) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(g.Tracker.Snapshot())
}

// serveStarting answers a request the server is not ready for.
func (g *Gate) serveStarting(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/ether":
		http.Redirect(w, r, "/ether/", http.StatusFound)
		return
	case strings.HasPrefix(path, RecoveryPrefix):
		if g.Recovery != nil && g.Tracker.Phase() == PhaseFailed {
			g.console(g.Recovery).ServeHTTP(w, r)
			return
		}
	case strings.HasPrefix(path, "/ether/") && !strings.HasPrefix(path, "/ether/api/") &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) && g.Shell != nil:
		g.console(g.Shell).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Retry-After", retryAfter)
	w.Header().Set("Cache-Control", "no-store")
	st := g.Tracker.Snapshot()
	if strings.HasPrefix(path, "/ether/") {
		g.console(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
				"code": "starting", "message": st.Message, "phase": string(st.Phase),
			}})
		})).ServeHTTP(w, r)
		return
	}
	http.Error(w, "Ether is not ready yet: "+st.Message, http.StatusServiceUnavailable)
}
