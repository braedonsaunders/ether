package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// Search must return while inference is stalled, coalesce repeated requests,
// survive request cancellation and reject completions from an old config.
func TestVibeRefinementCacheLifecycle(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"vibe\":true,\"languages\":[\"Telugu\",\"Tamil\",\"Malayalam\",\"Kannada\",\"te\",\"Hindi\",\"English\",\"French\"]}"}}]}`)
	}))
	defer server.Close()
	defer unblock()
	cfg := config.LLM{Enabled: true, BaseURL: server.URL, Model: "test"}
	s := New(ServiceOptions{Client: NewClient(ClientOptions{Config: cfg}), Catalog: fakeCatalog{}, CallTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.RefineVibe(ctx, nil, "regional cinema") }()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("search waited for inference")
	}
	cancel()
	waitFor(t, func() bool { return calls.Load() == 1 })
	for range 10 {
		s.RefineVibe(context.Background(), nil, "regional cinema")
	}
	s.mu.Lock()
	if len(s.vibeFlights) != 1 {
		t.Error("requests were not coalesced")
	}
	s.mu.Unlock()
	unblock()
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.vibeFlights) == 0 })
	v, ok, err := s.RefineVibe(context.Background(), nil, "regional cinema")
	if err != nil || !ok || len(v.Filter.Languages) != 6 || v.Filter.Languages[0] != "te" || v.Filter.Languages[3] != "kn" {
		t.Fatalf("cached language constraints: %+v %v %v", v, ok, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("model calls = %d", calls.Load())
	}
	// Simulate a completion from the generation preceding the settings change.
	cfg.Model = "new-model"
	s.UpdateConfig(cfg)
	s.refreshVibe("regional cinema", "regional cinema", 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.vibeCache) != 0 {
		t.Fatal("old model repopulated cache")
	}
}
