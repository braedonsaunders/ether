package remotetranscode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type WorkerOptions struct {
	Capabilities    Capabilities    // Explicit backend contract; zero retains fixed HLS.
	ValidateRenewal ValidateRenewal // Nil explicitly disables renewal.
	Boot            string
	Capacity        int // Includes all retained tombstones; never evicts unresolved work.
	MaxLease        time.Duration
	Security        Security
	Resolve         ResolveSource
	Encoder         core.OwnedTranscoder
}
type execution struct {
	acceptedStart             *AcceptedStartHandle
	origin                    EvidenceOrigin
	negativeBarrierGeneration uint64
	backendInvoked            bool
	validatingRenewals        uint32
	responsibilityVersion     uint8
	start                     Start // Immutable original grant and expiry, separate from current lease.
	lease                     LeaseState
	id                        Identity
	hash                      [32]byte
	session                   core.OwnedEncoderSession
	retirement                core.EncoderRetirement
	rungs                     []core.HLSRung
	independent               bool
	input                     core.EncoderInputLease
	gatewayInput              core.EncoderInputLease
	starting, stopped, failed bool
	cancel                    context.CancelFunc
	timer                     *time.Timer
	monotonicDeadline         time.Time // Local elapsed-time cap; never serialized.
}
type Worker struct {
	mu      sync.Mutex
	opts    WorkerOptions
	records map[Identity]*execution
	highest map[string]uint64
	closed  bool
}

func NewWorker(o WorkerOptions) (*Worker, error) {
	caps, err := NormalizeCapabilities(o.Capabilities)
	if err != nil {
		return nil, err
	}
	o.Capabilities = caps
	if !token(o.Boot) || o.Capacity < 1 || o.MaxLease <= 0 || o.MaxLease > 10*time.Minute || o.Security.Authenticate == nil || o.Security.Authorize == nil || o.Resolve == nil || o.Encoder == nil {
		return nil, errors.New("remotetranscode: explicit worker dependencies required")
	}
	return &Worker{opts: o, records: make(map[Identity]*execution), highest: make(map[string]uint64)}, nil
}

// Close stops admission and requests retirement of every exact execution. It does
// not claim completion; callers must reconcile pending records before exit.
func (w *Worker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for _, e := range w.records {
		w.stop(e)
	}
	return nil
}

// stop is called under mu. OwnedEncoderSession.RetireEncoder is bounded by contract.
func (w *Worker) stop(e *execution) {
	e.stopped = true
	if e.timer != nil {
		e.timer.Stop()
	}
	if e.cancel != nil {
		e.cancel()
	}
	if e.input != nil {
		e.input.SealInput()
	}
	if e.gatewayInput != nil {
		e.gatewayInput.SealInput()
	}
	if e.session != nil {
		_ = e.session.RetireEncoder()
	}
}
func result(e *execution) string {
	if e.starting {
		return "pending"
	}
	if e.retirement != nil {
		select {
		case <-e.retirement.EncoderDone():
			if e.retirement.EncoderErr() != nil {
				return "unverified"
			}
		default:
			if !e.stopped && !e.failed {
				return "running"
			}
			return "pending"
		}
	} else if !e.stopped {
		return "pending"
	}
	for _, input := range []core.EncoderInputLease{e.input, e.gatewayInput} {
		if input != nil {
			select {
			case <-input.InputDone():
				if input.InputErr() != nil {
					return "unverified"
				}
			default:
				return "pending"
			}
		}
	}
	return "retired"
}
func (w *Worker) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	// TLS verification is mandatory even when an injected callback is permissive.
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(out, "peer authentication required", 401)
		return
	}
	peer, err := w.opts.Security.Authenticate(r)
	if err != nil || !token(peer) {
		http.Error(out, "peer authentication required", 401)
		return
	}
	if r.Method != "POST" {
		http.Error(out, "method", 405)
		return
	}
	var m message
	dec := json.NewDecoder(http.MaxBytesReader(out, r.Body, MaxMessage))
	dec.DisallowUnknownFields()
	if dec.Decode(&m) != nil || dec.Decode(new(any)) != io.EOF || m.Version != Version || !validIdentity(m.Identity) || m.Identity.Worker != w.opts.Boot || peer != m.Identity.Controller {
		http.Error(out, "invalid request", 400)
		return
	}
	if m.Op != "renew" && m.Op != "start" && m.Op != "stop" && m.Op != "status" && m.Op != "touch" && m.Op != "playback-state" && m.Op != "inspect" && !mediaOperation(m.Op) {
		http.Error(out, "unsupported operation", 422)
		return
	}
	if m.Op == "start" {
		if m.Start == nil || len(m.Start.ReleaseID) > 512 || m.Start.Identity != m.Identity || len(m.Start.Grant) == 0 || len(m.Start.Grant) > 4096 || !SupportsRequest(core.HLSRequest{RecoveryStart: core.CloneRecoveryStart(m.Start.RecoveryStart), SoftSubtitleStream: m.Start.SoftSubtitleStream, Protocol: m.Start.Protocol, ReleaseID: m.Start.ReleaseID, Info: startInfo(*m.Start), Decision: m.Start.Decision, StartOffset: m.Start.Offset, SegmentDuration: m.Start.SegmentDuration}, w.opts.Capabilities) {
			http.Error(out, "unsupported start", 422)
			return
		}
	} else if m.Start != nil {
		http.Error(out, "invalid request", 400)
		return
	}
	if m.Op == "renew" {
		if m.Renewal == nil || m.Renewal.Identity != m.Identity || !validRenewal(*m.Renewal) {
			http.Error(out, "invalid renewal", 400)
			return
		}
	} else if m.Renewal != nil {
		http.Error(out, "invalid request", 400)
		return
	}
	if (m.Op == "soft-subtitle" && m.Head) || (m.Op == "playback-state" && m.State != "playing" && m.State != "paused" && m.State != "stopped") || m.Subtitle < 0 || m.Subtitle > 100000 || m.Segment < -1 || m.Segment > 10000000 || m.Representation < 0 || m.Representation > 63 || (m.Rung != "" && !token(m.Rung)) {
		http.Error(out, "invalid segment", 400)
		return
	}
	authorizationStart := m.Start
	if m.Start != nil && m.Start.RecoveryStart != nil {
		copyStart := *m.Start
		copyStart.RecoveryStart = core.CloneRecoveryStart(m.Start.RecoveryStart)
		authorizationStart = &copyStart
	}
	if w.opts.Security.Authorize(r.Context(), peer, m.Op, m.Identity, authorizationStart) != nil {
		http.Error(out, "operation denied", 403)
		return
	}
	if m.Op == "renew" {
		w.renew(out, r, peer, *m.Renewal)
		return
	}
	if m.Op == "start" {

		w.start(out, r, peer, *m.Start)
		return
	}
	w.mu.Lock()
	e := w.records[m.Identity]
	if e == nil && m.Op == "stop" && len(w.records) < w.opts.Capacity && !w.closed {
		// Stop-before-start is a permanent tombstone for this generation.
		e = &execution{id: m.Identity, stopped: true, origin: EvidenceStopBeforeStart, negativeBarrierGeneration: m.Identity.Generation}
		w.records[m.Identity] = e
		key := m.Identity.Controller + "/" + m.Identity.Session
		if m.Identity.Generation > w.highest[key] {
			w.highest[key] = m.Identity.Generation
		}
	}
	if e == nil {
		w.mu.Unlock()
		http.Error(out, "unknown execution", 409)
		return
	}
	w.expire(e)
	if m.Op == "stop" {
		w.stop(e)
	}
	state := result(e)
	s := e.session
	if m.Op == "status" || m.Op == "stop" {
		snapshot := w.snapshot(e)
		w.mu.Unlock()
		replyStatus(out, snapshot)
		return
	}
	if e.stopped || state != "running" || s == nil {
		w.mu.Unlock()
		http.Error(out, "execution unavailable", 409)
		return
	}
	w.mu.Unlock()
	if m.Op == "inspect" {
		w.inspect(out, r, e, s)
		return
	}
	// Only these request headers reach the local session, never caller credentials.
	request := r.Clone(r.Context())
	request.Method = "GET"
	if m.Head {
		request.Method = "HEAD"
	}
	request.Header = make(http.Header)
	for _, h := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		request.Header[h] = r.Header.Values(h)
	}
	if m.Op == "touch" || m.Op == "playback-state" {
		if !w.opts.Capabilities.Controls {
			http.Error(out, "controls unsupported", 422)
			return
		}
		if m.Op == "touch" {
			if v, ok := s.(interface{ Touch() }); ok {
				v.Touch()
			} else {
				http.Error(out, "control unsupported", 422)
				return
			}
		} else {
			if v, ok := s.(interface{ PlaybackState(string) }); ok {
				v.PlaybackState(m.State)
			} else {
				http.Error(out, "control unsupported", 422)
				return
			}
		}
		w.mu.Lock()
		snapshot := w.snapshot(e)
		w.mu.Unlock()
		replyStatus(out, snapshot)
		return
	}
	w.serveMedia(out, request, m, s, e.start.Protocol, e.rungs)
}
func reply(out http.ResponseWriter, state string, id Identity) {
	out.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(out).Encode(status{State: state, Identity: id})
}
func (w *Worker) start(out http.ResponseWriter, r *http.Request, peer string, s Start) {
	s.RecoveryStart = core.CloneRecoveryStart(s.RecoveryStart)
	encoded, _ := json.Marshal(s)
	hash := sha256.Sum256(encoded)
	w.mu.Lock()
	if e := w.records[s.Identity]; e != nil {
		w.expire(e)
		snapshot := w.snapshot(e)
		same := e.hash == hash && !e.stopped
		w.mu.Unlock()
		if !same {
			http.Error(out, "generation conflict", 409)
			return
		}
		replyStatus(out, snapshot)
		return
	}
	// Preserve immutable duplicate replay before rechecking current backend policy.
	// The checker runs without the worker mutex and before source/admission work.
	w.mu.Unlock()
	if backend, ok := w.opts.Encoder.(interface{ SupportsDistributedRequest(core.HLSRequest) bool }); ok {
		var planStart Start
		_ = json.Unmarshal(encoded, &planStart)
		plan := core.HLSRequest{RecoveryStart: core.CloneRecoveryStart(planStart.RecoveryStart), ReleaseID: planStart.ReleaseID, Info: startInfo(planStart), Decision: planStart.Decision, StartOffset: planStart.Offset, SegmentDuration: planStart.SegmentDuration, Protocol: planStart.Protocol, SoftSubtitleStream: planStart.SoftSubtitleStream, RequireIndependentProof: planStart.IndependentProof}
		if !backend.SupportsDistributedRequest(plan) {
			http.Error(out, "backend plan unsupported", 422)
			return
		}
	}
	w.mu.Lock()
	if w.records[s.Identity] != nil {
		w.mu.Unlock()
		w.start(out, r, peer, s)
		return
	}
	key := s.Identity.Controller + "/" + s.Identity.Session
	if w.closed || len(w.records) >= w.opts.Capacity || s.Identity.Generation <= w.highest[key] || !s.Expires.After(time.Now()) || time.Until(s.Expires) > w.opts.MaxLease {
		w.mu.Unlock()
		http.Error(out, "admission denied", 409)
		return
	}
	// A replacement cannot overlap any unresolved prior encoder for this session.
	for _, old := range w.records {
		if old.id.Controller == s.Identity.Controller && old.id.Session == s.Identity.Session && result(old) != "retired" {
			w.mu.Unlock()
			http.Error(out, "prior execution unresolved", 409)
			return
		}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	e := &execution{origin: EvidenceAcceptedStart, start: s, lease: LeaseState{Deadline: s.Expires.UTC(), Renewable: w.opts.ValidateRenewal != nil}, id: s.Identity, hash: hash, starting: true, cancel: cancel}
	e.acceptedStart = w.newAcceptedStart(e)
	w.records[s.Identity] = e
	w.highest[key] = s.Identity.Generation
	w.setMonotonicDeadline(e)
	w.arm(e)
	w.mu.Unlock()
	// Request cancellation seals a late start, but never forgets its responsibility.
	disconnect := context.AfterFunc(r.Context(), func() { w.mu.Lock(); defer w.mu.Unlock(); w.stop(e) })
	defer disconnect()
	var resolverStart Start
	_ = json.Unmarshal(encoded, &resolverStart) // Independent copy: trusted resolver cannot mutate immutable start.
	source, err := w.opts.Resolve(ctx, peer, resolverStart)
	var session core.HLSSession
	var retirement core.EncoderRetirement
	invoked := false
	var gatewayLease core.EncoderInputLease
	if source.Gateway != nil {
		// Outside worker mutex. Preserve responsibility even when Resolve or the
		// gateway fails, or when a resolver violates the mutually exclusive contract.
		raw, lease, gatewayErr := source.Gateway.GatewayInput(s.Identity, s.Grant)
		gatewayLease = lease
		if err == nil && gatewayErr == nil && source.URL == "" && source.Lease == nil && gatewayURL(raw) && lease != nil {
			source.URL = raw
		} else {
			err = errors.New("invalid trusted gateway input")
		}
	} else if err == nil && (!privateURL(source.URL) || source.Lease == nil) {
		err = errors.New("invalid authorized source")
	}
	w.mu.Lock()
	e.input = source.Lease
	e.gatewayInput = gatewayLease
	e.responsibilityVersion++
	input := source.Lease
	if source.Gateway != nil {
		input = gatewayLease
	}
	w.expire(e)
	stopped := e.stopped
	w.mu.Unlock()
	if stopped {
		err = context.Canceled
	}
	if err == nil {
		w.mu.Lock()
		e.backendInvoked = true
		w.mu.Unlock()
		invoked = true
		var backendStart Start
		_ = json.Unmarshal(encoded, &backendStart)
		// Backend receives separate nested slices/pointers; source renewal and duplicate
		// starts continue comparing the original immutable payload.
		backendInfo, backendDecision, backendSoft := startInfo(backendStart), backendStart.Decision, backendStart.SoftSubtitleStream
		session, retirement, err = w.opts.Encoder.StartHLSOwned(ctx, core.HLSRequest{RecoveryStart: core.CloneRecoveryStart(backendStart.RecoveryStart), ReleaseID: s.ReleaseID, SessionID: executionID(s.Identity), ManifestSessionID: s.Identity.Session, InputURL: source.URL, InputLease: input, Info: backendInfo, Decision: backendDecision, StartOffset: s.Offset, SegmentDuration: s.SegmentDuration, RequireIndependentProof: s.IndependentProof, SoftSubtitleStream: backendSoft, Protocol: s.Protocol})
	}
	if err == nil && session != nil {
		if w.opts.Capabilities.Controls {
			if _, ok := session.(interface{ Touch() }); !ok {
				err = ErrUnsupported
			}
			if s.Protocol != core.ProtocolHTTP {
				if _, ok := session.(interface{ PlaybackState(string) }); !ok {
					err = ErrUnsupported
				}
			}
		}
		if s.SoftSubtitleStream != nil {
			if _, ok := session.(interface {
				ServeSoftSubtitle(http.ResponseWriter, *http.Request, int) bool
			}); !ok {
				err = ErrUnsupported
			}
		}
	}
	independent := false
	if proof, ok := session.(core.HLSIndependentSegments); ok && err == nil {
		independent = proof.IndependentSegments()
	}
	var rungs []core.HLSRung
	if err == nil && session != nil {
		if w.opts.Capabilities.ABR && s.Protocol != core.ProtocolHTTP {
			ladder, ok := session.(core.HLSLadderSession)
			if !ok {
				err = ErrUnsupported
			} else {
				rungs = ladder.Rungs()
				if len(rungs) == 0 || !validRungs(rungs) {
					rungs = nil
					err = ErrUnsupported
				}
			}
		}
		if s.Protocol == core.ProtocolDASH {
			if _, ok := session.(core.DASHSession); !ok {
				err = ErrUnsupported
			}
		}
		if s.Protocol == core.ProtocolHTTP {
			if _, ok := session.(core.HTTPSession); !ok {
				err = ErrUnsupported
			}
		}
	}
	w.mu.Lock()
	w.expire(e)
	e.rungs = append([]core.HLSRung(nil), rungs...)
	e.independent = independent
	e.starting = false
	e.retirement = retirement
	e.responsibilityVersion++
	if session != nil {
		e.session, _ = session.(core.OwnedEncoderSession)
		if e.session == nil {
			_ = session.Close()
			err = ErrUncertain
		}
	}
	if retirement == nil && invoked {
		e.failed = true
		e.starting = true /* no proof: permanently pending */
	}
	if err != nil || e.stopped || ctx.Err() != nil {
		e.failed = true
		w.stop(e)
	}
	if err == nil && e.session == nil {
		e.failed = true
		w.stop(e)
	}
	failed, snapshot := e.failed, w.snapshot(e)
	w.mu.Unlock()
	if failed {
		http.Error(out, "start failed; reconcile execution", 409)
		return
	}
	replyStatus(out, snapshot)
}
func executionID(i Identity) string {
	b, _ := json.Marshal(i)
	h := sha256.Sum256(b)
	const hex = "0123456789abcdef"
	out := make([]byte, 64)
	for j, v := range h {
		out[j*2] = hex[v>>4]
		out[j*2+1] = hex[v&15]
	}
	return string(out)
}

// All lease transitions and deadline checks run under mu. Timer.Stop alone is
// insufficient: an already queued old callback must compare the accepted lease.
func (w *Worker) expire(e *execution) {
	if w.expired(e, time.Now()) {
		w.stop(e)
	}
}
func (w *Worker) expired(e *execution, now time.Time) bool {
	return (!e.lease.Deadline.IsZero() && !now.Before(e.lease.Deadline)) ||
		(!e.monotonicDeadline.IsZero() && !now.Before(e.monotonicDeadline))
}
func (w *Worker) setMonotonicDeadline(e *execution) {
	now := time.Now()
	// Add retains now's monotonic reading; wall-clock rollback cannot extend this
	// accepted execution's elapsed lifetime. A renewed lease gets a new bound.
	e.monotonicDeadline = now.Add(e.lease.Deadline.Sub(now))
}
func (w *Worker) arm(e *execution) {
	if e.timer != nil {
		e.timer.Stop()
	}
	version, deadline, cap := e.lease.Version, e.lease.Deadline, e.monotonicDeadline
	wait := time.Until(deadline)
	if !e.monotonicDeadline.IsZero() {
		wait = min(wait, time.Until(e.monotonicDeadline))
	}
	e.timer = time.AfterFunc(wait, func() { w.expireVersion(e, version, deadline, cap) })
}
func (w *Worker) expireVersion(e *execution, version uint64, deadline, cap time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e.lease.Version == version && e.lease.Deadline.Equal(deadline) && e.monotonicDeadline == cap {
		w.expire(e)
		// An early callback must not lose the only timer.
		if !e.stopped {
			w.arm(e)
		}
	}
}
func (w *Worker) snapshot(e *execution) status {
	lease := e.lease
	if lease.Accepted != nil {
		accepted := *lease.Accepted
		lease.Accepted = &accepted
	}
	return status{IndependentSegments: e.independent, Rungs: append([]core.HLSRung(nil), e.rungs...), State: result(e), Identity: e.id, Lease: lease, Stopped: e.stopped}
}
func replyStatus(out http.ResponseWriter, s status) {
	out.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(out).Encode(s)
}
func (w *Worker) renew(out http.ResponseWriter, r *http.Request, peer string, n Renewal) {
	if w.opts.ValidateRenewal == nil {
		http.Error(out, "renewal unsupported", 422)
		return
	}
	w.mu.Lock()
	e := w.records[n.Identity]
	if e == nil {
		w.mu.Unlock()
		http.Error(out, "unknown execution", 409)
		return
	}
	w.expire(e)
	// An exact latest replay reports history even after stop, without new authority.
	if e.lease.Accepted != nil && sameRenewal(*e.lease.Accepted, n) {
		snapshot := w.snapshot(e)
		w.mu.Unlock()
		replyStatus(out, snapshot)
		return
	}
	if !w.canRenew(e, n) {
		w.mu.Unlock()
		http.Error(out, "lease conflict", 409)
		return
	}
	var original Start
	originalBytes, _ := json.Marshal(e.start)
	_ = json.Unmarshal(originalBytes, &original)
	if e.validatingRenewals == ^uint32(0) {
		w.mu.Unlock()
		http.Error(out, "renewal capacity", 409)
		return
	}
	e.validatingRenewals++
	w.mu.Unlock()
	snapshot, code := w.validateAndCommitRenewal(r.Context(), peer, e, original, n)
	if code != 0 {
		message := "lease conflict"
		if code == 403 {
			message = "renewal authority denied"
		}
		http.Error(out, message, code)
		return
	}
	replyStatus(out, snapshot)
}

// The registered validator remains joined through every callback and CAS exit.
// Serialization/network output happens only after deferred accounting releases.
func (w *Worker) validateAndCommitRenewal(ctx context.Context, peer string, e *execution, original Start, n Renewal) (status, int) {
	defer func() { w.mu.Lock(); e.validatingRenewals--; w.mu.Unlock() }()
	receipt, err := w.opts.ValidateRenewal(ctx, peer, original, n)
	if err != nil || !sameRenewal(receipt.Renewal, n) || receipt.PlacementUntil.Before(n.Deadline) || receipt.SourceUntil.Before(n.Deadline) {
		return status{}, 403
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.expire(e)
	// Concurrent duplicate validation may complete after this operation committed.
	if e.lease.Accepted != nil && sameRenewal(*e.lease.Accepted, n) {
		return w.snapshot(e), 0
	}
	if ctx.Err() != nil || !w.canRenew(e, n) {
		return status{}, 409
	}
	accepted := n
	e.lease.Deadline, e.lease.Version, e.lease.Accepted = n.Deadline.UTC(), n.Operation, &accepted
	w.setMonotonicDeadline(e)
	w.arm(e)
	return w.snapshot(e), 0
}
func (w *Worker) canRenew(e *execution, n Renewal) bool {
	now := time.Now()
	return !w.closed && !e.stopped && !e.failed && !e.starting && !w.expired(e, now) && e.lease.Deadline.After(now) && result(e) == "running" &&
		e.lease.Version == n.PreviousVersion && e.lease.Deadline.Equal(n.PreviousDeadline) &&
		n.Deadline.After(now) && n.Deadline.Sub(now) <= w.opts.MaxLease
}
