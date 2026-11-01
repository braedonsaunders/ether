package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
)

// GDM ("G'Day Mate") is Plex's LAN discovery protocol. Clients send
// "M-SEARCH * HTTP/1.1" to UDP 32414 on multicast group 239.0.0.250 (and by
// broadcast); every PMS answers unicast with an "HTTP/1.0 200 OK" block of
// headers describing itself. PMS listens on 32410, 32412, 32413 and 32414.
const (
	GDMMulticastGroup = "239.0.0.250"
	GDMServerPort     = 32414
	// GDMAnnouncePort receives HELLO/BYE announcements.
	GDMAnnouncePort = 32413
)

// DefaultGDMPorts are the UDP ports PMS answers GDM on.
var DefaultGDMPorts = []int{32410, 32412, 32413, 32414}

// GDMInfo is what the responder advertises.
type GDMInfo struct {
	Name              string
	MachineIdentifier string
	Port              int
	Version           string
	UpdatedAt         time.Time
	// ContentType defaults to "plex/media-server".
	ContentType string
}

func (i GDMInfo) headers() string {
	ct := i.ContentType
	if ct == "" {
		ct = "plex/media-server"
	}
	updated := i.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Content-Type: %s\r\n", ct)
	fmt.Fprintf(&b, "Resource-Identifier: %s\r\n", i.MachineIdentifier)
	fmt.Fprintf(&b, "Name: %s\r\n", sanitizeHeader(i.Name))
	fmt.Fprintf(&b, "Port: %d\r\n", i.Port)
	fmt.Fprintf(&b, "Updated-At: %d\r\n", updated.Unix())
	fmt.Fprintf(&b, "Version: %s\r\n", i.Version)
	return b.String()
}

func sanitizeHeader(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

// GDMResponse renders the reply to an M-SEARCH.
func GDMResponse(i GDMInfo) []byte {
	return []byte("HTTP/1.0 200 OK\r\n" + i.headers() + "\r\n")
}

// GDMHello renders an announcement ("HELLO * HTTP/1.0") or, with bye, a
// departure ("BYE * HTTP/1.0").
func GDMHello(i GDMInfo, bye bool) []byte {
	verb := "HELLO"
	if bye {
		verb = "BYE"
	}
	return []byte(verb + " * HTTP/1.0\r\n" + i.headers() + "\r\n")
}

// GDMOptions configures a GDMResponder.
type GDMOptions struct {
	// Info is called per request so name/version changes apply live.
	Info func() GDMInfo
	// Ports to listen on; DefaultGDMPorts when empty.
	Ports []int
	// ListenAddrs, when set, replaces Ports/multicast with plain unicast
	// sockets on these addresses (e.g. "127.0.0.1:0" in tests).
	ListenAddrs []string
	// Announce sends HELLO on start and every AnnounceInterval (default
	// 15m) to the multicast group, and BYE on stop. Off by default: clients
	// discover servers by searching, and current PMS builds rely on that.
	Announce         bool
	AnnounceInterval time.Duration
	Logger           *slog.Logger

	// rejoinInterval overrides gdmRejoinInterval (tests).
	rejoinInterval time.Duration
	// listen overrides socket creation for lifecycle tests; nil uses listenGDM.
	listen func(int) (*net.UDPConn, error)
}

// gdmRejoinInterval is how often each multicast socket is reopened, which
// re-joins the group. Membership is silently lost when interfaces change
// (DHCP renewals, Wi-Fi roaming, sleep/wake), and the standard library has
// no explicit re-join.
const gdmRejoinInterval = 10 * time.Minute

// Backoff for consecutive socket read errors, so a persistently failing
// socket (e.g. ICMP port-unreachable resets on Windows) cannot spin a core.
const (
	gdmErrBackoffMin = 10 * time.Millisecond
	gdmErrBackoffMax = 5 * time.Second
)

// nextGDMBackoff returns the delay after another consecutive read error.
func nextGDMBackoff(d time.Duration) time.Duration {
	if d < gdmErrBackoffMin {
		return gdmErrBackoffMin
	}
	return min(d*2, gdmErrBackoffMax)
}

// GDMResponder answers GDM discovery on the LAN.
type GDMResponder struct {
	o      GDMOptions
	mu     sync.Mutex
	conns  []*net.UDPConn
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewGDMResponder returns a responder; call Start.
func NewGDMResponder(o GDMOptions) *GDMResponder {
	if len(o.Ports) == 0 {
		o.Ports = DefaultGDMPorts
	}
	if o.AnnounceInterval <= 0 {
		o.AnnounceInterval = 15 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.rejoinInterval <= 0 {
		o.rejoinInterval = gdmRejoinInterval
	}
	if o.listen == nil {
		o.listen = listenGDM
	}
	return &GDMResponder{o: o}
}

// Start opens the sockets and serves until ctx is done or Stop is called.
// Ports that cannot be bound (e.g. a real PMS on the same host) are logged
// and skipped; Start fails only when no socket could be opened.
func (g *GDMResponder) Start(ctx context.Context) error {
	if g.o.Info == nil {
		return errors.New("gdm: Info is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		return errors.New("gdm: already started")
	}
	var conns []*net.UDPConn
	var errs []error
	if len(g.o.ListenAddrs) > 0 {
		for _, a := range g.o.ListenAddrs {
			ua, err := net.ResolveUDPAddr("udp4", a)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			c, err := net.ListenUDP("udp4", ua)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			conns = append(conns, c)
		}
	} else {
		for _, p := range g.o.Ports {
			c, err := g.o.listen(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("port %d: %w", p, err))
				continue
			}
			conns = append(conns, c)
		}
	}
	for _, err := range errs {
		g.o.Logger.Warn("gdm listen failed", "err", err)
	}
	if len(conns) == 0 {
		return fmt.Errorf("gdm: no sockets: %w", errors.Join(errs...))
	}
	g.conns = conns
	ctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	rejoin := len(g.o.ListenAddrs) == 0
	for i, c := range conns {
		g.wg.Add(1)
		go g.serve(ctx, i, c, rejoin)
	}
	if g.o.Announce && len(g.o.ListenAddrs) == 0 {
		g.wg.Add(1)
		go g.announce(ctx)
	}
	go func() {
		<-ctx.Done()
		g.mu.Lock()
		for _, c := range g.conns {
			_ = c.Close()
		}
		g.mu.Unlock()
	}()
	return nil
}

// listenGDM opens one GDM socket bound to 0.0.0.0:port and joins the GDM
// multicast group on every active LAN interface so the socket receives
// multicast (239.0.0.250), broadcast (255.255.255.255 / subnet) and unicast
// M-SEARCH queries on Linux, macOS and Windows.
func listenGDM(port int) (*net.UDPConn, error) {
	// Reuse lets rejoin open the replacement while the current socket is still
	// answering. The old socket is closed only after the new one is installed.
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(setGDMReuseAddr)
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	c := pc.(*net.UDPConn)
	joinGDMMulticast(c)
	return c, nil
}

func joinGDMMulticast(c *net.UDPConn) {
	group := &net.UDPAddr{IP: net.ParseIP(GDMMulticastGroup)}
	pc := ipv4.NewPacketConn(c)
	ifs, err := net.Interfaces()
	if err != nil {
		_ = pc.JoinGroup(nil, group)
		return
	}
	joined := false
	for i := range ifs {
		ifi := &ifs[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback == 0 && IsVirtualInterface(ifi.Name) {
			continue
		}
		if err := pc.JoinGroup(ifi, group); err == nil {
			joined = true
		}
	}
	if !joined {
		_ = pc.JoinGroup(nil, group)
	}
}

// rejoin replaces socket i with a freshly opened one on the same port,
// re-joining the multicast group. It returns the socket to keep serving on
// and false when the responder is stopping.
func (g *GDMResponder) rejoin(ctx context.Context, i int, old *net.UDPConn) (*net.UDPConn, bool) {
	g.mu.Lock()
	if ctx.Err() != nil || i >= len(g.conns) || g.conns[i] != old {
		g.mu.Unlock()
		return old, false
	}
	g.mu.Unlock()
	port := old.LocalAddr().(*net.UDPAddr).Port
	c, err := g.o.listen(port)
	if err != nil {
		g.o.Logger.Debug("gdm rejoin failed; keeping the current socket", "port", port, "err", err)
		return old, ctx.Err() == nil
	}
	g.mu.Lock()
	if ctx.Err() != nil || i >= len(g.conns) || g.conns[i] != old {
		g.mu.Unlock()
		_ = c.Close()
		return old, false
	}
	g.conns[i] = c
	g.mu.Unlock()
	_ = old.Close()
	return c, true
}

// Addrs returns the bound local addresses.
func (g *GDMResponder) Addrs() []net.Addr {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]net.Addr, len(g.conns))
	for i, c := range g.conns {
		out[i] = c.LocalAddr()
	}
	return out
}

// Stop closes the sockets and waits for the serve loops.
func (g *GDMResponder) Stop() {
	g.mu.Lock()
	cancel := g.cancel
	g.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	g.mu.Lock()
	for _, c := range g.conns {
		_ = c.Close()
	}
	g.mu.Unlock()
	g.wg.Wait()
	g.mu.Lock()
	g.cancel = nil
	g.conns = nil
	g.mu.Unlock()
}

// serve answers M-SEARCH on socket i. With rejoin, the socket is reopened
// every rejoinInterval to refresh its multicast membership.
func (g *GDMResponder) serve(ctx context.Context, i int, c *net.UDPConn, rejoin bool) {
	defer g.wg.Done()
	buf := make([]byte, 2048)
	var backoff time.Duration
	nextRejoin := time.Now().Add(g.o.rejoinInterval)
	for {
		if rejoin {
			if !time.Now().Before(nextRejoin) {
				var ok bool
				if c, ok = g.rejoin(ctx, i, c); !ok {
					return
				}
				nextRejoin = time.Now().Add(g.o.rejoinInterval)
			}
			_ = c.SetReadDeadline(nextRejoin)
		}
		n, src, err := c.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			var ne net.Error
			if rejoin && errors.As(err, &ne) && ne.Timeout() {
				continue // time to rejoin
			}
			backoff = nextGDMBackoff(backoff)
			g.o.Logger.Debug("gdm read", "err", err, "retry_in", backoff)
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		backoff = 0
		if !isMSearch(buf[:n]) {
			continue
		}
		if _, err := c.WriteToUDP(GDMResponse(g.o.Info()), src); err != nil {
			g.o.Logger.Debug("gdm reply failed", "to", src, "err", err)
		}
	}
}

func isMSearch(b []byte) bool {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("M-SEARCH * HTTP/1."))
}

func (g *GDMResponder) announce(ctx context.Context) {
	defer g.wg.Done()
	dst := &net.UDPAddr{IP: net.ParseIP(GDMMulticastGroup), Port: GDMAnnouncePort}
	send := func(bye bool) {
		c, err := net.DialUDP("udp4", nil, dst)
		if err != nil {
			g.o.Logger.Debug("gdm announce", "err", err)
			return
		}
		defer c.Close()
		_, _ = c.Write(GDMHello(g.o.Info(), bye))
	}
	send(false)
	t := time.NewTicker(g.o.AnnounceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			send(true)
			return
		case <-t.C:
			send(false)
		}
	}
}

// GDMReply is one server's answer to a GDM search.
type GDMReply struct {
	From              *net.UDPAddr
	ContentType       string
	Name              string
	MachineIdentifier string
	Port              int
	Version           string
	UpdatedAt         time.Time
	Headers           http.Header
}

// ParseGDMReply parses an "HTTP/1.0 200 OK" GDM reply.
func ParseGDMReply(b []byte) (*GDMReply, error) {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(b)))
	status, err := r.ReadLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(status, "HTTP/1.") || !strings.Contains(status, " 200") {
		return nil, fmt.Errorf("gdm: unexpected status line %q", status)
	}
	h, err := r.ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return nil, err
	}
	rep := &GDMReply{
		Headers:           http.Header(h),
		ContentType:       h.Get("Content-Type"),
		Name:              h.Get("Name"),
		MachineIdentifier: h.Get("Resource-Identifier"),
		Version:           h.Get("Version"),
	}
	rep.Port, _ = strconv.Atoi(h.Get("Port"))
	if s, err := strconv.ParseInt(h.Get("Updated-At"), 10, 64); err == nil {
		rep.UpdatedAt = time.Unix(s, 0)
	}
	return rep, nil
}

// DiscoverGDM sends one M-SEARCH to target ("239.0.0.250:32414" when empty)
// and collects replies until ctx is done or wait elapses.
func DiscoverGDM(ctx context.Context, target string, wait time.Duration) ([]GDMReply, error) {
	if target == "" {
		target = net.JoinHostPort(GDMMulticastGroup, strconv.Itoa(GDMServerPort))
	}
	if wait <= 0 {
		wait = 2 * time.Second
	}
	raddr, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if _, err := conn.WriteToUDP([]byte("M-SEARCH * HTTP/1.1\r\n\r\n"), raddr); err != nil {
		return nil, err
	}
	var out []GDMReply
	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return out, nil
			}
			return out, err
		}
		rep, err := ParseGDMReply(buf[:n])
		if err != nil {
			continue
		}
		rep.From = src
		out = append(out, *rep)
	}
}
