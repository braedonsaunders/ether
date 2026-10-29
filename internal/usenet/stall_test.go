package usenet

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestStalledBodyBackfillsAndTripsBreaker(t *testing.T) {
	primary := nntptest.Start(t, nntptest.Options{})
	backfill := nntptest.Start(t, nntptest.Options{})
	for _, s := range []*nntptest.Server{primary, backfill} {
		addSeg(t, s, "a@x", []byte("movie"))
	}
	var bodies atomic.Int32
	primary.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			bodies.Add(1)
			return nntptest.Fault{Delay: 200 * time.Millisecond}
		}
		return nntptest.Fault{}
	})
	p, b := primary.Provider("p", 1), backfill.Provider("b", 1)
	b.Backfill = true
	f := newFetcher(t, []config.UsenetProvider{p, b}, Options{IdleReadTimeout: 30 * time.Millisecond, CommandTimeout: time.Second, Retries: 4, FailThreshold: 2})
	for i := 0; i < 3; i++ {
		data, err := f.Body(context.Background(), "a@x", nil, core.PriorityLive)
		if err != nil || string(data) != "movie" {
			t.Fatalf("backfill: %q %v", data, err)
		}
	}
	if bodies.Load() != 2 {
		t.Fatalf("stalled provider retried before backfill or breaker reset by login: %d BODY requests", bodies.Load())
	}
	if statsFor(f, "p").State != StateBackoff {
		t.Fatal("connected but stalled provider never opened breaker")
	}
}

func TestIdleDeadlineRenewsWithProgress(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &idleReadConn{Conn: a, idle: 80 * time.Millisecond}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer b.Close()
		for i := 0; i < 6; i++ {
			time.Sleep(20 * time.Millisecond)
			if _, err := b.Write([]byte{1}); err != nil {
				return
			}
		}
	}()
	data, err := io.ReadAll(c)
	// A read timeout can leave the peer blocked in Write. Close this end
	// before joining the writer, rather than deferring the close past the join.
	a.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("progress writer did not stop after closing the reader")
	}
	// net.Pipe reports io.ErrClosedPipe when its peer closes, unlike a TCP
	// socket's EOF. That is a normal end after all six progress bytes; the
	// regression this test guards against is an idle timeout before them.
	if (err != nil && !errors.Is(err, io.ErrClosedPipe)) || len(data) != 6 {
		t.Fatalf("healthy progress timed out: %d %v", len(data), err)
	}
}
