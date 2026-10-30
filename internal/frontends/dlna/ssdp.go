package dlna

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"golang.org/x/net/ipv4"
)

type ssdp struct {
	host      frontend.Host
	id        string
	mu        sync.Mutex
	conn      *net.UDPConn
	packet    *ipv4.PacketConn
	joined    map[int]net.Interface
	warning   string
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	published map[string]advertisement
	addresses func() ([]net.Addr, error)
	emit      func(advertisement, bool)
}

func (s *ssdp) Start(ctx context.Context) error {
	if s.host.Config != nil {
		enabled, _ := s.host.Config()["discovery"].(bool)
		if !enabled {
			return nil
		}
	}
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	conn, packet, joined, err := listenSSDP(group)
	if err != nil {
		s.mu.Lock()
		s.warning = "Discovery unavailable: " + err.Error()
		s.mu.Unlock()
		return nil
	}
	run, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.conn, s.cancel = conn, cancel
	s.packet, s.joined = packet, joined
	s.warning = ""
	s.published = make(map[string]advertisement)
	s.wg.Add(2)
	s.mu.Unlock()
	go func() { defer s.wg.Done(); s.serve(run, packet) }()
	go func() { defer s.wg.Done(); s.announceLoop(run) }()
	return nil
}
func (s *ssdp) Close() error {
	s.mu.Lock()
	conn, cancel := s.conn, s.cancel
	s.conn, s.cancel = nil, nil
	s.packet, s.joined = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	s.wg.Wait()
	return nil
}
func (s *ssdp) warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warning == "" {
		return nil
	}
	return []string{s.warning}
}
func (s *ssdp) serve(ctx context.Context, conn *ipv4.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, incoming, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
		if err != nil {
			continue
		}
		req.RemoteAddr = peer.String()
		viewer, err := s.host.LANViewer(ctx, req)
		if err != nil || viewer == nil {
			continue
		}
		targets := searchTargets(string(buf[:n]), s.uuid())
		if len(targets) == 0 {
			continue
		}
		if s.host.SurfaceURL == nil {
			continue
		}
		base, err := s.host.SurfaceURL(ctx, req)
		if err != nil {
			continue
		}
		for _, st := range targets {
			usn := "uuid:" + s.uuid()
			if st != usn {
				usn += "::" + st
			}
			response := fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nEXT:\r\nLOCATION: %s/description.xml\r\nSERVER: Ether/1.0 UPnP/1.0\r\nST: %s\r\nUSN: %s\r\n\r\n", strings.TrimRight(base, "/"), st, usn)
			var reply *ipv4.ControlMessage
			if incoming != nil {
				reply = &ipv4.ControlMessage{IfIndex: incoming.IfIndex}
			}
			_, _ = conn.WriteTo([]byte(response), reply, peer)
		}
	}
}

type advertisement struct {
	source   net.IP
	location string
}

// announceLoop publishes only while the host confirms an active LAN viewer.
// It withdraws previously advertised endpoints on disable or shutdown.
func (s *ssdp) announceLoop(ctx context.Context) {
	defer s.withdrawAll()
	s.refreshAdvertisements(ctx)
	// The listener registry may finish mounting just after Start returns.
	// Retry promptly so a valid endpoint is advertised without a long wait.
	retry := time.NewTimer(time.Second)
	defer retry.Stop()
	select {
	case <-ctx.Done():
		return
	case <-retry.C:
		s.refreshAdvertisements(ctx)
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshAdvertisements(ctx)
		}
	}
}
func (s *ssdp) refreshAdvertisements(ctx context.Context) {
	s.refreshInterfaces()
	current := make(map[string]advertisement)
	if s.host.LANViewer != nil && s.host.SurfaceURL != nil {
		list := s.addresses
		if list == nil {
			list = net.InterfaceAddrs
		}
		addresses, _ := list()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil {
				continue
			}
			ip = ip.To4()
			if ip == nil || (!ip.IsPrivate() && !ip.IsLinkLocalUnicast()) {
				continue
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
			if err != nil {
				continue
			}
			req.RemoteAddr = net.JoinHostPort(ip.String(), "1900")
			viewer, err := s.host.LANViewer(ctx, req)
			if err != nil || viewer == nil {
				continue
			}
			base, err := s.host.SurfaceURL(ctx, req)
			if err != nil || base == "" {
				continue
			}
			item := advertisement{source: append(net.IP(nil), ip...), location: strings.TrimRight(base, "/") + "/description.xml"}
			current[ip.String()+"|"+item.location] = item
		}
	}
	s.mu.Lock()
	previous := s.published
	s.published = current
	s.mu.Unlock()
	for key, item := range previous {
		if _, ok := current[key]; !ok {
			s.emitAdvertisement(item, false)
		}
	}
	for _, item := range current {
		s.emitAdvertisement(item, true)
	}
}
func (s *ssdp) withdrawAll() {
	s.mu.Lock()
	previous := s.published
	s.published = nil
	s.mu.Unlock()
	for _, item := range previous {
		s.emitAdvertisement(item, false)
	}
}
func (s *ssdp) emitAdvertisement(item advertisement, alive bool) {
	if s.emit != nil {
		s.emit(item, alive)
		return
	}
	s.sendAdvertisement(item, alive)
}
func (s *ssdp) sendAdvertisement(item advertisement, alive bool) {
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: item.source}, group)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	for _, packet := range notificationPackets(s.uuid(), item.location, alive) {
		_, _ = conn.Write([]byte(packet))
	}
}

// notificationPackets emits one UPnP advertisement per device/service target.
func notificationPackets(uuid, location string, alive bool) []string {
	targets := []string{"upnp:rootdevice", "uuid:" + uuid, "urn:schemas-upnp-org:device:MediaServer:1", "urn:schemas-upnp-org:service:ContentDirectory:1", "urn:schemas-upnp-org:service:ConnectionManager:1"}
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		usn := "uuid:" + uuid
		if target != usn {
			usn += "::" + target
		}
		packet := "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nNT: " + target + "\r\n"
		if alive {
			packet += "NTS: ssdp:alive\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: " + location + "\r\nSERVER: Ether/1.0 UPnP/1.0\r\n"
		} else {
			packet += "NTS: ssdp:byebye\r\n"
		}
		out = append(out, packet+"USN: "+usn+"\r\n\r\n")
	}
	return out
}

func (s *ssdp) uuid() string {
	return fmt.Sprintf("%s-%s-%s-%s-%s", s.id[:8], s.id[8:12], s.id[12:16], s.id[16:20], s.id[20:])
}

func searchTargets(packet, uuid string) []string {
	lines := strings.Split(strings.ReplaceAll(packet, "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.EqualFold(strings.TrimSpace(lines[0]), "M-SEARCH * HTTP/1.1") {
		return nil
	}
	var target, man string
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "st":
			target = strings.TrimSpace(value)
		case "man":
			man = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	if !strings.EqualFold(man, "ssdp:discover") {
		return nil
	}
	allowed := []string{"upnp:rootdevice", "uuid:" + uuid, "urn:schemas-upnp-org:device:MediaServer:1", "urn:schemas-upnp-org:service:ContentDirectory:1", "urn:schemas-upnp-org:service:ConnectionManager:1"}
	if strings.EqualFold(target, "ssdp:all") {
		return allowed
	}
	for _, value := range allowed {
		if strings.EqualFold(target, value) {
			return []string{value}
		}
	}
	return nil
}

var _ frontend.Announcer = (*ssdp)(nil)
