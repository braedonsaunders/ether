package app

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
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
