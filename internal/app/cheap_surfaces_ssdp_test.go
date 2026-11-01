package app

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

func TestCheapSurfacesMountedSSDP(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.frontends.control.Toggle(ctx, "plex", false); err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "discovery-viewer", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.frontends.control.PutSettings(ctx, "dlna", frontend.Settings{"viewerId": float64(id), "discovery": true}); err != nil {
		t.Fatal(err)
	}
	if err = a.frontends.control.Toggle(ctx, "dlna", true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	a.frontends.start(ctx, server.Listener.Addr())
	inst, ok := a.frontends.registry.Instance("dlna")
	if !ok {
		t.Fatal("DLNA not mounted")
	}
	for _, warning := range inst.Status(ctx).Warnings {
		if strings.Contains(warning, "using discovery") {
			interfaces, e := net.Interfaces()
			if e != nil {
				t.Fatal(e)
			}
			for _, iface := range interfaces {
				if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagMulticast != 0 && iface.Flags&net.FlagLoopback == 0 {
					t.Fatal(warning)
				}
			}
			t.Skip(warning)
		}
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1900}
	search := []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n")
	probe := func(want bool) {
		t.Helper()
		if _, err = conn.WriteToUDP(search, target); err != nil {
			t.Fatal(err)
		}
		timeout := time.Second
		if !want {
			timeout = 200 * time.Millisecond
		}
		if err = conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 4096)
		n, _, e := conn.ReadFromUDP(data)
		if !want {
			if e == nil {
				t.Fatalf("disabled DLNA answered SSDP: %s", data[:n])
			}
			return
		}
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(string(data[:n]), "LOCATION: "+server.URL+"/dlna/description.xml\r\n") {
			t.Fatalf("SSDP origin: %s", data[:n])
		}
	}
	probe(true)
	if err = a.frontends.control.Toggle(ctx, "dlna", false); err != nil {
		t.Fatal(err)
	}
	probe(false)
	if err = a.frontends.control.Toggle(ctx, "dlna", true); err != nil {
		t.Fatal(err)
	}
	probe(true)
}
