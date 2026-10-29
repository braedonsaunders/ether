package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// frontendListeners owns listeners and announcers for independently mounted
// protocol instances. A failed bind is retained as status, not a process error.
type frontendListeners struct {
	mu       sync.Mutex
	registry *frontend.Registry
	running  map[frontend.Protocol]*runningFrontend
	failures map[frontend.Protocol]string
}
type runningFrontend struct {
	servers    []*http.Server
	listeners  []net.Listener
	announcers []frontend.Announcer
}

func newFrontendListeners(r *frontend.Registry) *frontendListeners {
	return &frontendListeners{registry: r, running: make(map[frontend.Protocol]*runningFrontend), failures: make(map[frontend.Protocol]string)}
}
func (m *frontendListeners) start(ctx context.Context, id frontend.Protocol) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.running[id]; ok {
		return frontend.ErrAlreadyMounted
	}
	inst, err := m.registry.Mount(ctx, id)
	if err != nil {
		m.failures[id] = err.Error()
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
		ln, err = net.Listen("tcp", spec.Address)
		if err != nil {
			break
		}
		run.listeners = append(run.listeners, ln)
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
			if err = a.Start(ctx); err != nil {
				break
			}
			run.announcers = append(run.announcers, a)
		}
	}
	if err != nil {
		rollback()
		m.failures[id] = err.Error()
		return fmt.Errorf("start %s: %w", id, err)
	}
	delete(m.failures, id)
	m.running[id] = run
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
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.running[id]
	if !ok {
		delete(m.failures, id)
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
		delete(m.running, id)
	}
	return errors.Join(errs...)
}
func (m *frontendListeners) status(ctx context.Context, id frontend.Protocol) frontend.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st frontend.Status
	if inst, ok := m.registry.Instance(id); ok {
		st = inst.Status(ctx)
	}
	if run := m.running[id]; run != nil {
		for _, ln := range run.listeners {
			st.Addresses = append(st.Addresses, ln.Addr().String())
		}
	}
	if failure := m.failures[id]; failure != "" {
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
