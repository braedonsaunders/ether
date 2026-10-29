package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type probeFixture struct{ probe, response string }

func (p probeFixture) RespondDiscovery(_ context.Context, r frontend.DiscoveryRequest) ([]byte, bool) {
	if !strings.EqualFold(string(r.Data), p.probe) {
		return nil, false
	}
	return []byte(p.response + " " + r.Endpoint), true
}

type blockingProbe struct {
	entered chan struct{}
	release chan struct{}
}

func (p blockingProbe) RespondDiscovery(context.Context, frontend.DiscoveryRequest) ([]byte, bool) {
	close(p.entered)
	<-p.release
	return []byte("late"), true
}

func TestDisabledSurfaceCannotReplyToInflightProbe(t *testing.T) {
	d := newSharedDiscovery(context.Background())
	d.listen = func() (net.PacketConn, error) { return net.ListenPacket("udp4", "127.0.0.1:0") }
	p := blockingProbe{entered: make(chan struct{}), release: make(chan struct{})}
	endpoint := func(context.Context, frontend.Protocol, netip.AddrPort) (string, error) { return "http://example", nil }
	if err := d.add("jellyfin", discoveryEntry{p, endpoint}); err != nil {
		t.Fatal(err)
	}
	defer d.close()
	d.mu.Lock()
	addr := d.conn.LocalAddr().String()
	d.mu.Unlock()
	client, err := net.Dial("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("probe")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("probe not delivered")
	}
	d.remove("jellyfin")
	close(p.release)
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 32)
	if n, err := client.Read(buf); err == nil {
		t.Fatalf("disabled surface replied: %q", buf[:n])
	}
}

type discoveryFrontend struct {
	id    frontend.Protocol
	probe string
}

func (f discoveryFrontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: f.id, Name: string(f.id)}
}
func (f discoveryFrontend) Mount(context.Context, frontend.Host) (frontend.Instance, error) {
	return discoveryInstance{f}, nil
}

type discoveryInstance struct{ discoveryFrontend }

func (i discoveryInstance) Handler() http.Handler { return http.NotFoundHandler() }
func (i discoveryInstance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/" + string(i.id)}}
}
func (discoveryInstance) Discovery() []frontend.Announcer        { return nil }
func (discoveryInstance) Status(context.Context) frontend.Status { return frontend.Status{} }
func (discoveryInstance) Close(context.Context) error            { return nil }
func (i discoveryInstance) RespondDiscovery(_ context.Context, req frontend.DiscoveryRequest) ([]byte, bool) {
	if string(req.Data) != i.probe {
		return nil, false
	}
	return []byte(i.id), true
}

func TestDiscoveryFollowsFrontendMountLifecycle(t *testing.T) {
	registry := frontend.NewRegistry(func(frontend.Protocol) frontend.Host { return frontend.Host{} })
	for _, f := range []discoveryFrontend{{"jellyfin", "jelly"}, {"emby", "emby"}} {
		if err := registry.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	m := newFrontendListeners(registry)
	m.discovery = newSharedDiscovery(context.Background())
	m.discovery.listen = func() (net.PacketConn, error) { return net.ListenPacket("udp4", "127.0.0.1:0") }
	m.discoveryEndpoint = func(context.Context, frontend.Protocol, netip.AddrPort) (string, error) { return "http://example", nil }
	defer m.close(context.Background())
	for _, id := range []frontend.Protocol{"jellyfin", "emby"} {
		if err := m.start(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	m.discovery.mu.Lock()
	addr := m.discovery.conn.LocalAddr().String()
	m.discovery.mu.Unlock()
	client, err := net.Dial("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Write([]byte("jelly")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 32)
	n, err := client.Read(buf)
	if err != nil || string(buf[:n]) != "jellyfin" {
		t.Fatalf("jelly response %q %v", buf[:n], err)
	}
	if err = m.stop(context.Background(), "jellyfin"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write([]byte("jelly")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err = client.Read(buf); err == nil {
		t.Fatal("disabled surface announced")
	}
	if _, err = client.Write([]byte("emby")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	n, err = client.Read(buf)
	if err != nil || string(buf[:n]) != "emby" {
		t.Fatalf("emby response %q %v", buf[:n], err)
	}
	if err = m.stop(context.Background(), "emby"); err != nil {
		t.Fatal(err)
	}
	m.discovery.mu.Lock()
	conn := m.discovery.conn
	m.discovery.mu.Unlock()
	if conn != nil {
		t.Fatal("last unmount left socket open")
	}
}

func TestSharedDiscoveryMultiplexesAndUnbinds(t *testing.T) {
	d := newSharedDiscovery(context.Background())
	var binds atomic.Int32
	d.listen = func() (net.PacketConn, error) { binds.Add(1); return net.ListenPacket("udp4", "127.0.0.1:0") }
	endpoint := func(_ context.Context, id frontend.Protocol, _ netip.AddrPort) (string, error) {
		return "http://example/" + string(id), nil
	}
	if err := d.add("jellyfin", discoveryEntry{probeFixture{"who jelly", "jelly"}, endpoint}); err != nil {
		t.Fatal(err)
	}
	if err := d.add("emby", discoveryEntry{probeFixture{"who emby", "emby"}, endpoint}); err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if binds.Load() != 1 {
		t.Fatal("multiple sockets", binds.Load())
	}
	d.mu.Lock()
	addr := d.conn.LocalAddr().String()
	d.mu.Unlock()
	client, err := net.Dial("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	probe := func(message string, want string) {
		t.Helper()
		if _, err := client.Write([]byte(message)); err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 1024)
		n, err := client.Read(buf)
		if want == "" {
			if err == nil {
				t.Fatalf("unexpected reply %q", buf[:n])
			}
			return
		}
		if err != nil || string(buf[:n]) != want {
			t.Fatalf("probe %q got %q %v", message, buf[:n], err)
		}
	}
	probe("WHO JELLY", "jelly http://example/jellyfin")
	probe("who emby", "emby http://example/emby")
	d.remove("jellyfin")
	probe("who jelly", "")
	probe("who emby", "emby http://example/emby")
	d.remove("emby")
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn != nil {
		t.Fatal("socket stayed bound")
	}
	if err := d.add("jellyfin", discoveryEntry{probeFixture{"who jelly", "jelly"}, endpoint}); err != nil || binds.Load() != 2 {
		t.Fatalf("rebind: %v %d", err, binds.Load())
	}
}

func TestSharedDiscoveryBindFailureRetries(t *testing.T) {
	d := newSharedDiscovery(context.Background())
	defer d.close()
	var attempts int
	d.listen = func() (net.PacketConn, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("busy")
		}
		return net.ListenPacket("udp4", "127.0.0.1:0")
	}
	endpoint := func(context.Context, frontend.Protocol, netip.AddrPort) (string, error) { return "http://example", nil }
	if err := d.add("jellyfin", discoveryEntry{probeFixture{}, endpoint}); err == nil || d.error() == nil {
		t.Fatal("bind failure hidden")
	}
	if err := d.add("emby", discoveryEntry{probeFixture{}, endpoint}); err != nil || d.error() != nil {
		t.Fatalf("retry failed: %v", err)
	}
}
