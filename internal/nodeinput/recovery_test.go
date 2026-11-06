package nodeinput

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestRecoveryClaimAndExactAdmission(t *testing.T) {
	var checks atomic.Int32
	f := setup(t, func(context.Context) error { checks.Add(1); return nil })
	if _, _, err := f.in.RecoverySourceIdentity(); err != ErrDenied {
		t.Fatal("unclaimed identity")
	}
	if n, err := f.in.ReadRecoveryAt(context.Background(), "revision", make([]byte, 1), 0); n != 0 || err != ErrDenied {
		t.Fatal("unclaimed read")
	}
	if err := f.in.ClaimInput(func(error) {}); err != nil {
		t.Fatal(err)
	}
	if err := f.in.ClaimInput(func(error) {}); err != ErrDenied {
		t.Fatal("second claim accepted")
	}
	revision, size, err := f.in.RecoverySourceIdentity()
	if err != nil || revision != f.grant.Scope.Revision || size != 16 {
		t.Fatal("identity did not match authenticated source")
	}
	before := checks.Load()
	for _, tc := range []struct {
		revision string
		off      int64
		length   int
	}{{"", 0, 1}, {"other", 0, 1}, {revision, -1, 1}, {revision, math.MaxInt64, 1}, {revision, 15, 2}, {revision, 16, 1}, {revision, 0, 0}, {revision, 0, MaxRecoveryRead + 1}} {
		if n, err := f.in.ReadRecoveryAt(context.Background(), tc.revision, make([]byte, tc.length), tc.off); n != 0 || err != ErrDenied {
			t.Fatal("invalid range/revision admitted")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := f.in.ReadRecoveryAt(canceled, revision, make([]byte, 1), 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled request admitted")
	}
	if checks.Load() != before {
		t.Fatal("denied read reached transport")
	}
	b := make([]byte, 4)
	if n, err := f.in.ReadRecoveryAt(context.Background(), revision, b, 2); n != 4 || err != nil || string(b) != "2345" {
		t.Fatal("same-source read failed", n, err)
	}
	f.in.SealInput()
	if _, _, err := f.in.RecoverySourceIdentity(); err != ErrDenied {
		t.Fatal("sealed identity")
	}
	if n, err := f.in.ReadRecoveryAt(context.Background(), revision, b, 2); n != 0 || err != ErrDenied {
		t.Fatal("sealed read")
	}
	if f.g.Reconcile().Pending != 0 {
		t.Fatal("completed reads retained")
	}
}

func TestRecoveryPendingProducerAndSharedAdmission(t *testing.T) {
	var block atomic.Bool
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	exited := make(chan struct{}, 2)
	f := setup(t, func(ctx context.Context) error {
		if !block.Load() {
			return nil
		}
		// An actual transport producer may outlive the initiating request. It
		// must join the same tracker before it starts, never a separate lease.
		end, ok := core.ReadRetirementFrom(ctx).Begin()
		if !ok {
			return ErrDenied
		}
		defer func() { end(); exited <- struct{}{} }()
		entered <- struct{}{}
		<-release
		return ctx.Err()
	})
	var callbacks atomic.Int32
	if err := f.in.ClaimInput(func(error) { callbacks.Add(1) }); err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	finished := make(chan error, 2)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	for i := 0; i < 2; i++ {
		go func() {
			_, err := f.in.ReadRecoveryAt(context.Background(), "revision", make([]byte, 1), 0)
			finished <- err
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("read not admitted")
		}
	}
	if n, err := f.in.ReadRecoveryAt(context.Background(), "revision", make([]byte, 1), 0); n != 0 || err != ErrDenied {
		t.Fatal("request cap bypassed")
	}
	r := httptest.NewRequest("GET", f.in.url, nil)
	r.Header.Set(core.EncoderInputScopeHeader, f.in.key)
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("HTTP admission did not share cap")
	}
	f.in.SealInput()
	for i := 0; i < 2; i++ {
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("sealed read succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("input cancellation did not reach reader")
		}
	}
	if f.g.Reconcile().Pending != 1 || callbacks.Load() != 0 || f.in.InputErr() != core.ErrEncoderInputPending {
		t.Fatal("cancellation manufactured producer retirement")
	}
	unblock()
	for i := 0; i < 2; i++ {
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Fatal("producer did not exit")
		}
	}
	if f.g.Reconcile().Pending != 0 || callbacks.Load() != 1 || f.in.InputErr() != nil {
		t.Fatal("joined producer did not retire exactly once")
	}
	f.g.Reconcile()
	if callbacks.Load() != 1 {
		t.Fatal("duplicate completion")
	}
}

func TestRecoveryRevocationAndRequestDeadline(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "revocation"}[revoke], func(t *testing.T) {
			f := setup(t)
			entered, exited := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			f.src.read = func(ctx context.Context, p []byte, off int64) (int, error) {
				close(entered)
				defer close(exited)
				select {
				case <-ctx.Done():
					return 0, ctx.Err()
				case <-release:
					return f.src.ReadAt(p, off)
				}
			}
			if err := f.in.ClaimInput(func(error) {}); err != nil {
				t.Fatal(err)
			}
			timeout := 100 * time.Millisecond
			if revoke {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			finished := make(chan error, 1)
			go func() { _, err := f.in.ReadRecoveryAt(ctx, "revision", make([]byte, 1), 0); finished <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("source not entered")
			}
			if revoke {
				f.a.mu.Lock()
				delete(f.a.grants, f.grant.Credential)
				f.a.mu.Unlock()
				// Revalidation is performed around each actual source read,
				// once its configured 1ms cadence has elapsed.
				time.Sleep(2 * time.Millisecond)
				close(release)
			}
			select {
			case err := <-finished:
				if (!revoke && !errors.Is(err, context.DeadlineExceeded)) || (revoke && err != ErrDenied) {
					t.Fatal("wrong safe error", err)
				}
			case <-time.After(time.Second):
				t.Fatal("read did not cancel")
			}
			<-exited
			f.in.SealInput()
			if f.g.Reconcile().Pending != 0 {
				t.Fatal("read retained after exit")
			}
		})
	}
}
