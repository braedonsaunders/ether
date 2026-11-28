package dlna

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// Only a search the server would answer costs a viewer lookup; the NOTIFY
// chatter every other device multicasts must not.
func TestSSDPServeSkipsViewerLookupForNonSearchDatagrams(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skip("no loopback UDP:", err)
	}
	defer listener.Close()
	var lookups atomic.Int32
	s := &ssdp{id: "0123456789abcdef0123456789abcdef", host: frontend.Host{
		LANViewer: func(context.Context, *http.Request) (*frontend.Viewer, error) {
			lookups.Add(1)
			return &frontend.Viewer{ID: 1}, nil
		},
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://127.0.0.1:1/dlna", nil },
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.serve(ctx, ipv4.NewPacketConn(listener)) }()
	defer func() { cancel(); listener.Close(); <-done }()

	client, err := net.DialUDP("udp4", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nNTS: ssdp:alive\r\n\r\n"))
	_, _ = client.Write([]byte("M-SEARCH * HTTP/1.1\r\nMAN: \"ssdp:discover\"\r\nST: upnp:rootdevice\r\nMX: 1\r\n\r\n"))
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 2048)); err != nil {
		t.Fatalf("no search response: %v", err)
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("viewer looked up %d times for one search and one notify, want 1", got)
	}
}
