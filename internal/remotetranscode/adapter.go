package remotetranscode

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodewire/outbound"
)

type Assignment struct {
	Identity Identity
	Grant    string
	Expires  time.Time
}
type AdapterOptions struct {
	// CommitStart durably binds the installed immutable Start before any Start
	// bytes leave this adapter. It runs synchronously outside locks, with a
	// context bounded to five seconds; implementations must honor cancellation.
	// Failure retains the owned token and never establishes retirement. Nil
	// preserves existing behavior. Only identity and hash cross this boundary.
	CommitStart  func(context.Context, Identity, [32]byte) error
	Capabilities Capabilities // Trusted negotiated worker/backend contracts; zero is fixed HLS.
	Endpoint     string
	// RecheckServer composes current Guard policy for the expected worker identity.
	// Fresh TLS per operation; rebuild on rotation. Callback captures/opaque keys
	// are immutable, callbacks concurrency safe and context cooperative.
	RecheckServer func(context.Context, tls.ConnectionState) error
	Client        *http.Client // Transport must be *http.Transport with verified mutual TLS.
	Assign        func(context.Context, core.HLSRequest) (Assignment, error)
	Decide        func(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision
	Hardware      core.HWAccel
	Capacity      int
}
type Adapter struct {
	opts     AdapterOptions
	client   *http.Client
	mu       sync.Mutex
	sessions map[Identity]*Session
}

func NewAdapter(o AdapterOptions) (*Adapter, error) {
	caps, err := NormalizeCapabilities(o.Capabilities)
	if err != nil {
		return nil, err
	}
	o.Capabilities = caps
	u, e := url.Parse(o.Endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || o.Client == nil || o.RecheckServer == nil || o.Assign == nil || o.Decide == nil || o.Capacity < 1 {
		return nil, errors.New("remotetranscode: explicit adapter dependencies required")
	}
	transport, ok := o.Client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return nil, errors.New("remotetranscode: mutual TLS transport required")
	}
	tc := transport.TLSClientConfig
	if tc.InsecureSkipVerify || tc.RootCAs == nil || tc.VerifyConnection == nil || (len(tc.Certificates) == 0 && tc.GetClientCertificate == nil) {
		return nil, errors.New("remotetranscode: verified mutual TLS required")
	}
	// A new transport prevents caller dial/proxy/HTTP2 hooks from bypassing admission.
	timeout := o.Client.Timeout
	if timeout <= 0 || timeout > time.Minute {
		timeout = time.Minute
	}
	t, err := outbound.New(o.Endpoint, tc, timeout, o.RecheckServer)
	if err != nil {
		return nil, errors.New("remotetranscode: invalid transport")
	}
	client := *o.Client
	client.Transport = t
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Adapter{opts: o, client: &client, sessions: make(map[Identity]*Session)}, nil
}
func (a *Adapter) Capabilities() Capabilities                             { return a.opts.Capabilities }
func (a *Adapter) Probe(context.Context, string) (*core.MediaInfo, error) { return nil, ErrUnsupported }
func (a *Adapter) Decide(i core.MediaInfo, c core.ClientCaps, o core.DecideOptions) core.Decision {
	return a.opts.Decide(i, c, o)
}
func (a *Adapter) HWAccel() core.HWAccel { return a.opts.Hardware }
func (a *Adapter) StartHLS(ctx context.Context, r core.HLSRequest) (core.HLSSession, error) {
	s, _, e := a.StartHLSOwned(ctx, r)
	return s, e
}
func (a *Adapter) StartHLSOwned(ctx context.Context, r core.HLSRequest) (core.HLSSession, core.EncoderRetirement, error) {
	r.RecoveryStart = core.CloneRecoveryStart(r.RecoveryStart)
	if !a.SupportsRequest(r) || !token(r.SessionID) {
		return nil, nil, ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	assignmentRequest := r
	assignmentRequest.RecoveryStart = core.CloneRecoveryStart(r.RecoveryStart)
	assignment, err := a.opts.Assign(ctx, assignmentRequest)
	if err != nil {
		return nil, nil, err
	}
	if !validIdentity(assignment.Identity) || assignment.Identity.Session != r.SessionID || len(assignment.Grant) == 0 || len(assignment.Grant) > 4096 || !assignment.Expires.After(time.Now()) {
		return nil, nil, errors.New("remotetranscode: invalid assignment")
	}
	start := Start{RecoveryStart: core.CloneRecoveryStart(r.RecoveryStart), Keyframes: append([]core.KeyframePoint(nil), r.Info.Keyframes...), SoftSubtitleStream: r.SoftSubtitleStream, Protocol: r.Protocol, ReleaseID: r.ReleaseID, Identity: assignment.Identity, Grant: assignment.Grant, Expires: assignment.Expires, Info: r.Info, Decision: r.Decision, Offset: r.StartOffset, SegmentDuration: r.SegmentDuration, IndependentProof: r.RequireIndependentProof}
	b, err := json.Marshal(start)
	if err != nil || len(b) > MaxMessage-1024 {
		return nil, nil, ErrUnsupported
	}
	a.mu.Lock()
	s := a.sessions[assignment.Identity]
	if s != nil && !bytes.Equal(s.start, b) {
		a.mu.Unlock()
		return nil, nil, errors.New("remotetranscode: assignment conflict")
	}
	if s == nil {
		if len(a.sessions) >= a.opts.Capacity {
			a.mu.Unlock()
			return nil, nil, errors.New("remotetranscode: adapter capacity")
		}
		s = &Session{adapter: a, id: assignment.Identity, start: b, done: make(chan struct{})}
		a.sessions[s.id] = s
	}
	a.mu.Unlock()
	if a.opts.CommitStart != nil {
		commitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		identity, hash, commitmentErr := s.StartCommitment()
		if commitmentErr == nil {
			commitmentErr = a.opts.CommitStart(commitCtx, identity, hash)
		}
		if commitmentErr == nil {
			commitmentErr = commitCtx.Err()
		}
		cancel()
		if commitmentErr != nil || ctx.Err() != nil {
			return nil, s, ErrUncertain
		}
	}
	// The payload comes only from the immutable bytes installed in Session,
	// never from request pointers still reachable by a caller or callback.
	payload, err := json.Marshal(struct {
		Version  int
		Op       string
		Identity Identity
		Start    json.RawMessage
	}{Version: Version, Op: "start", Identity: s.id, Start: json.RawMessage(s.start)})
	if err != nil {
		return nil, s, ErrUncertain
	}
	response, err := a.callPayload(ctx, payload, nil, "start")
	if err != nil {
		return nil, s, ErrUncertain
	}
	defer response.Body.Close()
	if s.observe(response) != nil {
		return nil, s, ErrUncertain
	}
	s.mu.Lock()
	running := s.state == "running"
	s.mu.Unlock()
	if !running {
		return nil, s, ErrUncertain
	}
	s.touch()
	return s, s, nil
}
func (a *Adapter) Session(id string) (core.HLSSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var newest *Session
	for _, s := range a.sessions {
		if s.id.Session == id && (newest == nil || s.id.Generation > newest.id.Generation) {
			newest = s
		}
	}
	if newest == nil {
		return nil, false
	}
	newest.mu.Lock()
	defer newest.mu.Unlock()
	return newest, newest.state == "running" && !newest.stopped
}
func (a *Adapter) Stop(id string) {
	a.mu.Lock()
	var latest *Session
	for _, s := range a.sessions {
		if s.id.Session == id && (latest == nil || s.id.Generation > latest.id.Generation) {
			latest = s
		}
	}
	a.mu.Unlock()
	if latest != nil {
		_ = latest.RetireEncoder()
	}
}

// Close closes idle transport connections only; owned executions must be stopped
// and reconciled explicitly. A connection close never proves encoder retirement.
func (a *Adapter) Close() { a.client.CloseIdleConnections() }
func (a *Adapter) call(ctx context.Context, m message, headers http.Header) (*http.Response, error) {
	m.Version = Version
	b, err := json.Marshal(m)
	if err != nil || len(b) > MaxMessage {
		return nil, ErrUnsupported
	}
	return a.callPayload(ctx, b, headers, m.Op)
}
func (a *Adapter) callPayload(ctx context.Context, b []byte, headers http.Header, op string) (*http.Response, error) {
	if len(b) > MaxMessage {
		return nil, ErrUnsupported
	}
	req, err := http.NewRequestWithContext(ctx, "POST", a.opts.Endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if v := headers.Values(h); len(v) > 0 {
			req.Header[h] = v
		}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, ErrUncertain
	}
	if !mediaOperation(op) && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		resp.Body.Close()
		if (op == "renew" || op == "inspect") && resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, ErrUnsupported
		}
		return nil, ErrUncertain
	}
	return resp, nil
}

type Session struct {
	adapter     *Adapter
	id          Identity
	start       []byte
	mu          sync.Mutex
	done        chan struct{}
	lease       LeaseState
	state       string
	terminal    error
	stopped     bool
	access      atomic.Int64
	delivered   atomic.Bool
	rungs       []core.HLSRung
	independent bool
}

func (s *Session) ID() string                                { return s.id.Session }
func (s *Session) LastAccess() time.Time                     { return time.Unix(0, s.access.Load()) }
func (s *Session) touch()                                    { s.access.Store(time.Now().UnixNano()) }
func (s *Session) EncoderRetirement() core.EncoderRetirement { return s }
func (s *Session) EncoderDone() <-chan struct{}              { return s.done }
func (s *Session) EncoderErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return s.terminal
	default:
		return core.ErrEncoderPending
	}
}
func (s *Session) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return ErrUnsupported
}
func (s *Session) Close() error { return s.RetireEncoder() }
func (s *Session) RetireEncoder() error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, e := s.adapter.call(ctx, message{Op: "stop", Identity: s.id}, nil)
	if e != nil {
		return ErrUncertain
	}
	defer r.Body.Close()
	if e = s.observe(r); e != nil {
		return e
	}
	return s.EncoderErr()
}

// Reconcile obtains proof for this exact execution after a failed/late start,
// disconnect or expiry. No polling goroutine or timeout manufactures proof.
func (s *Session) Reconcile(ctx context.Context) error {
	r, e := s.adapter.call(ctx, message{Op: "status", Identity: s.id}, nil)
	if e != nil {
		return e
	}
	defer r.Body.Close()
	if e = s.observe(r); e != nil {
		return e
	}
	return s.EncoderErr()
}
func (s *Session) observe(r *http.Response) error {
	var st status
	d := json.NewDecoder(io.LimitReader(r.Body, MaxMessage+1))
	if d.Decode(&st) != nil || d.Decode(new(any)) != io.EOF {
		return ErrUncertain
	}
	if st.Identity != s.id {
		return ErrUncertain
	}
	if st.State != "running" && st.State != "pending" && st.State != "retired" && st.State != "unverified" {
		return ErrUncertain
	}
	if !validRungs(st.Rungs) {
		return ErrUncertain
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.independent = st.IndependentSegments
	if st.Rungs != nil {
		s.rungs = append([]core.HLSRung(nil), st.Rungs...)
	}
	// Do not regress accepted lease history on a delayed start/status response.
	if st.Lease.Version > 0 {
		n := st.Lease.Accepted
		if n == nil || !validRenewal(*n) || n.Identity != s.id || n.Operation != st.Lease.Version || !n.Deadline.Equal(st.Lease.Deadline) {
			return ErrUncertain
		}
	}
	if st.Lease.Version == s.lease.Version && !s.lease.Deadline.IsZero() && !st.Lease.Deadline.Equal(s.lease.Deadline) {
		return ErrUncertain
	}
	if st.Lease.Version == s.lease.Version && s.lease.Accepted != nil && (st.Lease.Accepted == nil || !sameRenewal(*st.Lease.Accepted, *s.lease.Accepted)) {
		return ErrUncertain
	}
	if st.Lease.Version >= s.lease.Version {
		s.lease = st.Lease
	}
	select {
	case <-s.done:
		return nil
	default:
	}
	s.stopped = s.stopped || st.Stopped
	s.state = st.State
	if st.State == "retired" || st.State == "unverified" {
		if st.State == "unverified" {
			s.terminal = core.ErrEncoderInputUnverified
		}
		close(s.done)
	}
	return nil
}
func (s *Session) ServePlaylist(w http.ResponseWriter, r *http.Request) { s.serve(w, r, "playlist", 0) }
func (s *Session) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	s.serve(w, r, "segment", n)
}
func (s *Session) serve(w http.ResponseWriter, r *http.Request, op string, n int) {
	s.serveRepresentation(w, r, message{Op: op, Segment: n})
}
func (s *Session) serveRepresentation(w http.ResponseWriter, r *http.Request, m message) (facts RepresentationCompletion) {
	n := m.Segment
	if n < -1 || n > 10000000 {
		http.Error(w, "invalid segment", 400)
		return
	}
	s.mu.Lock()
	blocked := s.stopped
	s.mu.Unlock()
	if blocked {
		http.Error(w, "execution stopped", 410)
		return
	}
	m.Identity, m.Head = s.id, r.Method == http.MethodHead
	response, err := s.adapter.call(r.Context(), m, r.Header)
	if err != nil {
		http.Error(w, "remote execution unavailable", 502)
		return
	}
	facts.StatusCode, facts.ContentLength = response.StatusCode, response.ContentLength
	facts.BodyPresent = response.Body != nil
	facts.EncodedOrChunked = len(response.TransferEncoding) != 0 || response.Uncompressed || response.Header.Get("Content-Encoding") != ""
	defer func() {
		if response.Body != nil {
			facts.CloseError = response.Body.Close() != nil
			facts.Closed = true
		}
		facts.Trailers = len(response.Trailer) != 0
	}()
	s.touch()
	// Preserve representation/range/cache headers, never cookies or peer secrets.
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Cache-Control", "ETag", "Last-Modified", "Content-Disposition"} {
		if v := response.Header.Values(h); len(v) > 0 {
			w.Header()[h] = v
		}
	}
	w.WriteHeader(response.StatusCode)
	// Track client output at this controller too; worker input reads and
	// playlists/init records are not proof that remote media was delivered.
	media := m.Op == "http" || m.Op == "media" || ((m.Op == "segment" || m.Op == "rung-segment") && m.Segment >= 0)
	if media && response.StatusCode >= 200 && response.StatusCode < 300 && r.Method == http.MethodGet {
		w = deliveryWriter{ResponseWriter: w, session: s}
	}
	// io.Copy applies downstream backpressure; request context cancels upstream.
	if r.Method != http.MethodHead {
		var target io.Writer = w
		if m.Op == "http" || m.Op == "soft-subtitle" {
			target = flushingWriter{w}
		}
		if response.Body != nil {
			_, err := io.Copy(completionWriter{target: target, facts: &facts}, completionReader{source: response.Body, facts: &facts})
			if err != nil && !facts.WriteError {
				facts.ReadError = true
			}
		}
	}
	return
}

var _ core.Transcoder = (*Adapter)(nil)
var _ core.OwnedTranscoder = (*Adapter)(nil)
var _ core.OwnedEncoderSession = (*Session)(nil)

// Lease reports accepted history, not proof that execution remains authorized.
// Inspect execution/retirement state and reconcile uncertainty independently.
func (s *Session) Lease() LeaseState {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := s.lease
	if lease.Accepted != nil {
		copy := *lease.Accepted
		lease.Accepted = &copy
	}
	return lease
}

// Renew is parent-driven; there are no automatic renewal/polling goroutines.
// Reuse the identical request after a lost response, or Reconcile then inspect
// Lease. Older operations after a newer acceptance return ErrUncertain; they
// never extend authority. A missing worker validator returns ErrUnsupported.
func (s *Session) Renew(ctx context.Context, n Renewal) (LeaseState, error) {
	if n.Identity != s.id || !validRenewal(n) {
		return s.Lease(), ErrUnsupported
	}
	s.mu.Lock()
	blocked := s.stopped
	s.mu.Unlock()
	if blocked {
		return s.Lease(), ErrUncertain
	}
	r, err := s.adapter.call(ctx, message{Op: "renew", Identity: s.id, Renewal: &n}, nil)
	if err != nil {
		return s.Lease(), err
	}
	defer r.Body.Close()
	if err = s.observe(r); err != nil {
		return s.Lease(), err
	}
	lease := s.Lease()
	s.mu.Lock()
	running := s.state == "running" && !s.stopped
	s.mu.Unlock()
	if !running || lease.Accepted == nil || !sameRenewal(*lease.Accepted, n) {
		return lease, ErrUncertain
	}
	return lease, nil
}

// PlaybackDelivered reports real media writes through this remote execution.
func (s *Session) PlaybackDelivered() bool { return s.delivered.Load() }

type deliveryWriter struct {
	http.ResponseWriter
	session *Session
}

func (w deliveryWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.session.delivered.Store(true)
	}
	return n, err
}
func (w deliveryWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
