package usenet

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/yenc"
)

// Default timeouts for a single connection.
const (
	DefaultDialTimeout     = 15 * time.Second
	DefaultCommandTimeout  = 60 * time.Second
	DefaultIdleReadTimeout = 5 * time.Second
	readBufferSize         = 64 << 10
)

// maxBodyLine bounds one line of a multi-line response. yEnc lines are a
// few hundred bytes; even a whole article on one line fits. A variable so
// tests can lower it.
var maxBodyLine = 16 << 20

// DialOptions describe how to reach and log in to one NNTP server.
type DialOptions struct {
	Host     string
	Port     int // 0 = 563 with TLS, 119 without
	TLS      bool
	Username string
	Password string
	// TLSConfig is the base TLS configuration (e.g. custom roots in tests).
	// ServerName defaults to Host.
	TLSConfig *tls.Config
	// Timeout bounds dial, TLS handshake, greeting and authentication.
	Timeout time.Duration
	// CommandTimeout bounds each command including reading its response.
	CommandTimeout time.Duration
	// IdleReadTimeout bounds a stalled read; progress renews it. Negative disables.
	IdleReadTimeout time.Duration
	// DialContext overrides the network dialer.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// BytesRead, when set, is incremented with every byte read off the wire.
	BytesRead *atomic.Int64
	// OnPhase receives durations for tcp, tls, greeting, and auth setup phases.
	OnPhase func(string, time.Duration)
	// OnStatus receives the raw server status line for trace diagnostics.
	OnStatus func(string)
	// OnCommand receives a protocol command before it is sent. AUTHINFO
	// credentials are redacted so trace logging cannot expose them.
	OnCommand func(string)
}

// Conn is one NNTP client connection. It is not safe for concurrent use;
// the pool hands each connection to one request at a time.
type Conn struct {
	nc             net.Conn
	r              *bufio.Reader
	w              *bufio.Writer
	commandTimeout time.Duration
	onPhase        func(string, time.Duration)
	onStatus       func(string)
	onCommand      func(string)

	// PostingAllowed reports a 200 (vs 201) greeting.
	PostingAllowed bool
	// Greeting is the server's welcome text.
	Greeting string

	recoveryProbe bool // pool-owned half-open breaker lease
	bodyStarted   bool // current pipeline reply accepted BODY before transport failed
	broken        bool
	inBody        bool
	group         string
	created       time.Time
	used          time.Time // last time a command completed
	// idleSince is when the pool last got the connection back from a
	// request; keepalive pings do not move it.
	idleSince time.Time
	uses      int           // requests served (maintained by the pool)
	lastRTT   time.Duration // command-to-status-line latency of the last command
	wire      *atomic.Int64 // bytes read off this socket, including TLS framing
}

// aLongTimeAgo is a deadline in the past, used to interrupt blocked I/O.
var aLongTimeAgo = time.Unix(1, 0)

// Dial connects, reads the greeting and authenticates if credentials are
// given. Errors wrap ErrAuthFailed, ErrServiceUnavailable, a *tls error or a
// network error so callers can tell bad credentials from an unreachable
// server.
func Dial(ctx context.Context, o DialOptions) (*Conn, error) {
	if o.Timeout <= 0 {
		o.Timeout = DefaultDialTimeout
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = DefaultCommandTimeout
	}
	port := o.Port
	if port == 0 {
		port = 119
		if o.TLS {
			port = 563
		}
	}
	addr := net.JoinHostPort(o.Host, strconv.Itoa(port))

	dctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	dial := o.DialContext
	if dial == nil {
		d := &net.Dialer{KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	phaseStart := time.Now()
	raw, err := dial(dctx, "tcp", addr)
	if o.OnPhase != nil {
		o.OnPhase("tcp", time.Since(phaseStart))
	}
	if err != nil {
		return nil, fmt.Errorf("nntp: dial %s: %w", addr, err)
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	wire := new(atomic.Int64)
	var nc net.Conn = &countingConn{Conn: raw, n: o.BytesRead, own: wire}
	if o.TLS {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if o.TLSConfig != nil {
			cfg = o.TLSConfig.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = o.Host
		}
		tc := tls.Client(nc, cfg)
		phaseStart = time.Now()
		err := tc.HandshakeContext(dctx)
		if o.OnPhase != nil {
			o.OnPhase("tls", time.Since(phaseStart))
		}
		if err != nil {
			raw.Close()
			return nil, fmt.Errorf("nntp: tls handshake with %s: %w", addr, err)
		}
		nc = tc
	}

	commandIdle := o.IdleReadTimeout
	if commandIdle == 0 {
		commandIdle = DefaultIdleReadTimeout
	}
	// The welcome shares the dial deadline. A short idle window would
	// expire that read when the suite is busy, before any command runs.
	var paced *idleReadConn
	if commandIdle > 0 {
		paced = &idleReadConn{Conn: nc, idle: o.Timeout}
		nc = paced
	}
	now := time.Now()
	c := &Conn{
		nc:             nc,
		r:              bufio.NewReaderSize(nc, readBufferSize),
		w:              bufio.NewWriterSize(nc, 4096),
		commandTimeout: o.CommandTimeout,
		onPhase:        o.OnPhase,
		onStatus:       o.OnStatus,
		onCommand:      o.OnCommand,
		created:        now,
		used:           now,
		wire:           wire,
	}
	if err := c.handshake(dctx, o.Username, o.Password); err != nil {
		c.Close()
		return nil, err
	}
	if paced != nil {
		paced.mu.Lock()
		paced.idle = commandIdle
		paced.mu.Unlock()
	}
	return c, nil
}

func (c *Conn) handshake(ctx context.Context, user, pass string) error {
	phaseStart := time.Now()
	// begin keeps the earlier context deadline. Dial set that from
	// DialOptions.Timeout, so the greeting does not use CommandTimeout.
	end, err := c.begin(ctx, time.Hour)
	if err != nil {
		return err
	}
	code, msg, err := c.readStatus()
	if err = end(err); err != nil {
		return fmt.Errorf("nntp: reading greeting: %w", err)
	}
	switch code {
	case 200:
		c.PostingAllowed = true
	case 201:
	default:
		c.broken = true
		return &ProtocolError{Cmd: "greeting", Code: code, Msg: msg}
	}
	c.Greeting = msg
	if c.onPhase != nil {
		c.onPhase("greeting", time.Since(phaseStart))
	}
	if user == "" {
		return nil
	}
	phaseStart = time.Now()
	err = c.auth(ctx, user, pass)
	if c.onPhase != nil {
		c.onPhase("auth", time.Since(phaseStart))
	}
	return err
}

func (c *Conn) auth(ctx context.Context, user, pass string) error {
	code, msg, err := c.cmdTimeout(ctx, "AUTHINFO", "AUTHINFO USER "+user, time.Hour)
	if err != nil {
		return err
	}
	switch code {
	case 281:
		return nil
	case 381:
	default:
		return &ProtocolError{Cmd: "AUTHINFO USER", Code: code, Msg: msg}
	}
	code, msg, err = c.cmdTimeout(ctx, "AUTHINFO", "AUTHINFO PASS "+pass, time.Hour)
	if err != nil {
		return err
	}
	if code != 281 {
		return &ProtocolError{Cmd: "AUTHINFO PASS", Code: code, Msg: msg}
	}
	return nil
}

// begin arms the connection deadline from timeout and ctx, and interrupts
// blocked I/O if ctx is cancelled. The returned end function must be called
// with the operation's error; it disarms the watcher, marks the connection
// broken on I/O failure and reports ctx.Err() in place of the I/O error the
// interruption caused.
func (c *Conn) begin(ctx context.Context, timeout time.Duration) (func(error) error, error) {
	if c.broken {
		return nil, ErrConnBroken
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dl := time.Now().Add(timeout)
	fromCtx := false
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl, fromCtx = d, true
	}
	if err := c.nc.SetDeadline(dl); err != nil {
		c.broken = true
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(aLongTimeAgo) })
	return func(err error) error {
		if !stop() {
			// The watcher fired: I/O may have been cut mid-response.
			c.broken = true
			return ctx.Err()
		}
		if err != nil {
			if isNetworkError(err) || errors.Is(err, bufio.ErrBufferFull) {
				c.broken = true
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if fromCtx && ctxErr(ctx) != nil && errors.Is(err, os.ErrDeadlineExceeded) {
					// The deadline taken from ctx expired the I/O a moment
					// before ctx itself noticed.
					return context.DeadlineExceeded
				}
			}
			return err
		}
		c.used = time.Now()
		return nil
	}, nil
}

// cmd sends one command line and reads the status reply.
func (c *Conn) cmd(ctx context.Context, verb, line string) (int, string, error) {
	return c.cmdTimeout(ctx, verb, line, c.commandTimeout)
}

// cmdTimeout is cmd with an explicit deadline ceiling. Handshake passes a
// long ceiling so AUTHINFO keeps the dial context's deadline.
func (c *Conn) cmdTimeout(ctx context.Context, verb, line string, timeout time.Duration) (int, string, error) {
	if c.inBody {
		return 0, "", fmt.Errorf("nntp: %s while a body is being read", verb)
	}
	end, err := c.begin(ctx, timeout)
	if err != nil {
		return 0, "", err
	}
	code, msg, err := c.roundTrip(line)
	if err = end(err); err != nil {
		return 0, "", fmt.Errorf("nntp: %s: %w", verb, err)
	}
	if code == 400 {
		// Server is closing the connection.
		c.broken = true
	}
	return code, msg, nil
}

func (c *Conn) roundTrip(line string) (int, string, error) {
	if c.onCommand != nil {
		if strings.HasPrefix(line, "AUTHINFO USER ") {
			c.onCommand("AUTHINFO USER [redacted]")
		} else if strings.HasPrefix(line, "AUTHINFO PASS ") {
			c.onCommand("AUTHINFO PASS [redacted]")
		} else {
			c.onCommand(line)
		}
	}
	if _, err := c.w.WriteString(line); err != nil {
		return 0, "", err
	}
	if _, err := c.w.WriteString("\r\n"); err != nil {
		return 0, "", err
	}
	start := time.Now()
	if err := c.w.Flush(); err != nil {
		return 0, "", err
	}
	code, msg, err := c.readStatus()
	c.lastRTT = time.Since(start)
	return code, msg, err
}

// readStatus reads a "NNN text" reply line.
func (c *Conn) readStatus() (int, string, error) {
	line, err := c.r.ReadSlice('\n')
	if err != nil {
		if err == bufio.ErrBufferFull {
			c.broken = true
			return 0, "", fmt.Errorf("nntp: status line too long")
		}
		return 0, "", err
	}
	if c.onStatus != nil {
		c.onStatus(strings.TrimRight(string(line), "\r\n"))
	}
	s := strings.TrimRight(string(line), "\r\n")
	if len(s) < 3 {
		c.broken = true
		return 0, "", fmt.Errorf("nntp: malformed reply %q", s)
	}
	code, err := strconv.Atoi(s[:3])
	if err != nil || code < 100 || code > 599 {
		c.broken = true
		return 0, "", fmt.Errorf("nntp: malformed reply %q", s)
	}
	return code, strings.TrimSpace(s[3:]), nil
}

// normalizeMessageID returns "<id>" and rejects ids that would break the
// command line.
func normalizeMessageID(id string) (string, error) {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	if id == "" || len(id) > 250 {
		return "", fmt.Errorf("%w: %q", ErrInvalidMessageID, id)
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c <= ' ' || c == 0x7f || c == '<' || c == '>' {
			return "", fmt.Errorf("%w: %q", ErrInvalidMessageID, id)
		}
	}
	return "<" + id + ">", nil
}

// Stat reports whether the server has the article (223) or not (430).
func (c *Conn) Stat(ctx context.Context, messageID string) (bool, error) {
	id, err := normalizeMessageID(messageID)
	if err != nil {
		return false, err
	}
	code, msg, err := c.cmd(ctx, "STAT", "STAT "+id)
	if err != nil {
		return false, err
	}
	switch code {
	case 223:
		return true, nil
	case 430, 423, 451:
		return false, nil
	}
	return false, &ProtocolError{Cmd: "STAT", Code: code, Msg: msg}
}

// StatMany pipelines STAT commands on this connection: all commands are
// flushed before any response is read. The connection is discarded by the
// pool if a pipeline is interrupted or its replies become desynchronised.
func (c *Conn) StatMany(ctx context.Context, messageIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(messageIDs))
	if len(messageIDs) == 0 {
		return out, nil
	}
	if c.inBody {
		return nil, errors.New("nntp: STAT while a body is being read")
	}
	ids := make([]string, len(messageIDs))
	for i, id := range messageIDs {
		normalized, err := normalizeMessageID(id)
		if err != nil {
			return nil, err
		}
		ids[i] = normalized
	}
	end, err := c.begin(ctx, c.commandTimeout)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	for _, id := range ids {
		if c.onCommand != nil {
			c.onCommand("STAT " + id)
		}
		if _, err = c.w.WriteString("STAT " + id + "\r\n"); err != nil {
			break
		}
	}
	if err == nil {
		err = c.w.Flush()
	}
	if err != nil {
		c.broken = true
		return nil, fmt.Errorf("nntp: STAT pipeline write: %w", end(err))
	}
	for i := range ids {
		code, msg, readErr := c.readStatus()
		if readErr != nil {
			c.broken = true
			return nil, fmt.Errorf("nntp: STAT pipeline reply %d/%d: %w", i+1, len(ids), end(readErr))
		}
		switch code {
		case 223:
			out[messageIDs[i]] = true
		case 430, 423, 451:
			out[messageIDs[i]] = false
		default:
			c.broken = true
			return nil, fmt.Errorf("nntp: STAT pipeline: %w", end(&ProtocolError{Cmd: "STAT", Code: code, Msg: msg}))
		}
	}
	c.lastRTT = time.Since(start)
	if err := end(nil); err != nil {
		return nil, err
	}
	return out, nil
}

// Group selects a newsgroup.
func (c *Conn) Group(ctx context.Context, name string) error {
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("nntp: invalid group %q", name)
	}
	code, msg, err := c.cmd(ctx, "GROUP", "GROUP "+name)
	if err != nil {
		return err
	}
	if code != 211 {
		return &ProtocolError{Cmd: "GROUP", Code: code, Msg: msg}
	}
	c.group = name
	return nil
}

// CurrentGroup is the last group selected with Group.
func (c *Conn) CurrentGroup() string { return c.group }

// Date returns the server's clock (DATE, 111). It doubles as a cheap
// keepalive.
func (c *Conn) Date(ctx context.Context) (time.Time, error) {
	code, msg, err := c.cmd(ctx, "DATE", "DATE")
	if err != nil {
		return time.Time{}, err
	}
	if code != 111 {
		return time.Time{}, &ProtocolError{Cmd: "DATE", Code: code, Msg: msg}
	}
	f := strings.Fields(msg)
	if len(f) == 0 {
		return time.Time{}, fmt.Errorf("nntp: DATE: malformed %q", msg)
	}
	t, err := time.Parse("20060102150405", f[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("nntp: DATE: malformed %q", msg)
	}
	return t, nil
}

// Quit says goodbye and closes the connection.
func (c *Conn) Quit() error {
	if !c.broken && !c.inBody {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, _ = c.cmd(ctx, "QUIT", "QUIT")
		cancel()
	}
	return c.Close()
}

// Close closes the connection without QUIT.
func (c *Conn) Close() error {
	c.broken = true
	return c.nc.Close()
}

// Broken reports whether the connection failed and must not be reused.
func (c *Conn) Broken() bool { return c.broken }

// Body issues BODY and returns a reader over the dot-unstuffed body. The
// reader must be closed (which drains any unread remainder) before the
// connection is used again.
func (c *Conn) Body(ctx context.Context, messageID string) (*BodyReader, error) {
	id, err := normalizeMessageID(messageID)
	if err != nil {
		return nil, err
	}
	if c.inBody {
		return nil, errors.New("nntp: BODY while a body is being read")
	}
	end, err := c.begin(ctx, c.commandTimeout)
	if err != nil {
		return nil, err
	}
	code, msg, err := c.roundTrip("BODY " + id)
	if err != nil {
		return nil, fmt.Errorf("nntp: BODY: %w", end(err))
	}
	if code != 222 {
		if err := end(nil); err != nil {
			return nil, fmt.Errorf("nntp: BODY: %w", err)
		}
		if code == 400 {
			c.broken = true
		}
		return nil, &ProtocolError{Cmd: "BODY", Code: code, Msg: msg}
	}
	c.inBody = true
	return &BodyReader{c: c, end: end}, nil
}

// BodyDecode fetches an article and yEnc-decodes it straight from the
// connection buffer. Decoding problems wrap ErrCorrupt; the connection stays
// usable after them because the body is drained to its terminator.
func (c *Conn) BodyDecode(ctx context.Context, messageID string, opt *yenc.DecodeOptions) (*yenc.Part, error) {
	br, err := c.Body(ctx, messageID)
	if err != nil {
		return nil, err
	}
	part, derr := yenc.DecodeLines(br, opt)
	cerr := br.Close()
	if br.ioErr != nil {
		// The transport failed; that is what the caller needs to see.
		return nil, br.ioErr
	}
	if cerr != nil {
		return nil, cerr
	}
	if derr != nil {
		return part, fmt.Errorf("%w: %s: %w", ErrCorrupt, messageID, derr)
	}
	return part, nil
}

// BodyReader streams a multi-line response, undoing dot-stuffing. It
// implements yenc.LineReader (zero-copy lines straight from the connection
// buffer) and io.Reader (lines with their CRLF terminators).
type BodyReader struct {
	c       *Conn
	end     func(error) error
	done    bool
	ioErr   error
	long    []byte
	pending []byte // io.Reader leftovers
}

// readRaw returns the next line including its terminator, unstuffed.
func (b *BodyReader) readRaw() ([]byte, error) {
	if b.done {
		if b.ioErr != nil {
			return nil, b.ioErr
		}
		return nil, io.EOF
	}
	line, err := b.c.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		b.long = append(b.long[:0], line...)
		for err == bufio.ErrBufferFull {
			if len(b.long) > maxBodyLine {
				// A server streaming one endless line must not grow this
				// buffer without bound; the rest of the body is unread, so
				// the connection cannot be reused (ErrBufferFull marks it
				// broken).
				b.long = nil
				b.finish(fmt.Errorf("%w: body line longer than %d bytes: %w", ErrCorrupt, maxBodyLine, bufio.ErrBufferFull))
				return nil, b.ioErr
			}
			line, err = b.c.r.ReadSlice('\n')
			b.long = append(b.long, line...)
		}
		line = b.long
	}
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		b.finish(err)
		return nil, b.ioErr
	}
	if line[0] == '.' {
		if len(line) == 3 && line[1] == '\r' || len(line) == 2 {
			b.finish(nil)
			return nil, io.EOF
		}
		line = line[1:]
	}
	return line, nil
}

func (b *BodyReader) finish(err error) {
	if b.done {
		return
	}
	b.done = true
	b.c.inBody = false
	if e := b.end(err); e != nil {
		b.ioErr = fmt.Errorf("nntp: BODY: %w", e)
	}
}

// ReadLine returns the next body line without its terminator. The slice is
// valid until the next call. It returns io.EOF after the terminating ".".
func (b *BodyReader) ReadLine() ([]byte, error) {
	line, err := b.readRaw()
	if err != nil {
		return nil, err
	}
	n := len(line) - 1
	if n >= 0 && line[n] == '\n' {
		if n > 0 && line[n-1] == '\r' {
			n--
		}
		line = line[:n]
	}
	return line, nil
}

// Read implements io.Reader over the unstuffed body.
func (b *BodyReader) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		line, err := b.readRaw()
		if err != nil {
			return 0, err
		}
		b.pending = line
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

// Close drains the remainder of the body so the connection can be reused.
func (b *BodyReader) Close() error {
	b.pending = nil
	for !b.done {
		if _, err := b.readRaw(); err != nil {
			break
		}
	}
	return b.ioErr
}

// wireBytes is how many bytes this connection has read off the wire.
func (c *Conn) wireBytes() int64 { return c.wire.Load() }

// countingConn counts bytes read for provider statistics (n, optional) and
// for this connection alone (own).
type countingConn struct {
	net.Conn
	n   *atomic.Int64
	own *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		if c.n != nil {
			c.n.Add(int64(n))
		}
		c.own.Add(int64(n))
	}
	return n, err
}
