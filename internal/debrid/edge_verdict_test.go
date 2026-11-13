package debrid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestProbeLinkVerdicts pins the three-way evidence ladder one probe can
// return. The degraded class is deliberately narrow: it rests on exactly
// what old main's 64-byte probe demanded — a valid 206 for exactly the
// promised range, plus the eligible first bytes — and it labels the
// INCOMPLETE remainder honestly. Sixty-four bytes followed by silence is
// degraded (compatible route, nothing more), never "sustained flow";
// EOF before eligibility, and silence before any byte, are rejections;
// EOF exactly at the promised end is a legitimate completion.
func TestProbeLinkVerdicts(t *testing.T) {
	const size = 4 << 20
	tests := []struct {
		name       string
		off        int64
		chunk      int
		pause      time.Duration
		truncate   bool // stop after the first chunk: EOF mid-promise
		silenceAll bool // headers, then nothing at all until the budget dies
		want       probeVerdict
	}{
		{name: "whole sample fast", off: 1 << 20, chunk: 64 << 10, want: probeApproved},
		{name: "sixty-four bytes then silence", off: 1 << 20, chunk: 64, pause: 5 * time.Second, want: probeDegraded},
		{name: "observed band dribble never completes", off: 1 << 20, chunk: 6 << 10, pause: 100 * time.Millisecond, want: probeDegraded},
		{name: "EOF after the eligible bytes truncates", off: 1 << 20, chunk: 64 << 10, truncate: true, want: probeDegraded},
		{name: "EOF before the eligible bytes", off: 1 << 20, chunk: 32, truncate: true, want: probeRejected},
		{name: "silence after headers delivers nothing", off: 1 << 20, silenceAll: true, want: probeRejected},
		{name: "short tail completes in slow dribbles", off: size - 4096, chunk: 64, pause: 10 * time.Millisecond, want: probeApproved},
	}
	h := newHarness(t, size, "torbox")
	e := NewEngine(h.svc, EngineOptions{BlockSize: testBlock, HedgeDelay: -1, Logger: h.svc.log})
	defer e.Close()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var lo, hi int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &lo, &hi); err != nil {
					t.Errorf("probe sent an unparsable range: %q", r.Header.Get("Range"))
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, size))
				w.WriteHeader(http.StatusPartialContent)
				flusher, _ := w.(http.Flusher)
				if tt.silenceAll {
					<-r.Context().Done()
					return
				}
				chunk := make([]byte, tt.chunk)
				for written := int64(0); written < hi-lo+1; written += int64(len(chunk)) {
					if want := hi - lo + 1 - written; want < int64(len(chunk)) {
						chunk = chunk[:want]
					}
					if _, err := w.Write(chunk); err != nil {
						return
					}
					if flusher != nil {
						flusher.Flush()
					}
					if tt.truncate {
						return // EOF with the promised range unfinished
					}
					select {
					case <-r.Context().Done():
						return
					case <-time.After(tt.pause):
					}
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), edgeProbeBudget)
			defer cancel()
			if got := e.probeLink(ctx, srv.URL+"/dl/link?auth=xyz", tt.off, size); got != tt.want {
				t.Fatalf("verdict=%v, want %v", got, tt.want)
			}
		})
	}
}
