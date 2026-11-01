package debrid

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Injected latency of the head of a file at Play. A provider API call
// (link generation) costs apiLatency and every CDN request costs
// cdnLatency before headers. The play test sums those delays. It does not
// compare wall clocks: a loaded runner can make the shorter path look slower.
const (
	apiLatency = 120 * time.Millisecond
	cdnLatency = 40 * time.Millisecond
)

type ttfbScenario struct {
	name    string
	prepare func(r *engineRig) []byte // returns the payload Play opens with
}

var ttfbScenarios = []ttfbScenario{
	{"expired-link", func(r *engineRig) []byte {
		p, _ := DecodePayload(r.pl)
		p.Expires = time.Now().Add(-time.Hour)
		r.svc.db = nil // no fresher stored link: Play must call the API
		return p.Encode()
	}},
	{"fresh-link", func(r *engineRig) []byte { return r.pl }},
	{"prewarmed", func(r *engineRig) []byte {
		// What the detail-page prewarm does: health check + prefetch.
		if ok, err := r.eng.Check(context.Background(), r.pl); !ok || err != nil {
			panic(err)
		}
		if err := r.eng.Prefetch(context.Background(), r.rel.ID, r.pl, 0, 1<<20); err != nil {
			panic(err)
		}
		return r.pl
	}},
}

// headTTFB is the wall-clock cost of opening and reading the first bytes.
// The benchmark uses it. Play-shape tests use headInjected so a slow runner
// cannot reorder the cases.
func headTTFB(tb testing.TB, sc ttfbScenario) time.Duration {
	start := time.Now()
	playHead(tb, sc, nil)
	return time.Since(start)
}

// headInjected is the latency the fakes added during Play: provider Link
// delay plus CDN header delay. It does not sleep.
func headInjected(tb testing.TB, sc ttfbScenario) time.Duration {
	var waited atomic.Int64
	playHead(tb, sc, &waited)
	return time.Duration(waited.Load())
}

func playHead(tb testing.TB, sc ttfbScenario, waited *atomic.Int64) {
	tb.Helper()
	r := newEngineRig(tb, 8<<20, -1)
	r.media.latency = cdnLatency
	p := r.provs["realdebrid-0"]
	p.delay = apiLatency
	if waited != nil {
		// Read-ahead starts a variable number of extra range requests before
		// the first byte returns. Hold it off so the counted delays are the
		// link refresh and the one range GET the reader is waiting on.
		cfg := r.svc.Config()
		cfg.ReadAheadChunks = -1
		r.svc.UpdateConfig(cfg)
		sleep := func(d time.Duration) { waited.Add(int64(d)) }
		r.media.sleep = sleep
		p.sleep = sleep
	}
	pl := sc.prepare(r)
	if waited != nil {
		// Check and Prefetch run before Play. Only the open and the first read count.
		waited.Store(0)
	}
	src, err := r.eng.Open(context.Background(), r.rel.ID, pl)
	if err != nil {
		tb.Fatal(err)
	}
	defer src.Close()
	buf := make([]byte, 32<<10)
	if _, err := src.ReadAt(buf, 0); err != nil {
		tb.Fatal(err)
	}
}

func TestHeadTTFBAtPlay(t *testing.T) {
	got := map[string]time.Duration{}
	for _, sc := range ttfbScenarios {
		got[sc.name] = headInjected(t, sc)
		t.Logf("%-12s injected head delay at Play: %v", sc.name, got[sc.name])
	}
	if got["prewarmed"] != 0 || got["fresh-link"] != cdnLatency || got["expired-link"] != apiLatency+cdnLatency {
		t.Fatalf("injected delay = %v, want prewarmed 0, fresh-link %v, expired-link %v", got, cdnLatency, apiLatency+cdnLatency)
	}
}

func BenchmarkHeadTTFB(b *testing.B) {
	for _, sc := range ttfbScenarios {
		b.Run(sc.name, func(b *testing.B) {
			var total time.Duration
			for i := 0; i < b.N; i++ {
				total += headTTFB(b, sc)
			}
			b.ReportMetric(float64(total.Microseconds())/float64(b.N)/1000, "ms/ttfb")
		})
	}
}
