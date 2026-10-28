package debrid

import (
	"context"
	"errors"
	"testing"
)

// A caller may retry as soon as its first block fails. No remaining flight
// from that failed run may still be joinable when the caller wakes.
func TestFailedRunRetiresAllFlightsBeforeWakingReaders(t *testing.T) {
	const count = 4096
	f := &remoteFile{flights: make(map[int64]*blockFlight), raSlots: make(chan struct{}, 1)}
	f.raSlots <- struct{}{} // force the cancelled prefetch down its failure path
	fls := make([]*blockFlight, count)
	for i := range fls {
		fls[i] = &blockFlight{done: make(chan struct{})}
		f.flights[int64(i)] = fls[i]
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.fetchRun(ctx, 0, fls, fetchPrefetch, false)
	}()
	<-fls[0].done
	f.bmu.Lock()
	remaining := len(f.flights)
	f.bmu.Unlock()
	<-finished
	if remaining != 0 {
		t.Fatalf("first reader woke with %d failed flights still joinable", remaining)
	}
	for i, fl := range fls {
		select {
		case <-fl.done:
		default:
			t.Fatalf("block %d was not notified", i)
		}
		if !errors.Is(fl.err, errRunCancelled) {
			t.Fatalf("block %d error = %v", i, fl.err)
		}
	}
}
