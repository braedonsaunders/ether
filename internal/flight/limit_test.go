package flight

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type limitResult struct {
	value int
	err   error
}

func receiveLimit[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("bounded channel wait expired")
		var zero T
		return zero
	}
}
func waitLimit(ctx context.Context, release <-chan struct{}) error {
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("test release timeout")
	}
}

func TestDetachedLimitSharesAtCapacityAndReusesSlot(t *testing.T) {
	for _, callbackErr := range []error{nil, errors.New("upstream failure")} {
		name := "success"
		if callbackErr != nil {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var g Group[string, int]
				var calls atomic.Int32
				started, release := make(chan struct{}), make(chan struct{})
				type valueKey struct{}
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), valueKey{}, "retained"))
				defer cancel()
				fn := func(ctx context.Context) (int, error) {
					calls.Add(1)
					close(started)
					if err := waitLimit(ctx, release); err != nil {
						return 0, err
					}
					if ctx.Value(valueKey{}) != "retained" || ctx.Err() != nil {
						return 0, errors.New("lost detached context")
					}
					return 7, callbackErr
				}
				first, waiter := make(chan limitResult, 1), make(chan limitResult, 1)
				go func() { v, err := g.DoDetachedLimit(ctx, "same", 1, fn); first <- limitResult{v, err} }()
				receiveLimit(t, started)
				go func() {
					v, err := g.DoDetachedLimit(context.Background(), "same", 1, fn)
					waiter <- limitResult{v, err}
				}()
				synctest.Wait()
				if _, err := g.DoDetachedLimit(context.Background(), "other", 1, fn); err != ErrCapacity {
					t.Fatalf("distinct key at capacity: %v", err)
				}
				cancel()
				if got := receiveLimit(t, first); !errors.Is(got.err, context.Canceled) {
					t.Fatalf("caller cancellation: %v", got.err)
				}
				if g.Len() != 1 {
					t.Fatal("caller cancellation released live work")
				}
				if _, err := g.DoDetachedLimit(context.Background(), "other", 1, fn); err != ErrCapacity {
					t.Fatalf("capacity after cancellation: %v", err)
				}
				close(release)
				got := receiveLimit(t, waiter)
				if got.value != 7 || got.err != callbackErr || calls.Load() != 1 || g.Len() != 0 {
					t.Fatalf("result=%+v calls=%d pending=%d", got, calls.Load(), g.Len())
				}
				v, err := g.DoDetachedLimit(context.Background(), "other", 1, func(context.Context) (int, error) { return 9, nil })
				if v != 9 || err != nil || g.Len() != 0 {
					t.Fatalf("reuse: %d %v pending=%d", v, err, g.Len())
				}
			})
		})
	}
}

func TestDetachedLimitNonpositiveIsUnlimited(t *testing.T) {
	for _, max := range []int{0, -1} {
		synctest.Test(t, func(t *testing.T) {
			var g Group[int, int]
			release := make(chan struct{})
			results := make(chan limitResult, 3)
			var calls atomic.Int32
			for key := range 3 {
				go func() {
					v, err := g.DoDetachedLimit(context.Background(), key, max, func(ctx context.Context) (int, error) { calls.Add(1); return key, waitLimit(ctx, release) })
					results <- limitResult{v, err}
				}()
			}
			synctest.Wait()
			count, pending := calls.Load(), g.Len()
			close(release)
			for range 3 {
				if got := receiveLimit(t, results); got.err != nil {
					t.Error(got.err)
				}
			}
			if count != 3 || pending != 3 || g.Len() != 0 {
				t.Fatalf("max=%d calls=%d pending=%d remaining=%d", max, count, pending, g.Len())
			}
		})
	}
}

func TestDetachedLimitCanceledBeforeAdmission(t *testing.T) {
	var g Group[string, int]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := g.DoDetachedLimit(ctx, "key", 1, func(context.Context) (int, error) { t.Fatal("canceled caller ran callback"); return 0, nil })
	if !errors.Is(err, context.Canceled) || g.Len() != 0 {
		t.Fatalf("err=%v pending=%d", err, g.Len())
	}
}
