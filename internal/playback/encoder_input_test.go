package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func inputHarness(t *testing.T) (*harness, *core.PlaySession, core.SourceRetirement) {
	t.Helper()
	h := newOwnedHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	ps, outcome, err := h.m.PrepareSurfaceSource(context.Background(), owner, movieKey, defaultCaps("input"), "", false)
	if err != nil {
		t.Fatal(err)
	}
	return h, ps, outcome
}
func inputLease(t *testing.T, m *Manager, id string, outcome core.SourceRetirement) *encoderInputScope {
	t.Helper()
	lease, err := m.ReserveEncoderInput(context.Background(), id, outcome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.SealInput)
	return lease.(*encoderInputScope)
}

func TestEncoderInputLeaseBlocksRetireAndReplacement(t *testing.T) {
	h, ps, outcome := inputHarness(t)
	lease := inputLease(t, h.m, ps.ID, outcome)
	if err := h.m.RetireSource(ps.ID, outcome); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("retire under lease: %v", err)
	}
	if _, err := h.m.Failover(context.Background(), ps.ID, 0); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("replace under lease: %v", err)
	}
	lease.SealInput()
	if err := h.m.RetireSource(ps.ID, outcome); err != nil {
		t.Fatal(err)
	}
}

func TestEncoderInputClaimDrainAndReentrancy(t *testing.T) {
	for _, earlyRelease := range []bool{false, true} {
		t.Run(map[bool]string{false: "seal-before-release", true: "release-before-seal"}[earlyRelease], func(t *testing.T) {
			m, g, _ := retirementFixture()
			g.sess.id, g.sess.sources, g.token = "exact", newSourceLifetime(1, "plex", "input"), "issued-generation"
			m.sessions = map[string]*session{g.sess.id: g.sess}
			m.tokens = map[string]*generation{g.sess.id + "/" + g.token: g}
			m.baseURL = "http://127.0.0.1:12345"
			scope := inputLease(t, m, g.sess.id, g.sess.sources)
			if _, err := scope.HeadersForInput(m.urlFor(g)); err == nil {
				t.Fatal("unclaimed scope issued headers")
			}
			var callbacks, rejected atomic.Int32
			if err := scope.ClaimInput(func(err error) {
				callbacks.Add(1)
				if err != nil || scope.InputErr() != nil {
					t.Errorf("drained result=%v/%v", err, scope.InputErr())
				}
				for name, mu := range map[string]*sync.Mutex{"manager": &m.mu, "scope": &scope.mu, "session": &scope.session.mu, "source": &scope.source.mu, "generation": &g.mu} {
					if !mu.TryLock() {
						t.Errorf("callback under %s lock", name)
					} else {
						mu.Unlock()
					}
				}
				m.mu.Lock()
				present := m.encoderInputs[scope.id] != nil
				m.mu.Unlock()
				if present {
					t.Error("callback preceded registry release")
				}
				scope.SealInput() // supported bounded reentrancy, no double release
				if err := scope.ClaimInput(func(error) { rejected.Add(1) }); err == nil {
					t.Error("reentrant claim succeeded")
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := scope.ClaimInput(func(error) { rejected.Add(1) }); err == nil {
				t.Fatal("second encoder shared the scope")
			}
			finish, ok := scope.begin(g)
			if !ok {
				t.Fatal("producer denied")
			}
			t.Cleanup(finish)
			if earlyRelease {
				finish()
			}
			if callbacks.Load() != 0 {
				t.Fatal("callback before seal")
			}
			scope.SealInput()
			if !earlyRelease {
				assertRetirementOpen(t, scope.InputDone(), "producer still active")
				finish()
			}
			finish()
			waitRetirementTest(t, scope.InputDone(), "input drain")
			if callbacks.Load() != 1 || rejected.Load() != 0 {
				t.Fatalf("callback count=%d rejected=%d", callbacks.Load(), rejected.Load())
			}
		})
	}
}

func TestEncoderInputSealReleaseRacesAndAdmission(t *testing.T) {
	h, ps, outcome := inputHarness(t)
	g := h.m.lookup(ps.ID).current()
	for i := 0; i < 64; i++ {
		scope := inputLease(t, h.m, ps.ID, outcome)
		var calls atomic.Int32
		if err := scope.ClaimInput(func(error) { calls.Add(1) }); err != nil {
			t.Fatal(err)
		}
		finish, ok := scope.begin(g)
		if !ok {
			t.Fatal("producer denied")
		}
		var wg sync.WaitGroup
		for _, f := range []func(){scope.SealInput, finish, scope.SealInput, finish} {
			wg.Add(1)
			go func() { defer wg.Done(); f() }()
		}
		wg.Wait()
		if calls.Load() != 1 || scope.InputErr() != nil {
			t.Fatalf("race completion count=%d err=%v", calls.Load(), scope.InputErr())
		}
	}
	var scopes []*encoderInputScope
	for i := 0; i < maxEncoderInputs; i++ {
		scopes = append(scopes, inputLease(t, h.m, ps.ID, outcome))
	}
	if _, err := h.m.ReserveEncoderInput(context.Background(), ps.ID, outcome); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("scope capacity=%v", err)
	}
	for _, scope := range scopes {
		scope.SealInput()
		var called atomic.Bool
		if err := scope.ClaimInput(func(error) { called.Store(true) }); err == nil || called.Load() {
			t.Fatal("claim-after-seal acquired ownership")
		}
	}
	if _, err := h.m.ReserveEncoderInput(context.Background(), ps.ID, newSourceLifetime(1, "", "")); err == nil {
		t.Fatal("caller-supplied source responsibility minted input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.m.ReserveEncoderInput(ctx, ps.ID, outcome); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled mint=%v", err)
	}
}

func TestEncoderInputExactGenerationAndLegacyWire(t *testing.T) {
	h, ps, outcome := inputHarness(t)
	old := h.m.lookup(ps.ID).current()
	scope := inputLease(t, h.m, ps.ID, outcome)
	if err := scope.ClaimInput(func(error) {}); err != nil {
		t.Fatal(err)
	}
	input := h.m.urlFor(old)
	headers, err := scope.HeadersForInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", scope, scope, scope), scope.id) {
		t.Fatal("opaque input lease leaked through formatting")
	}
	fetch := func(hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, input, nil)
		r.SetPathValue("session", old.sess.id)
		r.SetPathValue("token", old.token)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.m.serveLoopback(w, r)
		return w
	}
	legacy, owned := fetch(nil), fetch(headers)
	if legacy.Code != owned.Code || !reflect.DeepEqual(legacy.Header(), owned.Header()) || legacy.Body.String() != owned.Body.String() {
		t.Fatal("tracker changed legacy loopback status/headers/body bytes")
	}
	for _, bad := range []string{
		strings.Replace(input, "127.0.0.1", "localhost", 1),
		strings.Replace(input, "http://", "https://", 1),
		strings.Replace(input, "http://", "http://caller@", 1),
		input + "/", input + "#fragment", input + "?redirect=https://provider.invalid/", input + "?background=1&background=1", input + "?", "http://127.0.0.1:1/src/" + old.sess.id + "/" + old.token,
		"https://provider.invalid/redirect", input + "-unknown",
	} {
		if _, err := scope.HeadersForInput(bad); err == nil {
			t.Fatalf("foreign/redirect/generation input accepted: %q", bad)
		}
	}
	if _, err := scope.HeadersForInput(input + "?background=1"); err != nil {
		t.Fatal(err)
	}
	if w := fetch(map[string]string{core.EncoderInputScopeHeader: "forged"}); w.Code != http.StatusForbidden {
		t.Fatalf("forged scope fell back: %d", w.Code)
	}
	// The supplied future token is validated against the captured exact
	// responsibility; the implementation never substitutes current state.
	if _, err := h.m.Failover(context.Background(), ps.ID, 0); err != nil {
		t.Fatal(err)
	}
	current := h.m.lookup(ps.ID).current()
	if current == old {
		t.Fatal("failover did not issue another generation")
	}
	if _, err := scope.HeadersForInput(h.m.urlFor(current)); err != nil {
		t.Fatalf("same exact responsibility rejected failover generation: %v", err)
	}
	if _, err := scope.HeadersForInput(input); err == nil {
		t.Fatal("removed prior generation was resurrected")
	}
	other := h.prepare(owner, movie2, "other-input")
	if _, err := scope.HeadersForInput(h.m.InputURL(other.ID)); err == nil {
		t.Fatal("arbitrary later source adopted by current-state lookup")
	}
	h.m.Stop(ps.ID)
	replacement := h.prepare(owner, movieKey, "input")
	if _, err := scope.HeadersForInput(h.m.InputURL(replacement.ID)); err == nil {
		t.Fatal("replacement session adopted a retired lifetime")
	}
	if _, err := h.m.ReserveEncoderInput(context.Background(), replacement.ID, outcome); err == nil {
		t.Fatal("stale outcome minted on replacement")
	}
}

func TestEncoderInputStalledReadAndCloseRetainBothTrackers(t *testing.T) {
	m, g, src := retirementFixture()
	g.sess.id, g.sess.sources, g.token = "exact", newSourceLifetime(1, "plex", "input"), "issued-generation"
	m.sessions = map[string]*session{g.sess.id: g.sess}
	m.tokens = map[string]*generation{g.sess.id + "/" + g.token: g}
	m.baseURL = "http://127.0.0.1:12345"
	scope := inputLease(t, m, g.sess.id, g.sess.sources)
	callback := make(chan struct{})
	if err := scope.ClaimInput(func(error) { close(callback) }); err != nil {
		t.Fatal(err)
	}
	headers, err := scope.HeadersForInput(m.urlFor(g))
	if err != nil {
		t.Fatal(err)
	}
	tracker := &core.ReadRetirement{}
	ctx, cancel := context.WithCancel(core.WithReadRetirement(context.Background(), tracker))
	r := httptest.NewRequest(http.MethodGet, m.urlFor(g), nil).WithContext(ctx)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r, err = m.encoderInputRequest(r, g)
	if err != nil {
		t.Fatal(err)
	}
	var readOnce, closeOnce sync.Once
	returned := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		scope.SealInput()
		readOnce.Do(func() { close(src.readRelease) })
		closeOnce.Do(func() { close(src.closeRelease) })
		waitRetirementTest(t, returned, "copy joined")
		waitRetirementTest(t, scope.InputDone(), "producer joined")
	})
	go func() {
		defer close(returned)
		_, _, _ = m.copyFrom(r.Context(), io.Discard, g, 0, 1, func(int) {}, true)
	}()
	waitRetirementTest(t, src.entered, "scoped read entered")
	cancel()
	scope.SealInput()
	requestDone := tracker.Seal()
	assertRetirementOpen(t, scope.InputDone(), "stalled READ retained")
	assertRetirementOpen(t, requestDone, "request READ retained")
	g.retire()
	readOnce.Do(func() { close(src.readRelease) })
	waitRetirementTest(t, src.closing, "source Close entered")
	assertRetirementOpen(t, scope.InputDone(), "producer still in generation release")
	assertRetirementOpen(t, requestDone, "request still in generation release")
	closeOnce.Do(func() { close(src.closeRelease) })
	waitRetirementTest(t, callback, "one input callback")
	waitRetirementTest(t, requestDone, "request producer done")
	// Join only after the noncooperative read and actual Close are released;
	// cancellation alone does not prove either producer or consumer completion.
	waitRetirementTest(t, returned, "copy returned after source release")
}

func TestEncoderInputIndependentSharedSourceOwners(t *testing.T) {
	h, ps, outcome := inputHarness(t)
	g := h.m.lookup(ps.ID).current()
	one, two := inputLease(t, h.m, ps.ID, outcome), inputLease(t, h.m, ps.ID, outcome)
	var first, second atomic.Int32
	if err := one.ClaimInput(func(error) { first.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := two.ClaimInput(func(error) { second.Add(1) }); err != nil {
		t.Fatal(err)
	}
	a, ok := one.begin(g)
	if !ok {
		t.Fatal("first producer denied")
	}
	t.Cleanup(a)
	b, ok := two.begin(g)
	if !ok {
		t.Fatal("second producer denied")
	}
	t.Cleanup(b)
	one.SealInput()
	a()
	waitRetirementTest(t, one.InputDone(), "first independent encoder")
	if first.Load() != 1 || second.Load() != 0 {
		t.Fatal("one encoder retired another owner")
	}
	assertRetirementOpen(t, two.InputDone(), "second owner retained")
	if _, err := two.HeadersForInput(h.m.urlFor(g)); err != nil {
		t.Fatal("first encoder sealed shared owner's input")
	}
	two.SealInput()
	b()
	waitRetirementTest(t, two.InputDone(), "second independent encoder")
	zero := inputLease(t, h.m, ps.ID, outcome)
	if err := zero.ClaimInput(nil); err == nil {
		t.Fatal("nil callback claimed input")
	}
	var empty atomic.Int32
	if err := zero.ClaimInput(func(error) { empty.Add(1) }); err != nil {
		t.Fatal(err)
	}
	zero.SealInput()
	zero.SealInput()
	if empty.Load() != 1 || zero.InputErr() != nil {
		t.Fatal("zero-producer drain did not callback exactly once")
	}
}
