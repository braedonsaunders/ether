package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

type discoveryEntry struct {
	responder frontend.DiscoveryResponder
	endpoint  func(context.Context, frontend.Protocol, netip.AddrPort) (string, error)
}

// sharedDiscovery multiplexes the MediaBrowser discovery port for mounted
// surfaces. Each callback runs without the lifecycle lock held.
type sharedDiscovery struct {
	mu      sync.Mutex
	ctx     context.Context
	listen  func() (net.PacketConn, error)
	conn    net.PacketConn
	entries map[frontend.Protocol]*discoveryEntry
	lastErr error
}

func newSharedDiscovery(ctx context.Context) *sharedDiscovery {
	return &sharedDiscovery{ctx: ctx, entries: make(map[frontend.Protocol]*discoveryEntry), listen: func() (net.PacketConn, error) { return net.ListenPacket("udp4", ":7359") }}
}

func (s *sharedDiscovery) add(id frontend.Protocol, entry discoveryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || entry.responder == nil || entry.endpoint == nil {
		return errors.New("invalid discovery responder")
	}
	s.entries[id] = &entry
	if s.conn != nil {
		return nil
	}
	conn, err := s.listen()
	if err != nil {
		s.lastErr = err
		return err
	}
	s.lastErr = nil
	s.conn = conn
	go s.serve(conn)
	return nil
}

func (s *sharedDiscovery) error() error { s.mu.Lock(); defer s.mu.Unlock(); return s.lastErr }

func (s *sharedDiscovery) remove(id frontend.Protocol) {
	s.mu.Lock()
	delete(s.entries, id)
	var conn net.PacketConn
	if len(s.entries) == 0 {
		conn, s.conn = s.conn, nil
	}
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *sharedDiscovery) close() {
	s.mu.Lock()
	s.entries = make(map[frontend.Protocol]*discoveryEntry)
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *sharedDiscovery) serve(conn net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, remote, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		peer, err := netip.ParseAddrPort(remote.String())
		if err != nil || !identity.LAN(peer.Addr()) {
			continue
		}
		s.mu.Lock()
		entries := make(map[frontend.Protocol]*discoveryEntry, len(s.entries))
		for id, entry := range s.entries {
			entries[id] = entry
		}
		s.mu.Unlock()
		for id, entry := range entries {
			endpoint, err := entry.endpoint(s.ctx, id, peer)
			if err != nil || endpoint == "" {
				continue
			}
			reply, ok := entry.responder.RespondDiscovery(s.ctx, frontend.DiscoveryRequest{Data: append([]byte(nil), buf[:n]...), Peer: peer, Endpoint: endpoint})
			if ok && len(reply) > 0 && len(reply) <= 65507 {
				s.mu.Lock()
				if s.conn == conn && s.entries[id] == entry {
					_, _ = conn.WriteTo(reply, remote)
				}
				s.mu.Unlock()
			}
		}
	}
}

func (r *frontendRuntime) discoveryEndpoint(ctx context.Context, id frontend.Protocol, peer netip.AddrPort) (string, error) {
	if !identity.LAN(peer.Addr()) {
		return "", errors.New("discovery requires LAN")
	}
	r.control.listeners.mu.Lock()
	run := r.control.listeners.running[id]
	var local net.Addr
	if run != nil && len(run.listeners) > 0 {
		local = run.listeners[0].Addr()
	}
	r.control.listeners.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		return "", err
	}
	req.RemoteAddr = peer.String()
	if local != nil {
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
	}
	return r.surfaceURL(ctx, id, req)
}
