package clusterplayback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playbackbuffer"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
)

// OutputBufferSnapshot reports retained container observations. Fixed-grid
// recovery maps the immutable prefix to its original generations; observations
// do not prove viewer delivery, decoder continuity or permission to retire.
type OutputBufferSnapshot struct {
	Fence     placement.Fence
	Retaining bool
	Reason    string
	Buffer    playbackbuffer.Snapshot
}

type bufferedSession struct {
	*controlledSession // Preserve all negotiated optional session methods.
	r                  *Runtime
	route              Route
	access             atomic.Int64
	published          atomic.Bool
	mu                 sync.Mutex
	capture            chan struct{}
	headers            map[int64]http.Header // At most one entry per retained segment.
	supersededAt       int64                 // -1 until a fixed-grid replacement is committed (Runtime.mu).
	reason             string
	// unbuffered is set once a segment could not be retained and was delivered
	// anyway: the viewer is past the restart watermark, so this session is no
	// longer recoverable and later segments pass straight through.
	unbuffered bool
}

func (r *Runtime) newBufferedSession(route Route, q core.HLSRequest, s *remotetranscode.Session) *bufferedSession {
	// No implicit conversion, ABR, seek, subtitle or other representation cache.
	// Those operations continue using the existing exact remote adapter.
	if r.automaticOutput && !encoderRecoverySupported(q, route.Encoder) {
		return nil
	}
	if r.o.OutputBuffer == nil || s == nil || q.StartOffset != 0 || q.LiveInput || q.Decision.Container != "mpegts" || (q.Protocol != "" && q.Protocol != core.ProtocolHLS) || len(s.Rungs()) > 1 {
		return nil
	}
	state := playbackbuffer.State{Generation: uint64(route.Fence.Generation), PublishedFloor: -1}
	if err := r.o.OutputBuffer.OpenSession(route.Fence.Session, state); err != nil {
		slog.Warn("recovery buffer admission refused", "session", route.Fence.Session, "error", err)
		return nil // Existing public sequences are never overwritten or evicted.
	}
	return &bufferedSession{controlledSession: &controlledSession{Session: s, r: r}, r: r, route: route, capture: make(chan struct{}, 1), headers: make(map[int64]http.Header), supersededAt: -1}
}

const (
	// outputScratchBudgetBytes bounds worst-case concurrent segment capture
	// memory across buffered sessions (128 MiB only when MaxSegmentBytes > 32 MiB).
	outputScratchBudgetBytes int64 = 64 << 20
	maxPooledSegmentScratch        = 16 << 20
)

func outputScratchSlots(maxSegmentBytes int64) int {
	if maxSegmentBytes <= 0 {
		return 2
	}
	return min(max(int(outputScratchBudgetBytes/maxSegmentBytes), 2), 16)
}

func (r *Runtime) acquireOutputScratch() []byte {
	if p, ok := r.outputScratchPool.Get().(*[]byte); ok && p != nil {
		return (*p)[:0]
	}
	return nil
}

func (r *Runtime) releaseOutputScratch(b []byte) {
	limit := int(min(r.o.MaxSegmentBytes, maxPooledSegmentScratch))
	if cap(b) == 0 || (limit > 0 && cap(b) > limit) {
		return
	}
	b = b[:0]
	r.outputScratchPool.Put(&b)
}

func (s *bufferedSession) captureGate() chan struct{} {
	s.mu.Lock()
	if s.capture == nil {
		s.capture = make(chan struct{}, 1)
	}
	ch := s.capture
	s.mu.Unlock()
	return ch
}

func (r *Runtime) SnapshotOutput(id string) (OutputBufferSnapshot, error) {
	r.mu.Lock()
	x := r.executions[id]
	var output *bufferedSession
	if x != nil {
		output = x.output
	}
	r.mu.Unlock()
	if output == nil {
		return OutputBufferSnapshot{}, remotetranscode.ErrUnsupported
	}
	output.mu.Lock()
	reason := output.reason
	output.mu.Unlock()
	snapshot, err := r.o.OutputBuffer.Snapshot(id)
	return OutputBufferSnapshot{Fence: output.route.Fence, Retaining: reason == "", Reason: reason, Buffer: snapshot}, err
}

// ReleaseOutput is explicit final-session disposal, not rolling eviction. The
// caller must already have stopped public session publication. Exact durable
// retirement and no active readers are required; historical receipts alone do
// not authorize disposal of a later generation's bytes.
func (r *Runtime) ReleaseOutput(ctx context.Context, id string, fence placement.Fence) error {
	if r.o.OutputBuffer == nil || id != fence.Session {
		return ErrDenied
	}
	record, err := r.o.Placement.Lookup(ctx, id)
	if err != nil || record.Fence != fence || !record.Retired {
		return ErrPending
	}
	r.mu.Lock()
	x := r.executions[id]
	valid := x != nil && x.route.Fence == fence && x.retired && x.output != nil
	r.mu.Unlock()
	if !valid {
		return ErrDenied
	}
	return r.o.OutputBuffer.CloseSession(id)
}

func (s *bufferedSession) LastAccess() time.Time {
	access := time.Unix(0, s.access.Load())
	if original := s.Session.LastAccess(); original.After(access) {
		return original
	}
	return access
}
func (s *bufferedSession) key(n int) playbackbuffer.Key {
	return playbackbuffer.Key{Session: s.route.Fence.Session, Generation: uint64(s.route.Fence.Generation), Sequence: int64(n), Kind: playbackbuffer.Segment}
}

// passThrough reports whether segment n is served without capture: the
// session gave up buffering, or n lies at or below the published floor (its
// retained bytes were released by MakeRoom), so delivering it cannot put the
// viewer past the restart watermark.
func (s *bufferedSession) passThrough(n int) bool {
	s.mu.Lock()
	unbuffered := s.unbuffered
	s.mu.Unlock()
	if unbuffered {
		return true
	}
	floor, err := s.r.o.OutputBuffer.Floor(s.route.Fence.Session)
	return err == nil && int64(n) <= floor
}

// unbuffer stops retention for a still-current session, so a segment the
// buffer cannot hold may be delivered. Recovery checks the flag under s.mu
// before committing a replacement, so it never resumes behind a viewer.
func (s *bufferedSession) unbuffer(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r.mu.Lock()
	current := s.r.executions[s.route.Fence.Session]
	active := current != nil && current.route == s.route && current.replacement == nil && !current.stopping
	s.r.mu.Unlock()
	if !active {
		return false
	}
	if !s.unbuffered {
		s.unbuffered = true
		if s.reason == "" {
			s.reason = "segment retention unavailable; recovery disabled"
		}
		slog.Warn("recovery buffer full, continuing without encoder recovery", "session", s.route.Fence.Session, "generation", s.route.Fence.Generation, "segment", n)
	}
	return true
}

func (s *bufferedSession) isUnbuffered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unbuffered
}

func (s *bufferedSession) degrade(reason string) {
	s.mu.Lock()
	if s.reason == "" {
		s.reason = reason
	}
	s.mu.Unlock()
}

func (s *bufferedSession) ServeSegment(w http.ResponseWriter, q *http.Request, n int) {
	s.serveSegment(w, q, n, func(out http.ResponseWriter, request *http.Request) remotetranscode.RepresentationCompletion {
		return s.Session.ServeSegmentObserved(out, request, n)
	})
}
func (s *bufferedSession) ServeRungSegment(w http.ResponseWriter, q *http.Request, rung string, n int) {
	if rung != "base" {
		s.Session.ServeRungSegment(w, q, rung, n)
		return
	}
	s.serveSegment(w, q, n, func(out http.ResponseWriter, request *http.Request) remotetranscode.RepresentationCompletion {
		return s.Session.ServeRungSegmentObserved(out, request, rung, n)
	})
}

func (s *bufferedSession) serveSegment(w http.ResponseWriter, q *http.Request, n int, serve func(http.ResponseWriter, *http.Request) remotetranscode.RepresentationCompletion) {
	retained := false
	guarded := s.r.fencedDeliveryResolved(w, q, s.route, func() (Route, error) {
		if !retained {
			return s.route, nil
		}
		return s.retainedDeliveryRoute(q.Context(), n)
	})
	defer guarded.Close()
	w = guarded
	// Conditional and partial requests keep the exact upstream semantics. A 206,
	// HEAD, or even a Range request answered 200 is not capture admission.
	if n < 0 || n > 10000000 || q.Method != http.MethodGet || q.Header.Get("Range") != "" ||
		q.Header.Get("If-Range") != "" || q.Header.Get("If-Match") != "" ||
		q.Header.Get("If-None-Match") != "" || q.Header.Get("If-Modified-Since") != "" ||
		q.Header.Get("If-Unmodified-Since") != "" {
		serve(w, q)
		return
	}
	key := s.key(n)
	if s.serveRetained(w, q, n, key, &retained) {
		return
	}
	if s.passThrough(n) {
		serve(w, q)
		return
	}
	// Backpressure precedes allocation and the encoder RPC. A busy capture must
	// never turn a recoverable response into an unretained successful segment.
	waitBound := s.r.o.EncoderSegmentWait
	if waitBound <= 0 {
		waitBound = 30 * time.Second
	}
	wait, cancel := context.WithTimeout(q.Context(), waitBound)
	defer cancel()
	gate := s.captureGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-wait.Done():
		http.Error(w, "segment capture unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.serveRetained(w, q, n, key, &retained) {
		return
	}
	if s.passThrough(n) {
		serve(w, q)
		return
	}
	select {
	case s.r.outputScratch <- struct{}{}:
		defer func() { <-s.r.outputScratch }()
	case <-wait.Done():
		http.Error(w, "segment capture unavailable", http.StatusServiceUnavailable)
		return
	}
	// Another reader may have published this immutable key while we waited.
	if s.serveRetained(w, q, n, key, &retained) {
		return
	}
	// Scratch is charged globally BEFORE allocation/read. No recorder, pipe,
	// child reader or retry exists. Hold only the final representation byte until
	// validated contiguous publication; preceding bytes retain backpressure.
	capture := &segmentCapture{ResponseWriter: w, limit: s.r.o.MaxSegmentBytes, body: s.r.acquireOutputScratch()}
	defer func() { s.r.releaseOutputScratch(capture.body) }()
	completion := serve(capture, q)
	if completion.StatusCode == http.StatusServiceUnavailable && capture.Header().Get("Retry-After") != "" {
		return
	}
	if !completion.Complete() || completion.ReadBytes != int64(len(capture.body)) || !capture.complete(q.Context()) {
		s.degrade("incomplete or unsupported full segment response")
		return
	}
	if s.r.deliveryAuthority(q.Context(), s.route) != nil {
		return
	}
	err := s.r.o.OutputBuffer.Retain(q.Context(), key, playbackbuffer.MPEGTS, int64(len(capture.body)), bytes.NewReader(capture.body))
	if errors.Is(err, playbackbuffer.ErrCapacity) && s.r.o.OutputBuffer.MakeRoom(key, int64(len(capture.body))) {
		err = s.r.o.OutputBuffer.Retain(q.Context(), key, playbackbuffer.MPEGTS, int64(len(capture.body)), bytes.NewReader(capture.body))
	}
	if errors.Is(err, playbackbuffer.ErrCapacity) {
		// Withholding the last byte would stop playback for good: the
		// buffer never frees enough (other sessions hold the global pool,
		// or this segment alone exceeds the per-session budget). Deliver it
		// and give up recovery for this session instead.
		if s.unbuffer(n) && capture.complete(q.Context()) {
			_, _ = w.Write(capture.body[len(capture.body)-1:])
		}
		return
	}
	if err != nil {
		s.degrade("segment retention unavailable")
		return
	}
	// Retain inspects the final bytes; structural TS acceptance is required
	// here, but still says nothing about sample ends or decoder/client behavior.
	staged, err := s.r.o.OutputBuffer.Snapshot(key.Session)
	verified := false
	if err == nil {
		for _, entry := range staged.Entries {
			if entry.Key == key {
				verified = entry.Facts.Verified
				break
			}
		}
	}
	if !verified {
		_ = s.r.o.OutputBuffer.Delete(key)
		s.degrade("unverified TS structure")
		return
	}
	s.mu.Lock()
	s.r.mu.Lock()
	current := s.r.executions[s.route.Fence.Session]
	active := current != nil && current.route == s.route && current.replacement == nil && !current.stopping
	s.r.mu.Unlock()
	if !active {
		s.mu.Unlock()
		return
	}
	s.headers[int64(n)] = capture.headers
	s.mu.Unlock()
	_ = s.publishStaged(q.Context())
	// A full client receipt can only follow publication. Future staged segments
	// stay incomplete until their missing prefix is published by another request.
	lease, err := s.r.o.OutputBuffer.Acquire(key)
	if err != nil {
		return
	}
	defer lease.Close()
	if capture.complete(q.Context()) {
		_, _ = w.Write(capture.body[len(capture.body)-1:])
	}
}

func (s *bufferedSession) publishStaged(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r.mu.Lock()
	x := s.r.executions[s.route.Fence.Session]
	active := x != nil && x.route == s.route && x.replacement == nil && !x.stopping && !x.retired
	s.r.mu.Unlock()
	if !active {
		return ErrDenied
	}
	key := s.key(0)
	// Staged future segments may arrive out of order. Publish only the complete
	// contiguous range; old public keys remain immutable at the store boundary.
	snapshot, err := s.r.o.OutputBuffer.Snapshot(key.Session)
	if err == nil {
		var keys []playbackbuffer.Key
		for next := snapshot.State.PublishedFloor + 1; next <= 10000000; next++ {
			if s.headers[next] == nil {
				break
			}
			candidate := key
			candidate.Sequence = next
			keys = append(keys, candidate)
		}
		if len(keys) > 0 {
			err = s.r.deliveryAuthority(ctx, s.route)
			if err == nil {
				err = s.r.o.OutputBuffer.Publish(key.Session, snapshot.State, keys)
			}
			if err == nil {
				s.published.Store(true)
			}
		}
	}
	if err != nil && s.reason == "" {
		s.reason = "contiguous publication unavailable"
	}
	return err
}

func (s *bufferedSession) serveRetained(w http.ResponseWriter, q *http.Request, n int, key playbackbuffer.Key, retained *bool) bool {
	s.mu.Lock()
	headers := s.headers[int64(n)].Clone()
	s.mu.Unlock()
	if headers != nil {
		lease, err := s.r.o.OutputBuffer.Acquire(key)
		if err != nil {
			// A prior request may have staged valid bytes but lost its context
			// before publication. Retry the commit, never the worker read.
			if s.publishStaged(q.Context()) == nil {
				lease, err = s.r.o.OutputBuffer.Acquire(key)
			}
		}
		if err != nil && !s.r.o.OutputBuffer.Has(key) {
			// MakeRoom released these bytes behind the restart watermark.
			s.mu.Lock()
			delete(s.headers, int64(n))
			s.mu.Unlock()
			return false
		}
		if err == nil {
			defer lease.Close()
			// The outer authenticated broker owns current viewer/media policy. Cache
			// delivery additionally requires the exact current controller authority.
			record, err := s.r.o.Placement.Lookup(q.Context(), s.route.Fence.Session)
			if err != nil || !s.allowedFence(record.Fence, n) || !record.Authority || record.Retired || s.r.o.MemberRole(q.Context(), s.r.o.Self, config.RoleServer) != nil {
				http.Error(w, "output authority unavailable", http.StatusServiceUnavailable)
				return true
			}
			s.r.mu.Lock()
			x := s.r.executions[s.route.Fence.Session]
			active := !s.r.closed && x != nil && s.allowedExecution(x, n) && !x.stopping && !x.retired
			s.r.mu.Unlock()
			if !active {
				http.Error(w, "execution stopped", http.StatusGone)
				return true
			}
			*retained = true
			s.access.Store(time.Now().UnixNano())
			for name, values := range headers {
				w.Header()[name] = values
			}
			length, _ := strconv.ParseInt(headers.Get("Content-Length"), 10, 64)
			w.WriteHeader(http.StatusOK)
			_, _ = io.Copy(w, io.NewSectionReader(lease, 0, length))
			return true
		}
		// A staged but unpublished key cannot acknowledge successful delivery.
		http.Error(w, "segment publication pending", http.StatusServiceUnavailable)
		return true
	}
	return false
}

type segmentCapture struct {
	http.ResponseWriter
	limit   int64
	body    []byte
	headers http.Header
	status  int
	length  int64
	failed  bool
}

func (w *segmentCapture) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *segmentCapture) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	var size int
	for name, values := range w.Header() {
		if !outputHeader(name) {
			continue
		}
		for _, value := range values {
			if len(value) > (16<<10)-size-len(name) {
				w.failed = true
				break
			}
			size += len(name) + len(value)
		}
		if w.failed {
			break
		}
	}
	contentType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if status != http.StatusOK || err != nil || contentType != "video/mp2t" || w.Header().Get("Content-Range") != "" {
		w.failed = true
	}
	for _, directive := range strings.Split(w.Header().Get("Cache-Control"), ",") {
		name := strings.TrimSpace(strings.SplitN(directive, "=", 2)[0])
		// no-cache controls HTTP reuse, not this private immutable recovery
		// log. Every replay rechecks current authority, and preserves the
		// original client-facing cache policy. Explicit no-store still refuses
		// retention of the representation.
		if strings.EqualFold(name, "no-store") {
			w.failed = true
		}
	}
	if status == http.StatusOK {
		length, e := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
		w.length = length
		if e != nil || len(w.Header().Values("Content-Length")) != 1 || length <= 0 || length > w.limit {
			w.failed = true
		}
		if w.failed {
			for name := range w.Header() {
				delete(w.Header(), name)
			}
			http.Error(w.ResponseWriter, "segment capture unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if !w.failed {
		w.headers = make(http.Header)
		for name, values := range w.Header() {
			if outputHeader(name) {
				w.headers[name] = append([]string(nil), values...)
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *segmentCapture) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.status != http.StatusOK {
		return w.ResponseWriter.Write(p)
	}
	if w.failed || int64(len(p)) > w.length-int64(len(w.body)) {
		w.failed = true
		return 0, errors.New("segment capture incomplete")
	}
	count := len(p)
	if int64(len(w.body)+count) == w.length && count > 0 {
		count--
	}
	if count > 0 {
		n, err := w.ResponseWriter.Write(p[:count])
		if err != nil || n != count {
			w.failed = true
			w.body = w.body[:0]
			return n, err
		}
	}
	if need := len(w.body) + len(p); need > cap(w.body) {
		nextCap := max(cap(w.body)*2, need, 64<<10)
		if int64(nextCap) > w.length {
			nextCap = int(w.length)
		}
		grown := make([]byte, len(w.body), nextCap)
		copy(grown, w.body)
		w.body = grown
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *segmentCapture) complete(ctx context.Context) bool {
	length, err := strconv.ParseInt(w.headers.Get("Content-Length"), 10, 64)
	if len(w.headers.Values("Content-Length")) != 1 {
		return false
	}
	// HLSSession handlers have no error/EOF return. Unknown/chunked framing
	// cannot certify completeness, even when a handler returned normally.
	return !w.failed && ctx.Err() == nil && w.status == http.StatusOK && err == nil && length > 0 && length <= w.limit && int64(len(w.body)) == length
}

var _ core.HLSLadderSession = (*bufferedSession)(nil)
var _ core.DASHSession = (*bufferedSession)(nil)
var _ core.HTTPSession = (*bufferedSession)(nil)

// Preserve even upstream noncanonical header spelling; Header.Get alone cannot
// read maps populated directly with keys such as ETag by the remote adapter.
func outputHeader(name string) bool {
	for _, allowed := range deliveryHeaders {
		if strings.EqualFold(name, allowed) {
			return true
		}
	}
	return false
}

func (s *bufferedSession) allowedExecution(x *execution, n int) bool {
	if x == nil {
		return false
	}
	if x.route == s.route && x.replacement == nil {
		return true
	}
	return s.supersededAt >= 0 && int64(n) < s.supersededAt
}
func (s *bufferedSession) allowedFence(f placement.Fence, n int) bool {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	x := s.r.executions[s.route.Fence.Session]
	if x == nil {
		return false
	}
	current := x.route.Fence
	if x.replacement != nil {
		current = x.replacement.route.Fence
	}
	return current == f && s.allowedExecution(x, n)
}

// Historical immutable bytes are authorized by their published prefix, while
// the writer is fenced by the currently authoritative execution generation.
func (s *bufferedSession) retainedDeliveryRoute(ctx context.Context, n int) (Route, error) {
	current, err := s.r.route(ctx, s.route.Fence.Session)
	if err != nil || current.Viewer != s.route.Viewer || current.Revision != s.route.Revision || !s.allowedFence(current.Fence, n) {
		return Route{}, ErrDenied
	}
	return current, nil
}
