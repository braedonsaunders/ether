package remotetranscode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAcceptedStartLocalRetainedInvariant(t *testing.T) {
	w, f, start := fixture(t)
	ctx := context.Background()
	checker := &checkingEncoder{fakeEncoder: f, allowed: true}
	w.opts.Encoder = checker
	validations := 0
	w.opts.ValidateRenewal = func(ctx context.Context, peer string, s Start, n Renewal) (RenewalReceipt, error) {
		validations++
		return receipt(ctx, peer, s, n)
	}
	encoded, _ := json.Marshal(start)
	controller := &Session{id: start.Identity, start: encoded}
	_, committedHash, err := controller.StartCommitment()
	if err != nil || committedHash != sha256.Sum256(encoded) {
		t.Fatal("controller commitment", err)
	}
	if h, err := w.AcceptedStart(ctx, start.Identity); err == nil || h != nil || len(w.records) != 0 {
		t.Fatal("controller commitment became worker acceptance")
	}

	// Hold source resolution, then backend return, then response output. Each
	// boundary uses the real authenticated Worker Start path and owned fixture.
	resolveEntered, resolveRelease := make(chan struct{}), make(chan struct{})
	f.entered, f.release = make(chan struct{}), make(chan struct{})
	originalResolve := w.opts.Resolve
	resolves := 0
	w.opts.Resolve = func(ctx context.Context, peer string, s Start) (Source, error) {
		resolves++
		close(resolveEntered)
		<-resolveRelease
		return originalResolve(ctx, peer, s)
	}
	out := &blockedWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	body, _ := json.Marshal(message{Version: Version, Op: "start", Identity: start.Identity, Start: &start})
	r := httptest.NewRequest("POST", "https://worker/private", bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	returned := make(chan struct{})
	var sourceOnce, backendOnce, outputOnce sync.Once
	releaseSource := func() { sourceOnce.Do(func() { close(resolveRelease) }) }
	releaseBackend := func() { backendOnce.Do(func() { close(f.release) }) }
	releaseOutput := func() { outputOnce.Do(func() { close(out.release) }) }
	go func() { defer close(returned); w.ServeHTTP(out, r) }()
	defer func() { releaseSource(); releaseBackend(); releaseOutput(); <-returned }()
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("owned Start did not reach held boundary")
		}
	}
	wait(resolveEntered)
	h, err := w.AcceptedStart(ctx, start.Identity)
	if err != nil {
		t.Fatal("acceptance unavailable before Resolve completed", err)
	}
	inspect := func() AcceptedStartSnapshot {
		t.Helper()
		s, err := w.InspectAcceptedStart(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	initial := inspect()
	if initial.Identity != start.Identity || initial.StartHash != committedHash || initial.AttemptNonce == "" || !initial.Starting || !initial.InitialDeadline.Equal(start.Expires) || !initial.Lease.Deadline.Equal(start.Expires) || initial.Lease.Version != 0 || initial.Lease.Accepted != nil || !initial.Lease.Renewable || f.calls != 0 || checker.checks != 1 || resolves != 1 {
		t.Fatal("initial acceptance lost exact retained provenance")
	}
	if state(t, rpc(w, "start", start, true)) != "pending" {
		t.Fatal("duplicate did not attach to starting record")
	}
	duplicate, err := w.AcceptedStart(ctx, start.Identity)
	if err != nil || duplicate != h || inspect().AttemptNonce != initial.AttemptNonce {
		t.Fatal("duplicate changed record/nonce", err)
	}
	initial.Identity.Session = "changed"
	initial.StartHash = [32]byte{}
	initial.AttemptNonce = "changed"
	initial.Lease.Deadline = time.Time{}
	if s := inspect(); s.Identity != start.Identity || s.StartHash != committedHash || s.AttemptNonce == "changed" || !s.Lease.Deadline.Equal(start.Expires) {
		t.Fatal("snapshot mutation reached retained acceptance")
	}
	for _, invalid := range []*AcceptedStartHandle{nil, {}, {worker: w}, func() *AcceptedStartHandle { copy := *h; return &copy }()} {
		if _, err := w.InspectAcceptedStart(ctx, invalid); err == nil {
			t.Fatal("caller-created handle accepted")
		}
	}
	raw, _ := json.Marshal(h)
	var decoded AcceptedStartHandle
	if string(raw) != "{}" || json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("opaque handle exposed fields")
	}
	if _, err := w.InspectAcceptedStart(ctx, &decoded); err == nil {
		t.Fatal("deserialized handle accepted")
	}
	other, _, _ := fixture(t)
	if _, err := other.InspectAcceptedStart(ctx, h); err == nil {
		t.Fatal("another Worker accepted same-boot handle")
	}
	wrongBoot := start.Identity
	wrongBoot.Worker = "old-worker-boot"
	if _, err := w.AcceptedStart(ctx, wrongBoot); err == nil {
		t.Fatal("wrong boot accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.AcceptedStart(canceled, start.Identity); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled getter", err)
	}
	if _, err := w.InspectAcceptedStart(canceled, h); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled inspection", err)
	}

	// Inconsistent/missing/expired records fail without creating a tombstone,
	// expiring work, stopping the attempt or calling external dependencies.
	w.mu.Lock()
	e := w.records[start.Identity]
	originalHash, cap := e.hash, e.monotonicDeadline
	e.hash = [32]byte{}
	w.mu.Unlock()
	if _, err := w.InspectAcceptedStart(ctx, h); err == nil {
		t.Fatal("inconsistent retained hash accepted")
	}
	w.mu.Lock()
	e.hash = originalHash
	delete(w.records, start.Identity)
	w.mu.Unlock()
	if _, err := w.InspectAcceptedStart(ctx, h); err == nil {
		t.Fatal("missing retained record accepted")
	}
	w.mu.Lock()
	w.records[start.Identity] = e
	alias := start.Identity
	alias.Session = "inconsistent-map-alias"
	w.records[alias] = e
	w.mu.Unlock()
	if _, err := w.AcceptedStart(ctx, alias); err == nil {
		t.Fatal("requested identity substituted through inconsistent map alias")
	}
	w.mu.Lock()
	delete(w.records, alias)
	e.monotonicDeadline = time.Now().Add(-time.Second)
	w.mu.Unlock()
	if _, err := w.InspectAcceptedStart(ctx, h); err == nil {
		t.Fatal("expired record accepted")
	}
	w.mu.Lock()
	stopped := e.stopped
	e.monotonicDeadline = cap
	w.mu.Unlock()
	if stopped {
		t.Fatal("inspection expired/stopped ownership")
	}
	releaseSource()
	wait(f.entered)
	if !inspect().Starting || f.calls != 1 || checker.checks != 1 || resolves != 1 || validations != 0 {
		t.Fatal("inspection had execution effects")
	}
	releaseBackend()
	wait(out.entered) // Start has accepted ownership; caller still has no response.
	if s := inspect(); s.Starting || s.StartHash != committedHash || s.AttemptNonce != h.nonce || f.session.stopped {
		t.Fatal("blocked response changed acceptance")
	}
	n := renewal(start)
	if renewRPC(w, n).Code != 200 {
		t.Fatal("explicit fixture renewal")
	}
	renewed := inspect()
	if renewed.Lease.Version != 1 || renewed.Lease.Accepted == nil || !sameRenewal(*renewed.Lease.Accepted, n) || !renewed.InitialDeadline.Equal(start.Expires) || renewed.StartHash != committedHash {
		t.Fatal("accepted lease history lost original acceptance")
	}
	renewed.Lease.Accepted.Deadline = time.Time{}
	if !sameRenewal(*inspect().Lease.Accepted, n) || validations != 1 || f.calls != 1 || checker.checks != 1 || resolves != 1 || f.session.stopped {
		t.Fatal("inspection aliased renewal or had effects")
	}
	if state(t, rpc(w, "start", start, true)) != "running" {
		t.Fatal("duplicate after renewal")
	}
	if _, err := w.Terminal(ctx, start.Identity, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.InspectAcceptedStart(ctx, h); err == nil {
		t.Fatal("stopped handle accepted")
	}
	if _, err := w.AcceptedStart(ctx, start.Identity); err == nil {
		t.Fatal("stopped getter accepted")
	}

	// A different real acceptance receives a distinct boot-scoped nonce; a
	// negative barrier never gets a handle. Close invalidates retained handles.
	f.entered = nil
	w.opts.Resolve = originalResolve
	second := start
	second.Identity.Session = "second-session"
	if state(t, rpc(w, "start", second, true)) != "running" {
		t.Fatal("second record")
	}
	h2, err := w.AcceptedStart(ctx, second.Identity)
	if err != nil || h2.nonce == h.nonce {
		t.Fatal("nonce reused", err)
	}
	negative := start.Identity
	negative.Session = "negative-session"
	if _, err := w.Terminal(ctx, negative, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.AcceptedStart(ctx, negative); err == nil {
		t.Fatal("negative tombstone became accepted Start")
	}
	w.Close()
	if _, err := w.InspectAcceptedStart(ctx, h2); err == nil {
		t.Fatal("closed Worker accepted handle")
	}
	releaseOutput()
	wait(returned)
}
