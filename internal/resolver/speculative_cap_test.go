package resolver

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A warm-up nobody has pressed Play on checks at most
// speculativeHealthAttempts releases: every check downloads an NZB and
// indexers count grabs (#224). A failed capped warm-up proves nothing, so the
// title is not marked unavailable, and a real Play still checks them all.
func TestSpeculativeWarmUpCapsHealthChecks(t *testing.T) {
	var releases []core.Release
	unhealthy := map[string]bool{}
	for i := range 12 {
		id := fmt.Sprintf("bad%02d", i)
		releases = append(releases, rel(id, fmt.Sprintf("The.Signal.1999.1080p.WEB-DL.H264-G%02d", i), int64(4000+i)))
		unhealthy[id] = true
	}
	var checks atomic.Int64
	svc := New(&fakeIndexer{releases: releases}, fakeHealth{unhealthy: unhealthy, checks: &checks}, nil, testCfg(), Options{})
	defer svc.Close()

	warm, _ := core.WithSpeculative(context.Background())
	if _, err := svc.Resolve(warm, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err == nil {
		t.Fatal("unhealthy releases resolved")
	}
	if n := checks.Load(); n == 0 || n > speculativeHealthAttempts {
		t.Fatalf("warm-up ran %d health checks, want 1..%d", n, speculativeHealthAttempts)
	}
	if ok, known := svc.Available(context.Background(), matrixTitle().Key); known && !ok {
		t.Fatal("a capped warm-up marked the title unavailable")
	}

	checks.Store(0)
	if _, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err == nil {
		t.Fatal("unhealthy releases resolved")
	}
	if n := checks.Load(); n <= speculativeHealthAttempts {
		t.Fatalf("a real Play ran only %d health checks", n)
	}
}
