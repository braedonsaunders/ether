package usenet

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

// warmReused opens three connections and uses one, so the next request
// reuses it (the pool is LIFO) and the other two stay idle.
func warmReused(t *testing.T, s *nntptest.Server, o Options) *Fetcher {
	t.Helper()
	addSeg(t, s, "ok@x", []byte("movie"))
	f := newFetcher(t, []config.UsenetProvider{s.Provider("p", 3)}, o)
	if err := f.Warm(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if data, err := f.Body(context.Background(), "ok@x", nil, core.PriorityLive); err != nil || string(data) != "movie" {
		t.Fatalf("warm-up fetch: %q %v", data, err)
	}
	if st := statsFor(f, "p"); st.LiveConnections != 3 || st.IdleConnections != 3 {
		t.Fatalf("warm: %+v", st)
	}
	return f
}

// A read timeout on a reused connection is one stalled command, not a sign
// that the server dropped the whole idle set: the idle siblings stay open
// and nothing is redialled.
func TestTimeoutOnReusedConnectionKeepsIdleSiblings(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "slow@x", []byte("movie"))
	// DialTimeout is the greeting budget. The 1s command bound and the
	// 30ms idle window apply only after the welcome.
	f := warmReused(t, s, Options{IdleReadTimeout: 30 * time.Millisecond, CommandTimeout: time.Second, DialTimeout: 15 * time.Second, Retries: 0})
	before := statsFor(f, "p")
	s.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Delay: 300 * time.Millisecond}
		}
		return nntptest.Fault{}
	})
	if _, err := f.Body(context.Background(), "slow@x", nil, core.PriorityLive); err == nil {
		t.Fatal("stalled BODY succeeded")
	}
	after := statsFor(f, "p")
	if after.LiveConnections != 2 || after.IdleConnections != 2 {
		t.Fatalf("idle siblings were discarded after a timeout: %+v", after)
	}
	if after.Dials != before.Dials {
		t.Fatalf("timeout caused redials: %d -> %d", before.Dials, after.Dials)
	}
}

// EOF on a reused connection still discards the idle siblings: the server
// dropped them together.
func TestDropOnReusedConnectionDiscardsIdleSiblings(t *testing.T) {
	s := nntptest.Start(t, nntptest.Options{})
	addSeg(t, s, "drop@x", []byte("movie"))
	f := warmReused(t, s, Options{CommandTimeout: time.Second, Retries: 0})
	s.SetFault(func(cmd, arg string) nntptest.Fault {
		if cmd == "BODY" {
			return nntptest.Fault{Drop: true}
		}
		return nntptest.Fault{}
	})
	_, _ = f.Body(context.Background(), "drop@x", nil, core.PriorityLive)
	if st := statsFor(f, "p"); st.IdleConnections != 0 {
		t.Fatalf("idle siblings kept after the server dropped a reused connection: %+v", st)
	}
}
