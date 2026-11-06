package nodeinput

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodesource"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func certs(t *testing.T) (tls.Certificate, tls.Certificate, *x509.CertPool) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if e != nil {
		t.Fatal(e)
	}
	root, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	leaf := func(n int64, name string, usage x509.ExtKeyUsage) tls.Certificate {
		p, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		c := &x509.Certificate{SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
		d, e := x509.CreateCertificate(rand.Reader, c, root, p, key)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{d, der}, PrivateKey: k}
	}
	return leaf(2, "source", x509.ExtKeyUsageServerAuth), leaf(3, "worker", x509.ExtKeyUsageClientAuth), pool
}

type authority struct {
	mu     sync.Mutex
	grants map[string]nodesource.Grant
}

func (a *authority) Verify(_ context.Context, p nodesource.Peer, key string) (nodesource.Grant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.grants[key]
	if !ok || p.Boot != g.Scope.Fence.Target.Boot {
		return nodesource.Grant{}, errors.New("denied")
	}
	return g, nil
}

type source struct {
	read func(context.Context, []byte, int64) (int, error)
}

func (s *source) ReadAt(b []byte, o int64) (int, error) {
	return bytes.NewReader([]byte("0123456789abcdef")).ReadAt(b, o)
}
func (s *source) ReadAtContext(c context.Context, b []byte, o int64) (int, error) {
	if s.read != nil {
		return s.read(c, b, o)
	}
	return s.ReadAt(b, o)
}
func (*source) Size() int64       { return 16 }
func (*source) Name() string      { return "secret" }
func (*source) ReleaseID() string { return "revision" }
func (*source) Close() error      { panic("remote source must remain owned separately") }

type fixture struct {
	g     *Gateway
	in    *Input
	a     *authority
	grant nodesource.Grant
	start remotetranscode.Start
	h     *nodesource.Handler
	src   *source
}

func setup(t *testing.T, recheck ...func(context.Context) error) *fixture {
	t.Helper()
	f := &fixture{src: &source{}}
	f.grant = nodesource.Grant{Scope: nodesource.Scope{Fence: placement.Fence{Controller: placement.Controller{ID: "controller", Boot: "cb"}, Target: placement.Target{NodeID: "worker", Boot: "wb"}, Session: "session", Generation: 1}, Viewer: "viewer", Revision: "revision"}, Credential: "initial-bearer", Expires: time.Now().Add(30 * time.Second)}
	f.a = &authority{grants: map[string]nodesource.Grant{f.grant.Credential: f.grant}}
	var err error
	f.h, err = nodesource.NewHandler(nodesource.Options{Authority: f.a, Peer: func(*x509.Certificate) (nodesource.Peer, error) {
		return nodesource.Peer{NodeID: "worker", Boot: "wb"}, nil
	}, Acquire: func(_ context.Context, s nodesource.Scope) (nodesource.Hold, error) {
		return nodesource.Hold{Scope: s, Source: f.src, Release: func() {}}, nil
	}, Now: time.Now, MaxRequests: 8, MaxLifetime: time.Minute, Revalidate: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	sc, cc, pool := certs(t)
	server := httptest.NewUnstartedServer(f.h)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{sc}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	client, err := nodesource.NewClient(nodesource.ClientOptions{Endpoint: server.URL + nodesource.Path, TLS: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cc}, ServerName: "source"}, BindServer: func(s tls.ConnectionState) error {
		if s.PeerCertificates[0].Subject.CommonName != "source" {
			return errors.New("peer")
		}
		return nil
	}, RecheckServer: func(ctx context.Context, s tls.ConnectionState) error {
		if s.PeerCertificates[0].Subject.CommonName != "source" {
			return errors.New("peer")
		}
		for _, check := range recheck {
			if err := check(ctx); err != nil {
				return err
			}
		}
		return nil
	}, Peer: nodesource.Peer{NodeID: "worker", Boot: "wb"}, MaxLifetime: time.Minute, Timeout: time.Second, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	f.g, err = New(Options{Client: client, Capacity: 8, MaxRequests: 2, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { defer close(exited); _ = f.g.Serve(context.Background()) }()
	t.Cleanup(func() { _ = f.g.Close(); <-exited; f.g.Reconcile(); f.h.Reconcile() })
	f.start = remotetranscode.Start{Identity: remotetranscode.Identity{Controller: "cb", Worker: "wb", Session: "session", Generation: 1}, Grant: "stable-handle", Expires: time.Now().Add(15 * time.Second), Decision: core.Decision{Kind: core.DecisionRemux, SubtitleStream: -1}}
	f.in, err = f.g.Admit(context.Background(), f.start, f.grant, 16)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func fetch(t *testing.T, in *Input, method, ran string) (int, string, http.Header) {
	t.Helper()
	raw, lease, e := in.GatewayInput(in.id, in.handle)
	if e != nil {
		t.Fatal(e)
	}
	headers, e := lease.HeadersForInput(raw)
	if e != nil {
		t.Fatal(e)
	}
	r, _ := http.NewRequest(method, raw, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r.Header.Set("Range", ran)
	c := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, e := c.Do(r)
	if e != nil {
		t.Fatal("local request failed")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, string(b), res.Header
}

type encoder struct {
	t     *testing.T
	in    *Input
	calls int
	fail  bool
}

func (e *encoder) StartHLSOwned(_ context.Context, r core.HLSRequest) (core.HLSSession, core.EncoderRetirement, error) {
	e.calls++
	if r.InputURL != e.in.url || r.InputLease != e.in || len(r.InputHeaders) != 0 {
		e.t.Fatal("input escaped exact binding")
	}
	if err := r.InputLease.ClaimInput(func(error) {}); err != nil {
		e.t.Fatal(err)
	}
	code, b, _ := fetch(e.t, e.in, "GET", "bytes=2-5")
	if code != 206 || b != "2345" {
		e.t.Fatal(code, b)
	}
	s := &session{done: make(chan struct{})}
	if e.fail {
		return s, s, errors.New("encoder start failed")
	}
	return s, s, nil
}

type session struct {
	done chan struct{}
	once sync.Once
}

func (*session) ID() string                                           { return "owned" }
func (*session) ServePlaylist(http.ResponseWriter, *http.Request)     {}
func (*session) ServeSegment(http.ResponseWriter, *http.Request, int) {}
func (*session) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return errors.New("unsupported")
}
func (*session) LastAccess() time.Time                       { return time.Now() }
func (s *session) Close() error                              { return s.RetireEncoder() }
func (s *session) RetireEncoder() error                      { s.once.Do(func() { close(s.done) }); return nil }
func (s *session) EncoderRetirement() core.EncoderRetirement { return s }
func (s *session) EncoderDone() <-chan struct{}              { return s.done }
func (s *session) EncoderErr() error                         { return nil }
func rpc(t *testing.T, w *remotetranscode.Worker, f *fixture, op string, n *remotetranscode.Renewal) *httptest.ResponseRecorder {
	t.Helper()
	m := map[string]any{"Version": 1, "Op": op, "Identity": f.start.Identity}
	if op == "start" {
		m["Start"] = f.start
	}
	if n != nil {
		m["Renewal"] = n
	}
	b, _ := json.Marshal(m)
	r := httptest.NewRequest("POST", "https://worker/internal", bytes.NewReader(b))
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	return out
}
func TestOwnedEndToEndRenewStopAndStartError(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "renew-stop", true: "start-error"}[fail], func(t *testing.T) {
			f := setup(t)
			enc := &encoder{t: t, in: f.in, fail: fail}
			w, e := remotetranscode.NewWorker(remotetranscode.WorkerOptions{Boot: "wb", Capacity: 8, MaxLease: time.Minute, Security: remotetranscode.Security{Authenticate: func(*http.Request) (string, error) { return "cb", nil }, Authorize: func(context.Context, string, string, remotetranscode.Identity, *remotetranscode.Start) error {
				return nil
			}}, Encoder: enc, Resolve: func(context.Context, string, remotetranscode.Start) (remotetranscode.Source, error) {
				return remotetranscode.Source{Gateway: f.in}, nil
			}, ValidateRenewal: func(c context.Context, _ string, s remotetranscode.Start, n remotetranscode.Renewal) (remotetranscode.RenewalReceipt, error) {
				evidence, err := f.in.Renew(c, n, func(_ context.Context, old nodesource.Grant) (nodesource.Grant, error) {
					next := old
					next.Credential = "rotated-bearer"
					next.Expires = time.Now().Add(50 * time.Second)
					f.a.mu.Lock()
					f.a.grants[next.Credential] = next
					f.a.mu.Unlock()
					return next, nil
				})
				if err != nil {
					return remotetranscode.RenewalReceipt{}, err
				}
				if evidence.Identity != s.Identity || evidence.Handle != s.Grant || evidence.Scope != f.grant.Scope || evidence.Size != 16 || evidence.InputLease != f.in {
					t.Fatal("evidence changed")
				}
				return remotetranscode.RenewalReceipt{Renewal: n, PlacementUntil: n.Deadline, SourceUntil: evidence.SourceUntil}, nil
			}})
			if e != nil {
				t.Fatal(e)
			}
			defer w.Close()
			out := rpc(t, w, f, "start", nil)
			if fail {
				if out.Code != 409 {
					t.Fatal(out.Code)
				}
			} else {
				if out.Code != 200 {
					t.Fatal(out.Code, out.Body.String())
				}
				for _, tc := range []struct {
					method, ran, body, cr string
					status                int
				}{{"HEAD", "", "", "", 200}, {"GET", "bytes=-3", "def", "bytes 13-15/16", 206}, {"GET", "bytes=14-", "ef", "bytes 14-15/16", 206}, {"GET", "bytes=16-", "", "bytes */16", 416}, {"GET", "bytes=0-1,3-4", "", "bytes */16", 416}} {
					code, b, h := fetch(t, f.in, tc.method, tc.ran)
					if code != tc.status || b != tc.body || h.Get("Content-Range") != tc.cr {
						t.Fatal(code, b, h)
					}
				}
				n := remotetranscode.Renewal{Identity: f.start.Identity, Operation: 1, PreviousDeadline: f.start.Expires.UTC(), Deadline: time.Now().Add(40 * time.Second).UTC()}
				out = rpc(t, w, f, "renew", &n)
				if out.Code != 200 {
					t.Fatal(out.Code, out.Body.String())
				}
				f.a.mu.Lock()
				delete(f.a.grants, f.grant.Credential)
				f.a.mu.Unlock()
				code, b, _ := fetch(t, f.in, "GET", "bytes=0-1")
				if code != 206 || b != "01" {
					t.Fatal(code, b)
				}
				if enc.calls != 1 {
					t.Fatal("renew started encoder")
				}
				out = rpc(t, w, f, "stop", nil)
				if out.Code != 200 {
					t.Fatal(out.Code)
				}
			}
			if a := f.g.Reconcile(); a.Pending != 0 {
				t.Fatal(a)
			}
			if f.in.InputErr() != nil {
				t.Fatal("local input not drained")
			}
			out = rpc(t, w, f, "status", nil)
			if !bytes.Contains(out.Body.Bytes(), []byte(`"State":"retired"`)) {
				t.Fatal(out.Body.String())
			}
			if _, _, err := f.in.GatewayInput(f.start.Identity, f.start.Grant); err == nil {
				t.Fatal("sealed capability admitted")
			}
		})
	}
}

type blockedWriter struct {
	header           http.Header
	entered, release chan struct{}
}

func (w *blockedWriter) Header() http.Header { return w.header }
func (*blockedWriter) WriteHeader(int)       {}
func (w *blockedWriter) Write(b []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(b), nil
}
func TestSealRetainsBlockedWriterAndLateProducer(t *testing.T) {
	f := setup(t)
	if e := f.in.ClaimInput(func(error) {}); e != nil {
		t.Fatal(e)
	}
	// A producer that outlives its initiating call remains charged after sealing.
	release, ok := f.in.tracker.Begin()
	if !ok {
		t.Fatal("begin")
	}
	w := &blockedWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("GET", f.in.url, nil)
	r.Header.Set(core.EncoderInputScopeHeader, f.in.key)
	exited := make(chan struct{})
	go func() { defer close(exited); f.g.ServeHTTP(w, r) }()
	<-w.entered
	f.in.SealInput()
	if _, ok := f.in.tracker.Begin(); ok {
		t.Fatal("late producer admitted")
	}
	if f.g.Reconcile().Pending != 1 || f.in.InputErr() != core.ErrEncoderInputPending {
		t.Fatal("writer forgotten")
	}
	denied := httptest.NewRecorder()
	f.g.ServeHTTP(denied, r)
	if denied.Code != 403 {
		t.Fatal(denied.Code)
	}
	close(w.release)
	<-exited
	if f.g.Reconcile().Pending != 1 {
		t.Fatal("delayed producer forgotten")
	}
	release()
	if f.g.Reconcile().Pending != 0 {
		t.Fatal("did not drain")
	}
}

func TestRemoteProducerProofStaysSeparate(t *testing.T) {
	f := setup(t)
	entered, sourceExited := make(chan struct{}), make(chan struct{})
	var remoteRelease func()
	f.src.read = func(c context.Context, _ []byte, _ int64) (int, error) {
		var ok bool
		remoteRelease, ok = core.ReadRetirementFrom(c).Begin()
		if !ok {
			panic("sealed remote tracker")
		}
		close(entered)
		<-c.Done()
		close(sourceExited)
		return 0, c.Err()
	}
	if e := f.in.ClaimInput(func(error) {}); e != nil {
		t.Fatal(e)
	}
	request, _ := http.NewRequest("GET", f.in.url, nil)
	request.Header.Set(core.EncoderInputScopeHeader, f.in.key)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c := &http.Client{Timeout: time.Second}
		res, _ := c.Do(request)
		if res != nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
		}
	}()
	<-entered
	f.in.SealInput()
	<-finished
	<-sourceExited
	until := time.Now().Add(time.Second)
	for f.g.Reconcile().Pending != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if f.in.InputErr() != nil {
		t.Fatal("local HTTP reader not drained")
	}
	if f.h.Pending(f.grant.Scope) == 0 {
		t.Fatal("local input done discarded remote source producer")
	}
	remoteRelease()
	until = time.Now().Add(time.Second)
	for f.h.Reconcile() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if f.h.Pending(f.grant.Scope) != 0 {
		t.Fatal("remote producer did not drain")
	}
}

func TestCapabilityParsingAndFailedAdmission(t *testing.T) {
	f := setup(t)
	if e := f.in.ClaimInput(func(error) {}); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Add("Range", "bytes=0-1"); r.Header.Add("Range", "bytes=2-3") },
		func(r *http.Request) { r.Header.Add(core.EncoderInputScopeHeader, f.in.key) },
		func(r *http.Request) { r.URL.RawPath = r.URL.Path },
		func(r *http.Request) { r.URL.ForceQuery = true },
		func(r *http.Request) { r.Host = "elsewhere" },
	} {
		r := httptest.NewRequest("GET", f.in.url, nil)
		r.Header.Set(core.EncoderInputScopeHeader, f.in.key)
		mutate(r)
		out := httptest.NewRecorder()
		f.g.ServeHTTP(out, r)
		if out.Code != 403 {
			t.Fatal("alias admitted", out.Code)
		}
	}
	if _, e := f.in.HeadersForInput(f.in.url + "?"); e == nil {
		t.Fatal("header URL alias")
	}
	if _, _, e := f.in.GatewayInput(f.in.id, "wrong-handle"); e == nil {
		t.Fatal("handle mismatch")
	}
	wrong := f.start
	wrong.Identity.Generation = ^uint64(0)
	if in, e := f.g.Admit(context.Background(), wrong, f.grant, 16); in != nil || e == nil {
		t.Fatal("overflow admitted")
	}
	in, e := f.g.Admit(context.Background(), f.start, f.grant, 17)
	if in == nil || e == nil {
		t.Fatal("failed open lost responsibility")
	}
	if in.InputErr() != core.ErrEncoderInputPending {
		t.Fatal("proof before reconciliation")
	}
	f.g.Reconcile()
	if in.InputErr() != nil {
		t.Fatal("failed open did not retire")
	}
}

func TestRenewalHistoryAndRevokedWorkerRetry(t *testing.T) {
	t.Run("revoked-before-worker-retry", func(t *testing.T) {
		f := setup(t)
		enc := &encoder{t: t, in: f.in}
		calls := 0
		w, err := remotetranscode.NewWorker(remotetranscode.WorkerOptions{Boot: "wb", Capacity: 8, MaxLease: time.Minute, Encoder: enc,
			Security: remotetranscode.Security{Authenticate: func(*http.Request) (string, error) { return "cb", nil }, Authorize: func(context.Context, string, string, remotetranscode.Identity, *remotetranscode.Start) error {
				return nil
			}},
			Resolve: func(context.Context, string, remotetranscode.Start) (remotetranscode.Source, error) {
				return remotetranscode.Source{Gateway: f.in}, nil
			},
			ValidateRenewal: func(ctx context.Context, _ string, _ remotetranscode.Start, n remotetranscode.Renewal) (remotetranscode.RenewalReceipt, error) {
				evidence, e := f.in.Renew(ctx, n, func(_ context.Context, old nodesource.Grant) (nodesource.Grant, error) {
					calls++
					next := old
					next.Credential = "renewed"
					next.Expires = time.Now().Add(50 * time.Second)
					f.a.mu.Lock()
					f.a.grants[next.Credential] = next
					f.a.mu.Unlock()
					return next, nil
				})
				if e != nil {
					if evidence.InputLease != nil {
						t.Fatal("error returned historical evidence")
					}
					return remotetranscode.RenewalReceipt{}, e
				}
				// Source authority disappears before placement validation/worker acceptance.
				f.a.mu.Lock()
				delete(f.a.grants, "renewed")
				f.a.mu.Unlock()
				return remotetranscode.RenewalReceipt{}, errors.New("placement rejected after source extension")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if out := rpc(t, w, f, "start", nil); out.Code != 200 {
			t.Fatal(out.Code)
		}
		n := remotetranscode.Renewal{Identity: f.start.Identity, Operation: 1, PreviousDeadline: f.start.Expires.UTC(), Deadline: time.Now().Add(40 * time.Second).UTC()}
		if out := rpc(t, w, f, "renew", &n); out.Code != 403 {
			t.Fatal(out.Code)
		}
		if out := rpc(t, w, f, "renew", &n); out.Code != 403 {
			t.Fatal("revoked replay supplied stale evidence", out.Code)
		}
		if calls != 1 {
			t.Fatal("successful replay performed another extension")
		}
		out := rpc(t, w, f, "status", nil)
		var state struct{ Lease remotetranscode.LeaseState }
		if e := json.Unmarshal(out.Body.Bytes(), &state); e != nil {
			t.Fatal(e)
		}
		if state.Lease.Version != 0 || !state.Lease.Deadline.Equal(f.start.Expires) {
			t.Fatal("source extension manufactured worker acceptance")
		}
	})
	t.Run("operation-history", func(t *testing.T) {
		f := setup(t)
		calls := 0
		authority := func(_ context.Context, old nodesource.Grant) (nodesource.Grant, error) {
			calls++
			next := old
			next.Credential = old.Credential + "x"
			next.Expires = old.Expires.Add(10 * time.Second)
			f.a.mu.Lock()
			f.a.grants[next.Credential] = next
			f.a.mu.Unlock()
			return next, nil
		}
		n1 := remotetranscode.Renewal{Identity: f.start.Identity, Operation: 1, PreviousDeadline: f.start.Expires.UTC(), Deadline: f.start.Expires.Add(5 * time.Second).UTC()}
		if _, e := f.in.Renew(context.Background(), n1, authority); e != nil {
			t.Fatal(e)
		}
		n2 := remotetranscode.Renewal{Identity: n1.Identity, Operation: 2, PreviousVersion: 1, PreviousDeadline: n1.Deadline, Deadline: n1.Deadline.Add(5 * time.Second)}
		changed := n2
		changed.PreviousDeadline = changed.PreviousDeadline.Add(time.Nanosecond)
		if _, e := f.in.Renew(context.Background(), changed, authority); e == nil {
			t.Fatal("wrong prior deadline admitted")
		}
		if _, e := f.in.Renew(context.Background(), n2, authority); e != nil {
			t.Fatal(e)
		}
		changed = n2
		changed.Deadline = changed.Deadline.Add(time.Nanosecond)
		for _, n := range []remotetranscode.Renewal{n1, n2, changed} {
			evidence, e := f.in.Renew(context.Background(), n, authority)
			if e == nil || evidence.InputLease != nil {
				t.Fatal("historical/changed replay admitted")
			}
		}
		if calls != 2 || !equalRenewal(*f.in.renewal, n2) {
			t.Fatal("rejected replay mutated source history")
		}
	})
	t.Run("concurrent-validation-and-stop", func(t *testing.T) {
		f := setup(t)
		entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		n := remotetranscode.Renewal{Identity: f.start.Identity, Operation: 1, PreviousDeadline: f.start.Expires.UTC(), Deadline: f.start.Expires.Add(time.Second).UTC()}
		go func() {
			defer close(finished)
			evidence, e := f.in.Renew(context.Background(), n, func(c context.Context, old nodesource.Grant) (nodesource.Grant, error) {
				close(entered)
				<-release
				return nodesource.Grant{}, c.Err()
			})
			if e == nil || evidence.InputLease != nil {
				t.Error("stopped renewal returned evidence")
			}
		}()
		<-entered
		_, e := f.in.Renew(context.Background(), n, func(context.Context, nodesource.Grant) (nodesource.Grant, error) {
			t.Error("concurrent authority invoked")
			return nodesource.Grant{}, ErrDenied
		})
		if e == nil {
			t.Fatal("concurrent operation admitted")
		}
		f.in.SealInput()
		if f.g.Reconcile().Pending != 1 {
			t.Fatal("blocked authority producer forgotten")
		}
		close(release)
		<-finished
		if f.g.Reconcile().Pending != 0 {
			t.Fatal("renewal did not drain")
		}
	})
}
