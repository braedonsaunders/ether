package livetv

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/transcode"
)

// standbyTestDecoder has a picture ready at once until it is closed.
type standbyTestDecoder struct {
	mu     sync.Mutex
	closed bool
	served int
}

func (d *standbyTestDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return transcode.RawFrame{}, errors.New("decoder closed")
	}
	if err := ctx.Err(); err != nil {
		return transcode.RawFrame{}, err
	}
	d.served++
	return transcode.RawFrame{Video: []byte{1}}, nil
}

func (d *standbyTestDecoder) frames() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.served
}

func (d *standbyTestDecoder) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

// waitParked waits until the standby has decoded one frame more than its
// buffer holds beyond the read ones: it is then blocked handing that frame
// over, which is where an unadopted standby rests.
func waitParked(t *testing.T, sb *steadyStandby, read int) {
	t.Helper()
	dec := sb.decoder.(*standbyTestDecoder)
	for deadline := time.Now().Add(2 * time.Second); dec.frames() < read+cap(sb.frames)+1; {
		if time.Now().After(deadline) {
			t.Fatal("standby never filled its frame buffer")
		}
		time.Sleep(time.Millisecond)
	}
}

// newParkedStandby opens a standby on a live planner context and waits until
// it is parked on its full frame buffer.
func newParkedStandby(t *testing.T, ctx context.Context) *steadyStandby {
	t.Helper()
	sb := startSteadyStandby(ctx, StreamSource{ID: "backup"}, nil, &standbyTestDecoder{}, 1)
	waitParked(t, sb, 0)
	return sb
}

// closeWithin runs close and reports whether it returned in time. On a hang
// the planner context is cancelled so the goroutine does not outlive the test.
func closeWithin(t *testing.T, cancel context.CancelFunc, stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("close did not return within 2s while the standby's frame buffer was full")
	}
}

// Dropping a standby nobody adopted returns at once, though its decoder is
// parked on a full frame buffer while the broadcast keeps running.
func TestSteadyStandbyCloseWithFullBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sb := newParkedStandby(t, ctx)
	closeWithin(t, cancel, func() { sb.close(nil) })
}

// An adopted standby keeps handing out the frames it buffered, and closing
// it while nobody reads returns at once and ends with an error, never a
// zero frame.
func TestSteadyStandbyAdoptedCloseWithFullBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sb := newParkedStandby(t, ctx)
	d := sb.adopt()
	if _, err := d.Next(ctx); err != nil {
		t.Fatalf("adopted standby: %v", err)
	}
	if _, err := d.Next(ctx); err != nil {
		t.Fatalf("adopted standby after the buffered frame: %v", err)
	}
	waitParked(t, sb, 2)
	closeWithin(t, cancel, func() { d.Close() })
	for range cap(sb.frames) + 1 {
		frame, err := d.Next(ctx)
		if err != nil {
			return
		}
		if frame.Video == nil {
			t.Fatal("closed standby returned a zero frame without an error")
		}
	}
	t.Fatal("closed standby kept returning frames")
}
