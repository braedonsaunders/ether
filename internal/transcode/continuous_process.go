package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Go binds before spawning: there is no port handoff race, no ExtraFiles,
// and Close can interrupt an accept even if ffmpeg never opens that input.
type rawSocket struct {
	ln     net.Listener
	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func newRawSocket() (*rawSocket, error) {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	return &rawSocket{ln: l}, nil
}
func (s *rawSocket) url() string { return "tcp://" + s.ln.Addr().String() }
func (s *rawSocket) accept() (net.Conn, error) {
	c, e := s.ln.Accept()
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return nil, net.ErrClosed
	}
	s.conn = c
	s.ln.Close()
	return c, nil
}
func (s *rawSocket) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.ln.Close()
	if s.conn != nil {
		s.conn.Close()
	}
}

type continuousProcess struct {
	ctx     context.Context
	cancel  context.CancelFunc
	proc    Process
	sockets []*rawSocket
	done    chan struct{}
	once    sync.Once
	err     error
	stderr  *tailBuffer
}

func (m *Manager) startContinuous(ctx context.Context, args []string, dir string, drain bool, sockets []*rawSocket, stdout io.Writer, release func(), observers ...func(string)) (*continuousProcess, error) {
	ctx, cancel := context.WithTimeout(ctx, maxProcessLifetime)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return nil, errors.New("transcode: manager closed")
	}
	// Register before launch so Manager.Close waits even for a concurrent Start.
	m.continuousWG.Add(1)
	m.mu.Unlock()
	stderr := newTailBuffer(24)
	input := ""
	for i, arg := range args {
		if arg == "-i" && i+1 < len(args) {
			input = args[i+1]
			break
		}
	}
	p, e := m.runner.Start(Command{Path: m.ffmpeg, Args: args, Dir: dir, Stdout: stdout, Stderr: &lineWriter{fn: func(line string) {
		for _, observe := range observers {
			observe(line)
		}
		stderr.add((redactingRunner{input: input}).redact(line))
	}}})
	if e != nil {
		cancel()
		m.continuousWG.Done()
		return nil, e
	}
	c := &continuousProcess{ctx: ctx, cancel: cancel, proc: p, sockets: sockets, done: make(chan struct{}), stderr: stderr}
	go func() {
		defer m.continuousWG.Done()
		select {
		case <-ctx.Done():
		case <-m.stop:
		case <-p.Done():
			if drain {
				c.err = p.Err()
				if release != nil {
					release()
				}
				close(c.done)
				return
			}
		}
		cancel()
		for _, s := range sockets {
			s.close()
		}
		p.Stop()
		c.err = p.Err()
		if release != nil {
			release()
		}
		close(c.done)
	}()
	return c, nil
}
func (p *continuousProcess) close() error {
	p.once.Do(func() {
		p.cancel()
		for _, s := range p.sockets {
			s.close()
		}
	})
	<-p.done
	return nil
}
func (p *continuousProcess) result() error {
	<-p.done
	if p.err != nil {
		return fmt.Errorf("continuous ffmpeg: %w: %s", p.err, strings.TrimSpace(p.stderr.String()))
	}
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		return fmt.Errorf("continuous ffmpeg: %w: %s", io.EOF, tail)
	}
	return io.EOF
}
func validateRaw(f RawFormat) error {
	if f.Width < 2 || f.Height < 2 || f.Width > 7680 || f.Height > 4320 || f.Width%2 != 0 || f.Height%2 != 0 || f.SampleRate != 48000 || f.Channels != 2 || f.PixelFormat != "yuv420p" {
		return errors.New("transcode: invalid continuous raw format")
	}
	if !((f.FPSNum == 30 && f.FPSDen == 1) || (f.FPSNum == 25 && f.FPSDen == 1) || (f.FPSNum == 30000 && f.FPSDen == 1001)) {
		return errors.New("transcode: unsupported continuous frame rate")
	}
	return nil
}
func tickPCM(f RawFormat, n int64) int {
	return int(((n+1)*int64(f.SampleRate*f.FPSDen)/int64(f.FPSNum) - n*int64(f.SampleRate*f.FPSDen)/int64(f.FPSNum)) * int64(f.Channels*2))
}
func frameDuration(f RawFormat) time.Duration {
	return time.Second * time.Duration(f.FPSDen) / time.Duration(f.FPSNum)
}

func (p *continuousProcess) writeError() error {
	select {
	case <-p.done:
		return p.result()
	default:
		return io.ErrClosedPipe
	}
}
