package trickplay

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type backend struct {
	mu               sync.Mutex
	ensures, cancels int
	closed           bool
	entered, unblock chan struct{}
}

func (b *backend) EnsureTrickplay(context.Context, core.TrickplaySource) {
	if b.entered != nil {
		b.entered <- struct{}{}
		<-b.unblock
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensures++
}
func (b *backend) CancelTrickplay(string)                            { b.mu.Lock(); defer b.mu.Unlock(); b.cancels++ }
func (*backend) TrickplayAvailable(string) bool                      { return true }
func (*backend) TrickplayBIF(string) ([]byte, bool)                  { return []byte("bif"), true }
func (*backend) TrickplayFrame(string, time.Duration) ([]byte, bool) { return []byte("jpeg"), true }
func (*backend) SetTrickplayFallback(string, []byte)                 {}
func (b *backend) Close() error                                      { b.mu.Lock(); defer b.mu.Unlock(); b.closed = true; return nil }
func (*backend) PreviewWidth() int                                   { return 160 }
func (*backend) PreviewInterval() time.Duration                      { return 5 * time.Second }
func source(id string) core.TrickplaySource {
	return core.TrickplaySource{ReleaseID: id, InputURL: "loopback"}
}
func acquire(t *testing.T, p core.TrickplayProvider, id string) core.TrickplayLease {
	t.Helper()
	l, e := p.(core.TrickplayLeaser).AcquireTrickplay(context.Background(), source(id))
	if e != nil {
		t.Fatal(e)
	}
	return l
}
func TestSharedHoldersAndOwnerOnlyClose(t *testing.T) {
	for _, other := range []string{"jellyfin", "prewarm"} {
		t.Run(other, func(t *testing.T) {
			b := &backend{}
			o := New(b)
			p := o.Borrow()
			defer o.Close()
			if _, ok := p.(io.Closer); ok {
				t.Fatal("borrow exposes Close")
			}
			if p.(interface{ PreviewWidth() int }).PreviewWidth() != 160 || p.(interface{ PreviewInterval() time.Duration }).PreviewInterval() != 5*time.Second {
				t.Fatal("geometry")
			}
			plex := acquire(t, p, "r")
			second := acquire(t, p, "r")
			plex.Release(true)
			p.(core.TrickplayCanceler).CancelTrickplay("r")
			if b.cancels != 0 {
				t.Fatal("Plex canceled shared holder")
			}
			second.Ensure(context.Background(), source("r"))
			second.Release(true)
			second.Release(true)
			if b.cancels != 1 {
				t.Fatalf("last release cancels=%d", b.cancels)
			}
			second.Ensure(context.Background(), source("r"))
			if b.ensures != 3 {
				t.Fatal("released Ensure restarted")
			}
		})
	}
}
func TestIdlePolicy(t *testing.T) {
	b := &backend{}
	o := New(b)
	defer o.Close()
	p := o.Borrow()
	l := acquire(t, p, "r")
	l.Release(false)
	if b.cancels != 0 {
		t.Fatal("nonidle cancel")
	}
	p.(core.TrickplayCanceler).CancelTrickplay("r")
	if b.cancels != 1 {
		t.Fatal("idle cancel")
	}
}
func TestAcquireLinearizesBeforeCancel(t *testing.T) {
	b := &backend{}
	o := New(b)
	defer o.Close()
	p := o.Borrow()
	first := acquire(t, p, "r")
	b.entered = make(chan struct{}, 1)
	b.unblock = make(chan struct{})
	got := make(chan core.TrickplayLease, 1)
	go func() { l, _ := p.(core.TrickplayLeaser).AcquireTrickplay(context.Background(), source("r")); got <- l }()
	<-b.entered
	done := make(chan struct{})
	go func() { first.Release(true); p.(core.TrickplayCanceler).CancelTrickplay("r"); close(done) }()
	close(b.unblock)
	l := <-got
	<-done
	if b.cancels != 0 {
		t.Fatal("last release crossed acquire")
	}
	l.Release(true)
}

func TestHolderStopBeforePublicationAndSwap(t *testing.T) {
	b := &backend{}
	o := New(b)
	defer o.Close()
	p := o.Borrow()
	var h Holder
	h.Close(true)
	if e := h.Update(context.Background(), p, source("r")); e != nil {
		t.Fatal(e)
	}
	if b.ensures != 0 {
		t.Fatal("closed holder acquired")
	}
	var live Holder
	if e := live.Update(context.Background(), p, source("r")); e != nil {
		t.Fatal(e)
	}
	if e := live.Update(context.Background(), p, source("next")); e != nil {
		t.Fatal(e)
	}
	if b.cancels != 1 || o.count != 1 || o.holders["next"] != 1 {
		t.Fatal("swap failed")
	}
	live.Close(true)
}
func TestCapacityAndShutdown(t *testing.T) {
	b := &backend{}
	o := New(b)
	p := o.Borrow()
	leases := make([]core.TrickplayLease, 4096)
	for i := range leases {
		leases[i] = acquire(t, p, "r")
	}
	if _, e := p.(core.TrickplayLeaser).AcquireTrickplay(context.Background(), source("r")); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	if !p.TrickplayAvailable("r") {
		t.Fatal("capacity hid frames")
	}
	o.Close()
	for _, l := range leases {
		l.Release(true)
	}
	if !b.closed || o.count != 0 {
		t.Fatal("shutdown")
	}
	if _, e := p.(core.TrickplayLeaser).AcquireTrickplay(context.Background(), source("r")); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
}
func TestConcurrentEnsureRelease(t *testing.T) {
	b := &backend{}
	o := New(b)
	defer o.Close()
	p := o.Borrow()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var h Holder
			var pair sync.WaitGroup
			pair.Add(2)
			go func() { defer pair.Done(); _ = h.Update(context.Background(), p, source("r")) }()
			go func() { defer pair.Done(); h.Close(true) }()
			pair.Wait()
		}()
	}
	wg.Wait()
	if o.count != 0 {
		t.Fatal("leaked holders")
	}
}

func TestBorrowIdentityAndNil(t *testing.T) {
	var absent *Owner
	if absent.Borrow() != nil || New(nil).Borrow() != nil {
		t.Fatal("absent provider advertised")
	}
	o := New(&backend{})
	defer o.Close()
	if o.Borrow() != o.Borrow() {
		t.Fatal("consumers received different provider views")
	}
}
