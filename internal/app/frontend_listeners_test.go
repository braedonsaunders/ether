package app

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

type listenerFrontend struct {
	addr   string
	closed *int
}

func (f listenerFrontend) Describe() frontend.Descriptor { return frontend.Descriptor{ID: "test"} }
func (f listenerFrontend) Mount(context.Context, frontend.Host) (frontend.Instance, error) {
	return listenerInstance{f.addr, f.closed}, nil
}

type listenerInstance struct {
	addr   string
	closed *int
}

func (i listenerInstance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
}
func (i listenerInstance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Address: i.addr}}
}
func (i listenerInstance) Discovery() []frontend.Announcer { return nil }
func (i listenerInstance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "1"}
}
func (i listenerInstance) Close(context.Context) error { *i.closed++; return nil }
func TestFrontendListeners(t *testing.T) {
	ctx := context.Background()
	closed := 0
	reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
	if err := reg.Register(listenerFrontend{"127.0.0.1:0", &closed}); err != nil {
		t.Fatal(err)
	}
	m := newFrontendListeners(reg)
	if err := m.start(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	st := m.status(ctx, "test")
	if len(st.Addresses) != 1 || st.TargetVersion != "1" {
		t.Fatal(st)
	}
	res, err := http.Get("http://" + st.Addresses[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	if err := m.close(ctx); err != nil || closed != 1 {
		t.Fatal(err, closed)
	}
}
func TestFrontendListenerConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closed := 0
	reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
	if err := reg.Register(listenerFrontend{ln.Addr().String(), &closed}); err != nil {
		t.Fatal(err)
	}
	m := newFrontendListeners(reg)
	if err := m.start(context.Background(), "test"); err == nil || closed != 1 {
		t.Fatal(err, closed)
	}
	if st := m.status(context.Background(), "test"); st.Error == "" {
		t.Fatal(st)
	}
}

func TestFrontendDefaultFallback(t *testing.T) {
	var tried []string
	listen := func(_ string, addr string) (net.Listener, error) {
		tried = append(tried, addr)
		if len(tried) == 1 {
			return nil, syscall.EADDRINUSE
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	ln, err := listenFrontendWith("silo", "127.0.0.1:8090", listen)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if len(tried) != 2 || tried[1] != "127.0.0.1:8091" {
		t.Fatal(tried)
	}
	tried = nil
	ln, err = listenFrontendWith("plex", "127.0.0.1:32400", listen)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	if tried[1] != "127.0.0.1:32450" {
		t.Fatal(tried)
	}
	tried = nil
	_, err = listenFrontendWith("silo", "127.0.0.1:9090", listen)
	if !errors.Is(err, syscall.EADDRINUSE) || len(tried) != 1 {
		t.Fatal(tried, err)
	}
}

func TestFrontendConnectionURLs(t *testing.T) {
	addresses := []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.2"), Mask: net.CIDRMask(24, 32)}, &net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}
	got := connectionURLs("0.0.0.0:8090", "/silo", addresses)
	if len(got) != 1 || got[0] != "http://192.168.1.2:8090/silo" {
		t.Fatal(got)
	}
	if got := connectionURLs("127.0.0.1:8090", "", addresses); len(got) != 0 {
		t.Fatal(got)
	}
	if got := connectionURLs("[2001:db8::1]:8090", "", nil); len(got) != 1 || got[0] != "http://[2001:db8::1]:8090" {
		t.Fatal(got)
	}
}

type sharedFrontend struct {
	id     frontend.Protocol
	prefix string
}

func (f sharedFrontend) Describe() frontend.Descriptor { return frontend.Descriptor{ID: f.id} }
func (f sharedFrontend) Mount(context.Context, frontend.Host) (frontend.Instance, error) {
	return sharedInstance(f), nil
}

type sharedInstance sharedFrontend

func (i sharedInstance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, string(i.id)) })
}
func (i sharedInstance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: i.prefix}}
}
func (i sharedInstance) Discovery() []frontend.Announcer        { return nil }
func (i sharedInstance) Status(context.Context) frontend.Status { return frontend.Status{} }
func (i sharedInstance) Close(context.Context) error            { return nil }
func TestSharedFrontendPaths(t *testing.T) {
	ctx := context.Background()
	reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
	m := newFrontendListeners(reg)
	for _, f := range []sharedFrontend{{"files", "/files"}, {"nested", "/files/media"}, {"conflict", "/files"}, {"owner", "/ether/test"}} {
		if err := reg.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.start(ctx, "files"); err != nil {
		t.Fatal(err)
	}
	if err := m.start(ctx, "nested"); err != nil {
		t.Fatal(err)
	}
	if err := m.start(ctx, "conflict"); err == nil {
		t.Fatal("duplicate prefix accepted")
	}
	if err := m.start(ctx, "owner"); err == nil {
		t.Fatal("owner console path accepted")
	}
	for path, want := range map[string]string{"/files": "files", "/files/media/x": "nested"} {
		handler := m.sharedHandler(path)
		if handler == nil {
			t.Fatal(path)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Body.String() != want {
			t.Fatal(path, w.Body.String())
		}
	}
	if m.sharedHandler("/files-other") != nil || m.sharedHandler("/else") != nil {
		t.Fatal("matched outside prefix")
	}
	if err := m.close(ctx); err != nil {
		t.Fatal(err)
	}
}

type longFrontend struct{ entered chan struct{} }

func (f longFrontend) Describe() frontend.Descriptor { return frontend.Descriptor{ID: "long"} }
func (f longFrontend) Mount(context.Context, frontend.Host) (frontend.Instance, error) {
	return longInstance(f), nil
}

type longInstance longFrontend

func (i longInstance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(i.entered); <-r.Context().Done() })
}
func (i longInstance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Address: "127.0.0.1:0"}}
}
func (i longInstance) Discovery() []frontend.Announcer        { return nil }
func (i longInstance) Status(context.Context) frontend.Status { return frontend.Status{} }
func (i longInstance) Close(context.Context) error            { return nil }
func TestFrontendStopForcesLongRequestsClosed(t *testing.T) {
	ctx := context.Background()
	reg := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
	entered := make(chan struct{})
	if err := reg.Register(longFrontend{entered}); err != nil {
		t.Fatal(err)
	}
	m := newFrontendListeners(reg)
	if err := m.start(ctx, "long"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get("http://" + m.status(ctx, "long").Addresses[0])
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := m.stop(deadline, "long"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request survived forced close")
	}
	if _, ok := reg.Instance("long"); ok {
		t.Fatal("frontend still mounted")
	}
}
