package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// frontendListeners owns listeners and announcers for independently mounted
// protocol instances. A failed bind is retained as status, not a process error.
type frontendListeners struct {
	mu       sync.Mutex
	opMu     sync.Mutex
	registry *frontend.Registry
	running  map[frontend.Protocol]*runningFrontend
	failures map[frontend.Protocol]string
}
type runningFrontend struct {
	servers    []*http.Server
	listeners  []net.Listener
	announcers []frontend.Announcer
	warnings   []string
}

func newFrontendListeners(r *frontend.Registry) *frontendListeners {
	return &frontendListeners{registry: r, running: make(map[frontend.Protocol]*runningFrontend), failures: make(map[frontend.Protocol]string)}
}
func (m *frontendListeners) start(ctx context.Context, id frontend.Protocol) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	_, exists := m.running[id]
	m.mu.Unlock()
	if exists {
		return frontend.ErrAlreadyMounted
	}
	inst, err := m.registry.Mount(ctx, id)
	if err != nil {
		m.mu.Lock()
		m.failures[id] = err.Error()
		m.mu.Unlock()
		return err
	}
	run := &runningFrontend{}
	rollback := func() {
		for _, ln := range run.listeners {
			_ = ln.Close()
		}
		for _, a := range run.announcers {
			_ = a.Close()
		}
		_ = m.registry.Unmount(context.Background(), id)
	}
	for _, spec := range inst.Listeners() {
		if spec.TLS {
			err = errors.New("frontend TLS listener requires a certificate provider")
			break
		}
		var ln net.Listener
		ln, err = listenFrontend(id, spec.Address)
		if err != nil {
			break
		}
		run.listeners = append(run.listeners, ln)
		_, requestedPort, requestedErr := net.SplitHostPort(spec.Address)
		_, actualPort, actualErr := net.SplitHostPort(ln.Addr().String())
		if requestedErr == nil && actualErr == nil && requestedPort != "0" && requestedPort != actualPort {
			run.warnings = append(run.warnings, fmt.Sprintf("Port %s was unavailable; listening on %s", requestedPort, actualPort))
		}
		handler := inst.Handler()
		if handler == nil {
			err = errors.New("frontend returned nil handler")
			break
		}
		if spec.PathPrefix != "" && spec.PathPrefix != "/" {
			prefix := strings.TrimSuffix(spec.PathPrefix, "/")
			if !strings.HasPrefix(prefix, "/") {
				err = errors.New("invalid path prefix")
				break
			}
			next := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
					http.NotFound(w, r)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
		run.servers = append(run.servers, &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second})
	}
	if err == nil {
		for _, a := range inst.Discovery() {
			if a == nil {
				err = errors.New("nil discovery announcer")
				break
			}
			if err = a.Start(ctx); err != nil {
				_ = a.Close()
				break
			}
			run.announcers = append(run.announcers, a)
		}
	}
	if err != nil {
		rollback()
		m.mu.Lock()
		m.failures[id] = err.Error()
		m.mu.Unlock()
		return fmt.Errorf("start %s: %w", id, err)
	}
	m.mu.Lock()
	delete(m.failures, id)
	m.running[id] = run
	m.mu.Unlock()
	for i, srv := range run.servers {
		ln := run.listeners[i]
		go func() {
			err := srv.Serve(ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.mu.Lock()
				m.failures[id] = err.Error()
				m.mu.Unlock()
			}
		}()
	}
	return nil
}
func (m *frontendListeners) stop(ctx context.Context, id frontend.Protocol) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	run, ok := m.running[id]
	if !ok {
		delete(m.failures, id)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	var errs []error
	for _, srv := range run.servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, err)
			_ = srv.Close()
		}
	}
	for _, a := range run.announcers {
		if err := a.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.registry.Unmount(ctx, id); err != nil {
		errs = append(errs, err)
	} else {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}
func (m *frontendListeners) status(ctx context.Context, id frontend.Protocol) frontend.Status {
	m.mu.Lock()
	run := m.running[id]
	failure := m.failures[id]
	m.mu.Unlock()
	var st frontend.Status
	if inst, ok := m.registry.Instance(id); ok {
		st = inst.Status(ctx)
	}
	if run != nil {
		for _, ln := range run.listeners {
			st.Addresses = append(st.Addresses, ln.Addr().String())
		}
		st.Warnings = append(st.Warnings, run.warnings...)
	}
	if failure != "" {
		st.Error = failure
	}
	return st
}
func (m *frontendListeners) close(ctx context.Context) error {
	m.mu.Lock()
	ids := make([]frontend.Protocol, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := m.stop(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// listenFrontend falls back only when a protocol's default port is busy; a
// custom port remains explicit. Plex retains its historical 32450-32459 range.
func listenFrontend(id frontend.Protocol, address string) (net.Listener, error) {
	return listenFrontendWith(id, address, net.Listen)
}

func listenFrontendWith(id frontend.Protocol, address string, listen func(string, string) (net.Listener, error)) (net.Listener, error) {
	ln, err := listen("tcp", address)
	if err == nil || !errors.Is(err, syscall.EADDRINUSE) {
		return ln, err
	}
	host, portText, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, err
	}
	port, parseErr := strconv.Atoi(portText)
	defaultPort, ok := frontend.DefaultPort(id)
	if parseErr != nil || !ok || port != defaultPort {
		return nil, err
	}
	first := defaultPort + 1
	if id == "plex" {
		first = 32450
	}
	for candidate := first; candidate < first+10; candidate++ {
		reserved := false
		for _, other := range []frontend.Protocol{"plex", "jellyfin", "emby", "silo"} {
			if p, ok := frontend.DefaultPort(other); ok && candidate == p {
				reserved = true
				break
			}
		}
		if reserved {
			continue
		}
		ln, nextErr := listen("tcp", net.JoinHostPort(host, strconv.Itoa(candidate)))
		if nextErr == nil {
			return ln, nil
		}
		if !errors.Is(nextErr, syscall.EADDRINUSE) {
			return nil, nextErr
		}
	}
	return nil, err
}
