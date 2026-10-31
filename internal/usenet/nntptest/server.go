// Package nntptest provides an in-memory NNTP server for tests across Ether:
// articles keyed by message-id (yEnc-encoded on the way in), optional
// AUTHINFO, TLS, missing articles, latency, a connection limit and fault
// injection, plus Post, which uploads files as multipart yEnc segments and
// returns a matching NZB document.
//
// Typical use:
//
//	srv := nntptest.Start(t, nntptest.Options{Username: "u", Password: "p"})
//	nzb, _ := srv.PostFiles(map[string][]byte{"movie.mkv": data}, 700_000)
//	f := usenet.New([]config.UsenetProvider{srv.Provider("primary", 4)},
//		usenet.Options{TLSConfig: srv.ClientTLSConfig()})
package nntptest

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/yenc"
)

// Options configure a Server. All of them except TLS can also be changed
// while running.
type Options struct {
	// Username and Password, when Username is set, make AUTHINFO mandatory.
	Username string
	Password string
	// TLS serves NNTPS with a freshly generated self-signed certificate;
	// clients should use ClientTLSConfig.
	TLS bool
	// MaxConns rejects connections beyond this many with "502 too many
	// connections" (see RefuseCode/RefuseText). 0 = unlimited.
	MaxConns int
	// RefuseCode and RefuseText replace the greeting sent to connections
	// over MaxConns (default 502 "too many connections"), e.g. to state the
	// limit: "connection limit (8) reached".
	RefuseCode int
	RefuseText string
	// TeardownDelay keeps a closed client connection counted against
	// MaxConns for this long, like a server that has not finished tearing
	// the old socket down when the client redials.
	TeardownDelay time.Duration
	// Latency delays every reply.
	Latency time.Duration
	// LineLength for yEnc encoding (0 = 128).
	LineLength int
	// Addr to listen on (default "127.0.0.1:0").
	Addr   string
	Logger *slog.Logger
}

// Fault overrides the server's normal reply to one command.
type Fault struct {
	Code  int    // reply "Code Text" instead of the normal reply (0 = normal)
	Text  string // reply text for Code
	Drop  bool   // close the connection without replying
	Delay time.Duration
	// Truncate sends the 222 line and roughly half of the body, then drops
	// the connection (BODY/ARTICLE only).
	Truncate bool
	// BodyDelay stalls mid-body (BODY/ARTICLE only): the status line and
	// the first body line go out and are flushed, then nothing is sent
	// for BodyDelay — or until the client disconnects, whichever comes
	// first — and the rest of the body follows. This models an upstream
	// that stops sending without closing the connection, unlike Delay,
	// which stalls before the status line.
	BodyDelay time.Duration
}

// FaultFunc decides a fault per command. cmd is the upper-case verb
// ("BODY", "STAT", "AUTHINFO", ...); arg is the message-id without angle
// brackets for article commands, else the raw argument string. It is called
// for every command, so it can count, match ids or fail the first N.
type FaultFunc func(cmd, arg string) Fault

// Stats are counters for assertions.
type Stats struct {
	Accepted      int // connections accepted (including rejected ones)
	Rejected      int // turned away by MaxConns
	Active        int // currently open
	MaxActive     int // high-water mark of concurrently open connections
	AuthFailures  int
	Commands      map[string]int // by verb
	BodyRequests  map[string]int // BODY/ARTICLE by message-id
	BytesSent     int64
	ArticlesSent  int
	ArticleMisses int
}

// Server is a fake NNTP server.
type Server struct {
	ln        net.Listener
	tlsConfig *tls.Config
	clientTLS *tls.Config
	log       *slog.Logger
	seq       atomic.Int64

	mu       sync.Mutex
	opts     Options
	articles map[string][]byte // id -> dot-stuffed, CRLF body without terminator
	missing  map[string]bool
	fault    FaultFunc
	conns    map[net.Conn]struct{}
	stats    Stats
	closed   bool
	stop     chan struct{}

	wg sync.WaitGroup
}

// Start is NewServer for tests: it fails tb on error and closes the server
// in tb.Cleanup.
func Start(tb testing.TB, opts Options) *Server {
	tb.Helper()
	s, err := NewServer(opts)
	if err != nil {
		tb.Fatalf("nntptest: %v", err)
	}
	tb.Cleanup(s.Close)
	return s
}

// NewServer starts a server listening on a loopback port.
func NewServer(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		opts:     opts,
		stop:     make(chan struct{}),
		log:      opts.Logger,
		articles: make(map[string][]byte),
		missing:  make(map[string]bool),
		conns:    make(map[net.Conn]struct{}),
		stats:    Stats{Commands: map[string]int{}, BodyRequests: map[string]int{}},
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, err
	}
	if opts.TLS {
		srvCfg, cliCfg, err := selfSigned()
		if err != nil {
			ln.Close()
			return nil, err
		}
		s.tlsConfig, s.clientTLS = srvCfg, cliCfg
		ln = tls.NewListener(ln, srvCfg)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Addr is "host:port".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Host is the listening IP.
func (s *Server) Host() string {
	h, _, _ := net.SplitHostPort(s.Addr())
	return h
}

// Port is the listening port.
func (s *Server) Port() int {
	_, p, _ := net.SplitHostPort(s.Addr())
	n, _ := strconv.Atoi(p)
	return n
}

// ClientTLSConfig trusts the server's certificate (nil without TLS).
func (s *Server) ClientTLSConfig() *tls.Config {
	if s.clientTLS == nil {
		return nil
	}
	return s.clientTLS.Clone()
}

// Provider returns an enabled provider config pointing at this server.
func (s *Server) Provider(id string, connections int) config.UsenetProvider {
	s.mu.Lock()
	defer s.mu.Unlock()
	return config.UsenetProvider{
		ID: id, Name: id, Host: s.Host(), Port: s.Port(), TLS: s.opts.TLS,
		Username: s.opts.Username, Password: s.opts.Password,
		Connections: connections, Enabled: true,
	}
}

// SetLatency changes the per-reply delay.
func (s *Server) SetLatency(d time.Duration) {
	s.mu.Lock()
	s.opts.Latency = d
	s.mu.Unlock()
}

// SetMaxConns changes the connection limit (0 = unlimited).
func (s *Server) SetMaxConns(n int) {
	s.mu.Lock()
	s.opts.MaxConns = n
	s.mu.Unlock()
}

// SetRefusal changes the greeting sent to connections over MaxConns
// (code 0 restores 502 "too many connections").
func (s *Server) SetRefusal(code int, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.RefuseCode, s.opts.RefuseText = code, text
}

// SetTeardownDelay changes Options.TeardownDelay.
func (s *Server) SetTeardownDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.TeardownDelay = d
}

// SetCredentials changes the required login (empty username = no auth).
func (s *Server) SetCredentials(user, pass string) {
	s.mu.Lock()
	s.opts.Username, s.opts.Password = user, pass
	s.mu.Unlock()
}

// SetFault installs (or with nil removes) a fault injector.
func (s *Server) SetFault(f FaultFunc) {
	s.mu.Lock()
	s.fault = f
	s.mu.Unlock()
}

// Stats returns a copy of the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Commands = make(map[string]int, len(s.stats.Commands))
	for k, v := range s.stats.Commands {
		st.Commands[k] = v
	}
	st.BodyRequests = make(map[string]int, len(s.stats.BodyRequests))
	for k, v := range s.stats.BodyRequests {
		st.BodyRequests[k] = v
	}
	return st
}

// ResetStats zeroes the counters (Active is kept).
func (s *Server) ResetStats() {
	s.mu.Lock()
	s.stats = Stats{Active: s.stats.Active, MaxActive: s.stats.Active, Commands: map[string]int{}, BodyRequests: map[string]int{}}
	s.mu.Unlock()
}

// BodyRequests is how often BODY/ARTICLE asked for id.
func (s *Server) BodyRequests(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats.BodyRequests[trimID(id)]
}

func trimID(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
}

// AddArticle stores a raw article body. Line endings are normalised to
// CRLF and lines are dot-stuffed on the wire.
func (s *Server) AddArticle(id string, body []byte) {
	var b bytes.Buffer
	b.Grow(len(body) + len(body)/64 + 8)
	for len(body) > 0 {
		i := bytes.IndexByte(body, '\n')
		var line []byte
		if i < 0 {
			line, body = body, nil
		} else {
			line, body = body[:i], body[i+1:]
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > 0 && line[0] == '.' {
			b.WriteByte('.')
		}
		b.Write(line)
		b.WriteString("\r\n")
	}
	s.mu.Lock()
	s.articles[trimID(id)] = b.Bytes()
	s.mu.Unlock()
}

// AddYenc yEnc-encodes data with o and stores it under id. It returns the
// encoded size.
func (s *Server) AddYenc(id string, data []byte, o yenc.EncodeOptions) (int, error) {
	if o.LineLength == 0 {
		s.mu.Lock()
		o.LineLength = s.opts.LineLength
		s.mu.Unlock()
	}
	if o.Name == "" {
		o.Name = "data.bin"
	}
	enc, err := yenc.EncodeBytes(data, o)
	if err != nil {
		return 0, err
	}
	s.AddArticle(id, enc)
	return len(enc), nil
}

// Remove deletes articles.
func (s *Server) Remove(ids ...string) {
	s.mu.Lock()
	for _, id := range ids {
		delete(s.articles, trimID(id))
	}
	s.mu.Unlock()
}

// SetMissing makes the server answer 430 for ids (while keeping them
// stored), or with missing=false serve them again.
func (s *Server) SetMissing(missing bool, ids ...string) {
	s.mu.Lock()
	for _, id := range ids {
		if missing {
			s.missing[trimID(id)] = true
		} else {
			delete(s.missing, trimID(id))
		}
	}
	s.mu.Unlock()
}

// Has reports whether id is stored and not marked missing.
func (s *Server) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.articles[trimID(id)]
	return ok && !s.missing[trimID(id)]
}

// Corrupt changes one encoded data character of a stored yEnc article so
// its CRC no longer matches.
func (s *Server) Corrupt(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.articles[trimID(id)]
	if !ok {
		return fmt.Errorf("nntptest: no article %q", id)
	}
	body = bytes.Clone(body)
	for off := 0; off < len(body); {
		end := bytes.Index(body[off:], []byte("\r\n"))
		if end < 0 {
			break
		}
		line := body[off : off+end]
		if !bytes.HasPrefix(line, []byte("=y")) {
			for i := 2; i < len(line); i++ {
				if c := line[i]; c != '=' && line[i-1] != '=' {
					if c == 'a' {
						line[i] = 'b'
					} else {
						line[i] = 'a'
					}
					s.articles[trimID(id)] = body
					return nil
				}
			}
		}
		off += end + 2
	}
	return fmt.Errorf("nntptest: article %q has no data line to corrupt", id)
}

// DropConnections closes every open client connection, as a server restart
// or network blip would.
func (s *Server) DropConnections() {
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

// Close stops the server and closes all connections.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stop)
	s.ln.Close()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.stats.Accepted++
		if s.opts.MaxConns > 0 && s.stats.Active >= s.opts.MaxConns {
			s.stats.Rejected++
			code, text := s.opts.RefuseCode, s.opts.RefuseText
			if code == 0 {
				code = 502
			}
			if text == "" {
				text = "too many connections"
			}
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				_, _ = fmt.Fprintf(c, "%d %s\r\n", code, text)
				c.Close()
			}()
			continue
		}
		s.stats.Active++
		s.stats.MaxActive = max(s.stats.MaxActive, s.stats.Active)
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handle(c)
	}
}

type session struct {
	user   string
	authed bool
	group  string
}

func (s *Server) handle(c net.Conn) {
	defer s.wg.Done()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		teardown := s.opts.TeardownDelay
		if teardown <= 0 || s.closed {
			s.stats.Active--
			s.mu.Unlock()
			return
		}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			t := time.NewTimer(teardown)
			defer t.Stop()
			select {
			case <-t.C:
			case <-s.stop:
			}
			s.mu.Lock()
			s.stats.Active--
			s.mu.Unlock()
		}()
	}()
	r := bufio.NewReader(c)
	w := bufio.NewWriterSize(c, 64<<10)
	var sess session
	s.reply(w, 200, "nntptest ready (posting ok)")
	if w.Flush() != nil {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		arg = strings.TrimSpace(arg)
		if !s.command(w, r, c, &sess, verb, arg) {
			_ = w.Flush()
			return
		}
		if w.Flush() != nil {
			return
		}
	}
}

func (s *Server) reply(w *bufio.Writer, code int, text string) {
	fmt.Fprintf(w, "%d %s\r\n", code, text)
}

// command handles one command; false closes the connection.
func (s *Server) command(w *bufio.Writer, r *bufio.Reader, c net.Conn, sess *session, verb, arg string) bool {
	faultArg := arg
	switch verb {
	case "BODY", "ARTICLE", "HEAD", "STAT":
		faultArg = trimID(arg)
	}
	s.mu.Lock()
	s.stats.Commands[verb]++
	latency := s.opts.Latency
	fault := s.fault
	user, pass := s.opts.Username, s.opts.Password
	s.mu.Unlock()

	var f Fault
	if fault != nil {
		f = fault(verb, faultArg)
	}
	if d := latency + f.Delay; d > 0 {
		time.Sleep(d)
	}
	if f.Drop {
		return false
	}
	if f.Code != 0 && !f.Truncate {
		s.reply(w, f.Code, f.Text)
		return f.Code != 400 && f.Code != 205
	}

	needAuth := func() bool {
		if user != "" && !sess.authed {
			s.reply(w, 480, "authentication required")
			return true
		}
		return false
	}

	switch verb {
	case "QUIT":
		s.reply(w, 205, "bye")
		return false
	case "CAPABILITIES":
		w.WriteString("101 capability list follows\r\nVERSION 2\r\nREADER\r\nAUTHINFO USER\r\n.\r\n")
	case "MODE":
		s.reply(w, 200, "reader mode, posting ok")
	case "DATE":
		s.reply(w, 111, time.Now().UTC().Format("20060102150405"))
	case "AUTHINFO":
		sub, val, _ := strings.Cut(arg, " ")
		switch strings.ToUpper(sub) {
		case "USER":
			sess.user, sess.authed = val, false
			if user == "" {
				sess.authed = true
				s.reply(w, 281, "no password needed")
			} else {
				s.reply(w, 381, "password required")
			}
		case "PASS":
			switch {
			case sess.user == "":
				s.reply(w, 482, "authentication commands issued out of sequence")
			case user == "" || (sess.user == user && val == pass):
				sess.authed = true
				s.reply(w, 281, "authentication accepted")
			default:
				s.mu.Lock()
				s.stats.AuthFailures++
				s.mu.Unlock()
				s.reply(w, 481, "authentication failed")
			}
		default:
			s.reply(w, 501, "syntax error")
		}
	case "GROUP":
		if needAuth() {
			return true
		}
		if arg == "" {
			s.reply(w, 501, "syntax error")
			return true
		}
		sess.group = arg
		s.reply(w, 211, "1000 1 1000 "+arg)
	case "STAT", "BODY", "ARTICLE", "HEAD":
		if needAuth() {
			return true
		}
		if !strings.HasPrefix(arg, "<") || !strings.HasSuffix(arg, ">") {
			s.reply(w, 501, "message-id required")
			return true
		}
		id := trimID(arg)
		s.mu.Lock()
		body, ok := s.articles[id]
		if s.missing[id] {
			ok = false
		}
		if verb == "BODY" || verb == "ARTICLE" {
			s.stats.BodyRequests[id]++
			if ok {
				s.stats.ArticlesSent++
				s.stats.BytesSent += int64(len(body))
			}
		}
		if !ok {
			s.stats.ArticleMisses++
		}
		s.mu.Unlock()
		if !ok {
			s.reply(w, 430, "no such article")
			return true
		}
		head := fmt.Sprintf("Message-ID: <%s>\r\nSubject: %s\r\nFrom: nntptest <nntptest@ether.invalid>\r\nNewsgroups: alt.binaries.test\r\n", id, id)
		switch verb {
		case "STAT":
			s.reply(w, 223, "0 "+arg)
		case "HEAD":
			s.reply(w, 221, "0 "+arg)
			w.WriteString(head)
			w.WriteString(".\r\n")
		case "ARTICLE", "BODY":
			if verb == "ARTICLE" {
				s.reply(w, 220, "0 "+arg)
				w.WriteString(head)
				w.WriteString("\r\n")
			} else {
				s.reply(w, 222, "0 "+arg)
			}
			if f.BodyDelay > 0 && len(body) > 0 {
				// Flush the status line (and ARTICLE head) plus the
				// first body line, then go silent mid-body.
				cut := bytes.Index(body, []byte("\r\n"))
				if cut < 0 {
					cut = 0
				} else {
					cut += 2
				}
				w.Write(body[:cut])
				if w.Flush() != nil || !waitBodyDelay(c, r, s.stop, f.BodyDelay) {
					return false
				}
				w.Write(body[cut:])
				w.WriteString(".\r\n")
				break
			}
			if f.Truncate {
				w.Write(body[:len(body)/2])
				_ = w.Flush()
				return false
			}
			w.Write(body)
			w.WriteString(".\r\n")
		}
	default:
		s.reply(w, 500, "unknown command")
	}
	return true
}

// waitBodyDelay preserves pipelined commands while probing disconnects. Once
// input is buffered, waiting on the delay/shutdown signal avoids consuming it
// or spinning; the next command is still read by handle in its original order.
func waitBodyDelay(c net.Conn, r *bufio.Reader, stop <-chan struct{}, d time.Duration) bool {
	defer c.SetReadDeadline(time.Time{})
	end := time.Now().Add(d)
	for {
		remain := time.Until(end)
		if remain <= 0 {
			return true
		}
		select {
		case <-stop:
			return false
		default:
		}
		_ = c.SetReadDeadline(time.Now().Add(min(remain, 5*time.Millisecond)))
		_, err := r.Peek(1)
		if err == nil {
			timer := time.NewTimer(max(0, time.Until(end)))
			defer timer.Stop()
			select {
			case <-timer.C:
				return true
			case <-stop:
				return false
			}
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			return false
		}
	}
}

// selfSigned makes an ECDSA certificate for 127.0.0.1/::1/localhost.
func selfSigned() (server, client *tls.Config, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "nntptest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}},
		MinVersion:   tls.VersionTLS12,
	}
	client = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return server, client, nil
}
