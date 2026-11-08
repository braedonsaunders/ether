package clusterplayback

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
)

// recoverySession is a stable public session: its optional methods remain the
// negotiated remote contract, while media resolves the current encoder on each
// request. Published historical segments keep their exact original bytes.
type recoverySession struct{ *bufferedSession }

func (r *Runtime) recoveryDelivery(s *bufferedSession) core.HLSSession {
	r.mu.Lock()
	x := r.executions[s.route.Fence.Session]
	enabled := x != nil && x.initial != nil && encoderRecoverySupported(x.initial.request, x.route.Encoder)
	r.mu.Unlock()
	if enabled {
		return &recoverySession{s}
	}
	return s
}
func (s *recoverySession) resolve(ctx context.Context, n int) (*bufferedSession, error) {
	ctx, cancel := context.WithTimeout(ctx, s.r.o.EncoderSegmentWait)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		s.r.mu.Lock()
		x := s.r.executions[s.route.Fence.Session]
		if x == nil || x.stopping || x.retired || s.r.closed {
			s.r.mu.Unlock()
			return nil, ErrDenied
		}
		for _, old := range x.previous {
			if n >= 0 && int64(n) < old.supersededAt {
				s.r.mu.Unlock()
				return old, nil
			}
		}
		if x.replacement == nil && x.output != nil {
			out := x.output
			s.r.mu.Unlock()
			return out, nil
		}
		if n >= 0 && x.output != nil && int64(n) < x.output.supersededAt {
			out := x.output
			s.r.mu.Unlock()
			return out, nil
		}
		s.r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *recoverySession) ServeSegment(w http.ResponseWriter, q *http.Request, n int) {
	s.fetch(w, q, "", n)
}
func (s *recoverySession) ServeRungSegment(w http.ResponseWriter, q *http.Request, rung string, n int) {
	s.fetch(w, q, rung, n)
}
func (s *recoverySession) fetch(w http.ResponseWriter, q *http.Request, rung string, n int) {
	ctx, cancel := context.WithTimeout(q.Context(), s.r.o.EncoderSegmentWait)
	defer cancel()
	select {
	case s.r.deliveryScratch <- struct{}{}:
		defer func() { <-s.r.deliveryScratch }()
	case <-ctx.Done():
		http.Error(w, "encoder recovery pending", 503)
		return
	}
	for ctx.Err() == nil {
		out, err := s.resolve(ctx, n)
		if err != nil {
			break
		}
		attempt, cancelAttempt := context.WithTimeout(ctx, 2*time.Second)
		response := &pendingSegment{headers: make(http.Header), limit: s.r.o.MaxSegmentBytes}
		if rung == "" {
			out.ServeSegment(response, q.WithContext(attempt), n)
		} else {
			out.ServeRungSegment(response, q.WithContext(attempt), rung, n)
		}
		finished := attempt.Err() == nil && response.complete(q.Method)
		cancelAttempt()
		// Validate AFTER receiving the complete representation, before any client
		// bytes. A partitioned former encoder cannot win a late response race.
		// 502 is the adapter's own answer when no worker response arrived (dial,
		// TLS or transport failure). Like 503 and 404 it is retried until the
		// wait expires: the worker may be the encoder being replaced, and a dial
		// cut off by the attempt deadline can fail before attempt.Err() reports
		// it, so finished alone cannot tell it apart from a real response.
		retry := response.status == 0 || response.status == http.StatusServiceUnavailable || response.status == http.StatusNotFound || response.status == http.StatusBadGateway
		rec, lookupErr := s.r.o.Placement.Lookup(ctx, s.route.Fence.Session)
		if lookupErr == nil && rec.Authority && !rec.Retired && out.allowedFence(rec.Fence, n) && finished && !response.overflow && !retry {
			guarded := s.r.fencedDeliveryResolved(w, q, out.route, func() (Route, error) { return out.retainedDeliveryRoute(ctx, n) })
			defer guarded.Close()
			w = guarded
			for k, v := range response.headers {
				w.Header()[k] = v
			}
			w.WriteHeader(response.status)
			_, _ = io.Copy(w, bytes.NewReader(response.body.Bytes()))
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	http.Error(w, "encoder recovery pending", 503)
}

type pendingSegment struct {
	headers  http.Header
	body     bytes.Buffer
	limit    int64
	status   int
	overflow bool
}

func (w *pendingSegment) Header() http.Header { return w.headers }
func (w *pendingSegment) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *pendingSegment) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if int64(len(p)) > w.limit-int64(w.body.Len()) {
		w.overflow = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(p)
}

func (s *recoverySession) ServePlaylist(w http.ResponseWriter, q *http.Request) {
	out, err := s.resolve(q.Context(), -1)
	if err != nil {
		http.Error(w, "encoder recovery pending", 503)
		return
	}
	guarded := s.r.fencedDelivery(w, q, out.route)
	defer guarded.Close()
	out.ServePlaylist(guarded, q)
}

func (w *pendingSegment) complete(method string) bool {
	if method == http.MethodHead || w.status == http.StatusNotModified || w.status >= 400 {
		return true
	}
	length, err := strconv.ParseInt(w.headers.Get("Content-Length"), 10, 64)
	return err == nil && length >= 0 && length == int64(w.body.Len())
}

func (s *recoverySession) current() *bufferedSession {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if x := s.r.executions[s.route.Fence.Session]; x != nil {
		return x.output
	}
	return nil
}
func (s *recoverySession) LastAccess() time.Time {
	if out := s.current(); out != nil {
		return out.LastAccess()
	}
	return s.bufferedSession.LastAccess()
}
func (s *recoverySession) ServeRungPlaylist(w http.ResponseWriter, q *http.Request, rung string) {
	out, err := s.resolve(q.Context(), -1)
	if err != nil {
		http.Error(w, "encoder recovery pending", 503)
		return
	}
	guarded := s.r.fencedDelivery(w, q, out.route)
	defer guarded.Close()
	out.Session.ServeRungPlaylist(guarded, q, rung)
}
func (s *recoverySession) Rungs() []core.HLSRung {
	if out := s.current(); out != nil {
		return out.Rungs()
	}
	return nil
}
func (s *recoverySession) IndependentSegments() bool {
	if out := s.current(); out != nil {
		return out.IndependentSegments()
	}
	return false
}
func (s *recoverySession) Touch() {
	if out := s.current(); out != nil {
		out.Touch()
	}
}
func (s *recoverySession) PlaybackState(state string) {
	if out := s.current(); out != nil {
		out.PlaybackState(state)
	}
}
func (s *recoverySession) PlaybackDelivered() bool {
	if s.Session.PlaybackDelivered() {
		return true
	}
	if out := s.current(); out != nil {
		return out.PlaybackDelivered()
	}
	return false
}
func (s *recoverySession) EncoderRetirement() core.EncoderRetirement {
	if token, ok := s.r.EncoderRetirement(s.route.Fence.Session); ok {
		return token
	}
	return s
}
func (s *recoverySession) EncoderDone() <-chan struct{} {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if x := s.r.executions[s.route.Fence.Session]; x != nil {
		return x.done
	}
	return s.Session.EncoderDone()
}
func (s *recoverySession) EncoderErr() error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if x := s.r.executions[s.route.Fence.Session]; x != nil && x.retired {
		return nil
	}
	return core.ErrEncoderPending
}
func (s *recoverySession) Close() error { return s.RetireEncoder() }
func (s *recoverySession) RetireEncoder() error {
	s.r.Stop(s.route.Fence.Session)
	return s.EncoderErr()
}
func (s *recoverySession) RetireEncoderContext(ctx context.Context) error {
	s.r.stop(ctx, s.route.Fence.Session)
	return s.EncoderErr()
}

func (s *recoverySession) Observe(ctx context.Context) (*remotetranscode.ObservedSession, error) {
	return s.r.ObserveExecution(ctx, s.route.Fence.Session, time.Minute)
}
