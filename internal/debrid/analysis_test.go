package debrid

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestAnalysisAccountConcurrencyAndViewerYield(t *testing.T) {
	now := time.Now()
	s := NewService(config.Debrid{}, Options{Now: func() time.Time { return now }})
	s.entries = []*entry{{cfg: config.DebridProvider{ID: "account", Enabled: true}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	release, err := s.BeginAnalysis(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginAnalysis(ctx); err == nil {
		t.Fatal("two background jobs admitted on one account")
	}
	readCtx, releaseRead, err := s.beginAnalysisRange(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer stop()
	if _, _, err := s.beginAnalysisRange(short, "account"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("parallel account ranges admitted", err)
	}
	s.noteAnalysisLive("account")
	select {
	case <-readCtx.Done():
	default:
		t.Fatal("viewer did not cancel analysis range")
	}
	releaseRead()
	if err = s.checkAnalysisAccount("account"); err == nil {
		t.Fatal("background did not yield to a viewer")
	}
	release()
	if _, err = s.BeginAnalysis(ctx); err == nil {
		t.Fatal("new job admitted while viewer active")
	}
	now = now.Add(2*time.Minute + time.Second)
	release, err = s.BeginAnalysis(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestAnalysis429AccountBackoffAndPacing(t *testing.T) {
	now := time.Now()
	s := NewService(config.Debrid{}, Options{Now: func() time.Time { return now }})
	s.entries = []*entry{{cfg: config.DebridProvider{ID: "account", Enabled: true}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	release, err := s.BeginAnalysis(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err = s.paceAnalysis(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer stop()
	if err = s.paceAnalysis(short, "account"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("background request pacing missing", err)
	}
	s.pauseAnalysisAccount("account", 4*time.Minute)
	if err = s.checkAnalysisAccount("account"); err == nil {
		t.Fatal("429 did not stop account")
	}
	s.analysisMu.Lock()
	first := s.analysis["account"].until
	s.analysisMu.Unlock()
	if first.Before(now.Add(4 * time.Minute)) {
		t.Fatal("Retry-After shortened")
	}
	s.pauseAnalysisAccount("account", 4*time.Minute)
	s.analysisMu.Lock()
	second := s.analysis["account"].until
	s.analysisMu.Unlock()
	if second.Before(now.Add(8 * time.Minute)) {
		t.Fatal("repeated429 did not back off exponentially", first, second)
	}
}
func TestAnalysis429StopsRequestsAcrossFiles(t *testing.T) {
	rig := newEngineRig(t, 16*int(testBlock), -1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	release, err := rig.svc.BeginAnalysis(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var requests atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "240")
		w.WriteHeader(429)
	}))
	defer cdn.Close()
	payload, err := DecodePayload(rig.pl)
	if err != nil {
		t.Fatal(err)
	}
	payload.URL = cdn.URL + "/one"
	file, err := rig.eng.acquireFile(ctx, "limited", payload.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer rig.eng.releaseFile(file)
	_, _, err = file.rangeGet(AnalysisContext(ctx), 0, 100, false)
	if err == nil || requests.Load() != 1 {
		t.Fatal("expected single429", err, requests.Load())
	}
	payload.URL = cdn.URL + "/two"
	other, err := rig.eng.acquireFile(ctx, "other", payload.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer rig.eng.releaseFile(other)
	_, _, err = other.rangeGet(AnalysisContext(ctx), 0, 100, false)
	if err == nil || requests.Load() != 1 {
		t.Fatal("cooldown allowed another account request", err, requests.Load())
	}
}

func TestAnalysisLargeSequentialRangeAndCounters(t *testing.T) {
	rig := newEngineRig(t, 16<<20, -1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	release, err := rig.svc.BeginAnalysis(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	source, err := rig.eng.Open(AnalysisContext(ctx), "bulk", rig.pl)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	reader, ok := source.(core.AnalysisReaderAt)
	if !ok {
		t.Fatal("analysis reader missing")
	}
	data := make([]byte, 8<<20)
	if n, err := reader.ReadAtAnalysisContext(ctx, data, 0); err != nil || n != len(data) {
		t.Fatal(n, err)
	}
	if err := rig.svc.DrainAnalysis(ctx); err != nil {
		t.Fatal(err)
	}
	requests, bytes, failures := source.(core.AnalysisSourceStats).AnalysisStats()
	if requests != 1 || bytes != 8<<20 || failures != 0 {
		t.Fatal("8MiB split or counter wrong", requests, bytes, failures)
	}
}
