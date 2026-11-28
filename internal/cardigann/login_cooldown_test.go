package cardigann

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestFailedLoginCooldownPreventsRepeatedPosts(t *testing.T) {
	site := newPrivateSite(t)
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		if rq.URL.Path == "/takelogin.php" {
			posts.Add(1)
		}
		site.serve(w, rq)
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	r := testRunner(t, loadDef(t, "private.yml"), srv.URL, runnerSetup{
		settings: map[string]string{"username": "alice", "password": "nope"},
	})
	r.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if _, err := r.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "Private Film"}, nil, nil, nil); err == nil {
			t.Fatalf("search %d unexpectedly succeeded", i)
		}
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("login POSTs during cooldown = %d, want 1", got)
	}
	// After cooldown expires, another attempt is allowed.
	now = now.Add(loginFailureCooldown + time.Second)
	_, _ = r.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "Private Film"}, nil, nil, nil)
	if got := posts.Load(); got != 2 {
		t.Fatalf("login POSTs after cooldown = %d, want 2", got)
	}
}

func TestRateLimit429DoesNotTriggerRelogin(t *testing.T) {
	site := newPrivateSite(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		if rq.URL.Path == "/torrents.php" {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("<html>Too many requests</html>"))
			return
		}
		site.serve(w, rq)
	}))
	defer srv.Close()

	r := testRunner(t, loadDef(t, "private.yml"), srv.URL, runnerSetup{settings: aliceSettings})
	for i := 0; i < 3; i++ {
		_, _ = r.Search(context.Background(), core.SearchRequest{Type: core.TypeMovie, Title: "Private Film"}, nil, nil, nil)
	}
	if got := site.loginCount(); got != 1 {
		t.Fatalf("logins on repeated 429 = %d, want 1", got)
	}
}
