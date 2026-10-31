package app

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/startup"
)

// frontDoor is the server's port for the whole life of Serve: it is bound
// as soon as the data directory is locked, before the database opens, and
// answers through a startup.Gate, so the console can show what a starting
// server is doing (waiting for PostgreSQL, migrating, restoring). Each App
// opens the gate when it is ready and closes it to drain before stopping;
// a cluster transition or a retried start keeps the same listener.
type frontDoor struct {
	tracker *startup.Tracker
	gate    *startup.Gate
	log     *slog.Logger

	ln   net.Listener
	srv  *http.Server
	errc chan error
	// port is the port bound; moved is set when the default port was busy
	// and a fallback was taken (the App saves it once settings are open).
	port  int
	moved bool
	// tls serves TLS connections once a ready App has its certificates.
	tls atomic.Pointer[func() *tls.Config]
	// retry starts a failed start again at once (a recovery restore).
	retry chan struct{}
	// recovery serves the failed-start recovery API; replaced per start.
	recovery atomic.Pointer[http.Handler]
	// proxies are the trusted reverse-proxy CIDRs for the console's network
	// policy: from the settings on disk until a server is ready, then from
	// its live settings.
	proxies atomic.Pointer[func() []string]
}

func newFrontDoor(tracker *startup.Tracker, log *slog.Logger) *frontDoor {
	f := &frontDoor{
		tracker: tracker,
		log:     log,
		errc:    make(chan error, 1),
		retry:   make(chan struct{}, 1),
	}
	f.gate = &startup.Gate{
		Tracker: f.tracker,
		Shell:   admin.Shell(),
		Console: func(h http.Handler) http.Handler { return lanOnly(h, f.trustedProxies) },
		Recovery: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h := f.recovery.Load(); h != nil {
				(*h).ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}),
	}
	return f
}

func (f *frontDoor) bound() bool { return f.ln != nil }

func (f *frontDoor) trustedProxies() []string {
	if fn := f.proxies.Load(); fn != nil {
		return (*fn)()
	}
	return nil
}

// setProxies sets where the trusted proxy CIDRs come from.
func (f *frontDoor) setProxies(fn func() []string) { f.proxies.Store(&fn) }

// bind listens on the configured port (or a fallback, see bindPort) and
// starts serving the gate.
func (f *frontDoor) bind(server config.Server, envPort bool) error {
	ln, moved, err := bindPort(server, envPort)
	if err != nil {
		return err
	}
	cidrs := server.TrustedProxyCIDRs
	f.setProxies(func() []string { return cidrs })
	f.serve(ln)
	f.moved = moved
	return nil
}

// serve starts answering on ln.
func (f *frontDoor) serve(ln net.Listener) {
	f.ln = ln
	f.port = ln.Addr().(*net.TCPAddr).Port
	dl := newDualListener(ln, func() *tls.Config {
		if fn := f.tls.Load(); fn != nil {
			return (*fn)()
		}
		return nil // no certificates until a server is ready
	})
	f.srv = &http.Server{
		Handler:           f.gate,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
	}
	go func() { f.errc <- f.srv.Serve(dl) }()
}

// setTLS serves TLS with the ready server's certificates.
func (f *frontDoor) setTLS(fn func() *tls.Config) { f.tls.Store(&fn) }

// close stops listening. In-flight requests were drained by Gate.Close.
func (f *frontDoor) close() {
	if f.srv != nil {
		_ = f.srv.Close()
	}
}

// retryNow wakes a failed start's wait.
func (f *frontDoor) retryNow() {
	select {
	case f.retry <- struct{}{}:
	default:
	}
}

// fallbackPorts are tried, in order, when the default port 32400 is already
// taken, normally by a Plex Media Server on the same machine.
var fallbackPorts = []int{32450, 32451, 32452, 32453, 32454, 32455, 32456, 32457, 32458, 32459}

// bindPort binds the configured port. When that is the default 32400 and it
// is in use, Ether moves to the first free fallback port (moved), so it
// runs next to Plex out of the box; plex.tv publishes the port, so Plex
// apps follow. A port the owner chose (or ETHER_PORT, envPort) is never
// moved.
func bindPort(server config.Server, envPort bool) (net.Listener, bool, error) {
	addr := net.JoinHostPort(server.BindAddress, strconv.Itoa(server.Port))
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, false, nil
	}
	if server.Port != config.DefaultPort || !addrInUse(err) || envPort {
		return nil, false, fmt.Errorf("listen %s: %w", addr, err)
	}
	for _, port := range fallbackPorts {
		alt, aerr := net.Listen("tcp", net.JoinHostPort(server.BindAddress, strconv.Itoa(port)))
		if aerr == nil {
			return alt, true, nil
		}
	}
	return nil, false, fmt.Errorf("listen %s: %w (and ports %d-%d are also in use)", addr, err, fallbackPorts[0], fallbackPorts[len(fallbackPorts)-1])
}

// listen binds the configured port for an App run without Serve's front
// door (tests), saving a fallback port like Serve does.
func (a *App) listen(cfg config.Config) (net.Listener, error) {
	ln, moved, err := bindPort(cfg.Server, os.Getenv("ETHER_PORT") != "")
	if err != nil {
		return nil, err
	}
	if moved {
		if err := a.saveMovedPort(ln.Addr().(*net.TCPAddr).Port); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

// saveMovedPort records the fallback port the server moved to.
func (a *App) saveMovedPort(port int) error {
	if err := a.cfg.Update(func(c *config.Config) error { c.Server.Port = port; return nil }); err != nil {
		return fmt.Errorf("save port %d: %w", port, err)
	}
	a.log.Warn("port 32400 is in use (usually by Plex Media Server); Ether moved to another port and saved it",
		"port", port, "ui", fmt.Sprintf("http://localhost:%d/ether/", port))
	return nil
}

// envPort is the ETHER_PORT override, or 0.
func envPort() (int, error) {
	v := strings.TrimSpace(os.Getenv("ETHER_PORT"))
	if v == "" {
		return 0, nil
	}
	port, err := strconv.Atoi(v)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("ETHER_PORT=%q: want a port between 1 and 65535", v)
	}
	return port, nil
}
