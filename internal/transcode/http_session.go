package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// httpSession is one protocol=http conversion: ffmpeg writes streamable
// Matroska to stdout and the client's reads pace the pipe.
type httpSession struct {
	m    *Manager
	id   string
	req  core.HLSRequest
	life *encoderLifetime
	stub *Session
	log  *slog.Logger

	mu         sync.Mutex
	proc       Process
	closed     bool
	lastAccess time.Time
}

var _ core.HTTPSession = (*httpSession)(nil)

func (m *Manager) startHTTP(ctx context.Context, req core.HLSRequest, life *encoderLifetime) (core.HLSSession, error) {
	id := req.SessionID
	if id == "" {
		id = randomHex(12)
		req.SessionID = id
	}
	if req.InputLease != nil {
		if err := life.installInput(req.InputLease); err != nil {
			return nil, err
		}
	}
	if err := m.claimEncoderID(ctx, id, life); err != nil {
		return nil, err
	}
	safe := unsafeID.ReplaceAllString(id, "_")
	dir := filepath.Join(m.tempDir, "http-"+safe)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: http session dir: %w", err)
	}
	stub := &Session{
		id:      id,
		proto:   core.ProtocolHTTP,
		m:       m,
		dir:     dir,
		log:     m.log.With("session", id, "protocol", core.ProtocolHTTP),
		created: time.Now(),
		encoder: life,
		sessionDelivery: sessionDelivery{changed: make(chan struct{})},
	}
	if !life.attach(stub) {
		return nil, errors.New("transcode: session closed")
	}
	h := &httpSession{
		m:          m,
		id:         id,
		req:        req,
		life:       life,
		stub:       stub,
		log:        stub.log,
		lastAccess: time.Now(),
	}
	m.mu.Lock()
	if m.closed || ctx.Err() != nil || life.sealed {
		m.mu.Unlock()
		_ = os.RemoveAll(dir)
		return nil, errors.New("transcode: manager closed or start canceled")
	}
	if m.httpSessions == nil {
		m.httpSessions = map[string]*httpSession{}
	}
	m.httpSessions[id] = h
	m.mu.Unlock()
	h.log.Info("http session starting", "kind", req.Decision.Kind, "offset", req.StartOffset)
	return h, nil
}

func (s *httpSession) ID() string { return s.id }

func (s *httpSession) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not a playlist", http.StatusNotFound)
}

func (s *httpSession) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	http.Error(w, "not a segment", http.StatusNotFound)
}

func (s *httpSession) SwapInput(ctx context.Context, inputURL string, info core.MediaInfo, at time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("transcode: session closed")
	}
	s.req.InputURL = inputURL
	s.req.Info = info
	s.req.StartOffset = at
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
	return nil
}

func (s *httpSession) LastAccess() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccess
}

func (s *httpSession) Touch() {
	s.mu.Lock()
	s.lastAccess = time.Now()
	s.mu.Unlock()
}

func (s *httpSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	s.lastAccess = time.Now()
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
	spec := runSpec{
		LiveInput:    s.req.LiveInput,
		InputHeaders: s.req.InputHeaders,
		Input:        s.req.InputURL,
		Info:         s.req.Info,
		Decision:     s.req.Decision,
		Start:        s.req.StartOffset,
		Accel:        s.m.hw,
		Features:     s.m.feat,
		Protocol:     core.ProtocolHTTP,
	}
	args := buildArgs(spec)
	pr, pw := io.Pipe()
	cmd := Command{Path: s.m.ffmpeg, Args: args, Stdout: pw}
	proc, err := s.m.runner.Start(cmd)
	if err != nil {
		s.mu.Unlock()
		_ = pw.Close()
		_ = pr.Close()
		s.log.Warn("http ffmpeg start", "err", err)
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return
	}
	s.proc = proc
	s.life.markStarted()
	s.life.encoding.Store(!s.req.Decision.VideoCopy)
	s.mu.Unlock()

	go func() {
		<-proc.Done()
		_ = pw.Close()
	}()

	h := w.Header()
	h.Set("Content-Type", "video/x-matroska")
	h.Set("Accept-Ranges", "none")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		_ = pr.Close()
		proc.Stop()
		return
	}
	_, _ = io.Copy(flushWriter{w}, pr)
	_ = pr.Close()
	if r.Context().Err() != nil {
		proc.Stop()
	}
	s.mu.Lock()
	s.lastAccess = time.Now()
	if s.proc == proc {
		s.proc = nil
	}
	s.mu.Unlock()
}

func (s *httpSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	proc := s.proc
	s.proc = nil
	s.mu.Unlock()
	if proc != nil {
		proc.Stop()
	}
	s.m.mu.Lock()
	if s.m.httpSessions[s.id] == s {
		delete(s.m.httpSessions, s.id)
	}
	s.m.mu.Unlock()
	if s.life != nil {
		s.life.seal()
	}
	return nil
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}
