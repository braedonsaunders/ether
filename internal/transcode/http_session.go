package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	log  *slog.Logger

	mu         sync.Mutex
	proc       Process
	pipe       *io.PipeReader
	requestSeq uint64
	closed     bool
	forceCPU   bool
	delivered  atomic.Bool
	lastAccess time.Time
}

var _ core.HTTPSession = (*httpSession)(nil)
var _ core.OwnedEncoderSession = (*httpSession)(nil)

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
	encoding := !req.Decision.VideoCopy
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("transcode: manager closed")
	}
	if encoding && m.cfg.MaxTranscodes > 0 {
		if m.encodingLocked("") >= m.cfg.MaxTranscodes {
			m.preemptBackgroundLocked()
		}
		if n := m.encodingLocked(""); n >= m.cfg.MaxTranscodes {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w (%d running)", ErrTooManyTranscodes, n)
		}
	}
	life.encoding.Store(encoding)
	m.mu.Unlock()
	// Attach uses dir only as a cleanup key; HTTP output is a pipe.
	dir := filepath.Join(m.tempDir, "http-"+unsafeID.ReplaceAllString(id, "_")+"-"+randomHex(4))
	stub := &Session{
		id:              id,
		proto:           core.ProtocolHTTP,
		m:               m,
		dir:             dir,
		log:             m.log.With("session", id, "protocol", core.ProtocolHTTP),
		created:         time.Now(),
		encoder:         life,
		sessionDelivery: sessionDelivery{changed: make(chan struct{})},
	}
	if !life.attach(stub) {
		return nil, errors.New("transcode: session closed")
	}
	h := &httpSession{
		m:    m,
		id:   id,
		req:  req,
		life: life,

		log:        stub.log,
		lastAccess: time.Now(),
	}
	// This ticket retains responsibility for HTTP registry/pipe detachment even
	// when Start is canceled before the lazy HTTP session is returned.
	detach, err := life.reserve(1)
	if err != nil {
		return nil, err
	}
	go func() {
		<-life.cancel
		h.detachOwnedHTTP()
		detach[0].finish()
	}()
	life.mu.Lock()
	defer life.mu.Unlock()
	m.mu.Lock()
	if m.closed || ctx.Err() != nil || life.sealed {
		m.mu.Unlock()
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
	s.requestSeq++
	if s.pipe != nil {
		_ = s.pipe.CloseWithError(errors.New("transcode: input changed"))
		s.pipe = nil
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

func (s *httpSession) Closed() bool {
	s.mu.Lock()
	closed, life := s.closed, s.life
	s.mu.Unlock()
	return closed || (life != nil && life.isSealed())
}

func (s *httpSession) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

func (s *httpSession) writeHTTPHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "video/x-matroska")
	h.Set("Accept-Ranges", "none")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
}

func (s *httpSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed || s.life.isSealed() {
		s.mu.Unlock()
		http.Error(w, "session closed", http.StatusGone)
		return
	}
	s.lastAccess = time.Now()
	// A byte range cannot resume an encoder's Matroska output: a new encoder
	// writes a different header and its offsets do not describe the old stream.
	if s.forceCPU && r.Header.Get("Range") != "" {
		s.mu.Unlock()
		http.Error(w, "progressive transcode does not support byte ranges", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if r.Method == http.MethodHead {
		s.mu.Unlock()
		s.writeHTTPHeaders(w)
		return
	}
	s.requestSeq++
	seq := s.requestSeq
	if s.pipe != nil {
		_ = s.pipe.CloseWithError(errors.New("transcode: superseded request"))
		s.pipe = nil
	}
	if s.proc != nil {
		s.proc.Stop()
		s.proc = nil
	}
	s.mu.Unlock()
	silenceAudio := false
	for attempt := 0; attempt < 2; attempt++ {
		s.mu.Lock()
		if s.closed || s.life.isSealed() || s.requestSeq != seq || r.Context().Err() != nil {
			s.mu.Unlock()
			return
		}
		accel := s.m.hw
		if s.forceCPU || time.Now().UnixNano() < s.m.httpHWFailedUntil.Load() {
			accel = core.HWAccel{Name: AccelNone}
		}
		spec := runSpec{SilenceAudio: silenceAudio, LiveInput: s.req.LiveInput, CopyTimestamps: s.req.CopyTimestamps, InputHeaders: s.req.InputHeaders, Input: s.req.InputURL, Info: s.req.Info, Decision: s.req.Decision, Start: s.req.StartOffset, Accel: accel, Features: s.m.feat, Protocol: core.ProtocolHTTP, InputTimeout: s.m.opts.stallTimeout}
		scopeHeaders, err := encoderScopeHeaders(s.req.InputLease, s.req.InputURL)
		if err != nil {
			s.mu.Unlock()
			http.Error(w, "input authority unavailable", http.StatusServiceUnavailable)
			return
		}
		spec.InputScopeHeaders = scopeHeaders
		pr, pw := io.Pipe()
		stderr := newTailBuffer(40)
		cmd := Command{Path: s.m.ffmpeg, Args: buildArgs(spec), Stdout: pw, Stderr: &lineWriter{fn: stderr.add}}
		encoding := !s.req.Decision.VideoCopy
		work, err := s.life.runWork(encoding)
		if err != nil {
			s.mu.Unlock()
			_ = pw.Close()
			_ = pr.Close()
			http.Error(w, "transcode failed", http.StatusServiceUnavailable)
			return
		}
		proc, err := s.m.runner.Start(cmd)
		if err != nil {
			s.mu.Unlock()
			for _, workItem := range work {
				workItem.finish()
			}
			_ = pw.Close()
			_ = pr.Close()
			s.log.Warn("http ffmpeg start", "err", err)
			http.Error(w, "transcode failed", http.StatusInternalServerError)
			return
		}
		s.proc = proc
		s.pipe = pr
		s.life.markStarted()
		s.life.encoding.Store(encoding)
		s.mu.Unlock()
		// Process exit and the serving writer are independent producers. A blocked
		// downstream writer must keep retirement pending even after ffmpeg exits.
		defer work[1].finish()
		go func() {
			<-proc.Done()
			_ = pw.Close()
			work[0].finish()
		}()
		stopCancel := context.AfterFunc(r.Context(), func() { _ = pr.CloseWithError(r.Context().Err()); proc.Stop() })
		var started atomic.Bool
		var timer *time.Timer
		if timeout := s.m.opts.stallTimeout; timeout > 0 {
			timer = time.AfterFunc(timeout, func() {
				if !started.Load() {
					_ = pr.CloseWithError(errors.New("transcode: HTTP startup stalled"))
					proc.Stop()
				}
			})
		}
		buf := make([]byte, 32*1024)
		n, readErr := pr.Read(buf)
		if n > 0 {
			started.Store(true)
		}
		if timer != nil {
			timer.Stop()
		}
		var copyErr error
		clientWriteFailed := false
		if n > 0 {
			s.writeHTTPHeaders(w)
			fw := &httpWriteTracker{writer: flushWriter{ResponseWriter: w, s: s}}
			if spec.usesHW() {
				copyErr = copyHTTPMatroska(fw, io.MultiReader(bytes.NewReader(buf[:n]), pr))
			} else {
				_, copyErr = fw.Write(buf[:n])
				if copyErr == nil && readErr == nil {
					_, copyErr = io.Copy(fw, pr)
				}
			}
			clientWriteFailed = fw.failed
		}
		_ = pr.Close()
		stopCancel()
		if r.Context().Err() != nil || copyErr != nil || n == 0 {
			proc.Stop()
		}
		<-proc.Done()
		procErr := proc.Err()
		s.mu.Lock()
		current := !s.closed && s.requestSeq == seq
		hardwareFailed := current && r.Context().Err() == nil && !clientWriteFailed && spec.usesHW() && procErr != nil && looksLikeEncoderFailure(stderr.String(), spec)
		if hardwareFailed {
			s.forceCPU = true
			s.m.httpHWFailedUntil.Store(time.Now().Add(5 * time.Minute).UnixNano())
			s.log.Warn("progressive hardware encoder failed; next request will use CPU", "hwaccel", spec.Accel.Name)
		}
		s.lastAccess = time.Now()
		if s.proc == proc {
			s.proc = nil
		}
		if s.pipe == pr {
			s.pipe = nil
		}
		s.mu.Unlock()
		if n > 0 {
			if hardwareFailed {
				// net/http closes the HTTP/1 connection (or resets an HTTP/2
				// stream) without a final chunk. A clean EOF looks like a
				// completed movie to clients and can advance the play queue.
				panic(http.ErrAbortHandler)
			}
			return
		}
		if r.Context().Err() != nil || !current {
			return
		}
		if attempt == 0 {
			if spec.LiveInput && procErr != nil && missingLiveAudio(spec, stderr.String()) {
				silenceAudio = true
			}
			s.log.Warn("http transcode produced no output; retrying")
			continue
		}
		http.Error(w, "transcode failed", http.StatusBadGateway)
		return
	}
}

// ProgressiveFailed reports a hardware failure to the timeline owner so a
// late terminal update cannot replace the last reported playhead with EOF.
func (s *httpSession) ProgressiveFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forceCPU
}

func (s *httpSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	proc := s.proc
	pipe := s.pipe
	s.pipe = nil
	s.proc = nil
	s.mu.Unlock()
	if pipe != nil {
		_ = pipe.CloseWithError(errors.New("transcode: session closed"))
	}
	if proc != nil {
		proc.Stop()
		<-proc.Done()
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

type flushWriter struct {
	http.ResponseWriter
	s *httpSession
}

type httpWriteTracker struct {
	writer io.Writer
	failed bool
}

func (w *httpWriteTracker) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil || n != len(p) {
		w.failed = true
	}
	return n, err
}

func (w flushWriter) Write(p []byte) (int, error) {
	if w.s != nil {
		w.s.Touch()
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.s != nil {
		w.s.delivered.Store(true)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// EncoderRetirement covers the exact lazy HTTP execution and its input producers.
func (s *httpSession) EncoderRetirement() core.EncoderRetirement { return s.life }

// RetireEncoder never takes the HTTP mutex (Runner.Start may hold it) or waits
// for process exit. The registered detach producer handles late publication.
func (s *httpSession) RetireEncoder() error {
	if s.life == nil {
		return errors.New("transcode: session has no encoder responsibility")
	}
	s.life.seal()
	return nil
}

func (s *httpSession) detachOwnedHTTP() {
	s.mu.Lock()
	s.closed = true
	s.requestSeq++
	proc, pipe := s.proc, s.pipe
	s.proc, s.pipe = nil, nil
	s.mu.Unlock()
	if pipe != nil {
		_ = pipe.CloseWithError(errors.New("transcode: session retired"))
	}
	if proc != nil {
		proc.Stop()
	}
	s.m.mu.Lock()
	if s.m.httpSessions[s.id] == s {
		delete(s.m.httpSessions, s.id)
	}
	s.m.mu.Unlock()
}

// PlaybackDelivered reports output bytes, not source reads by ffmpeg.
func (s *httpSession) PlaybackDelivered() bool { return s.delivered.Load() }
