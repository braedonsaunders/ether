package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterWaitIsCapped(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status_message":"rate limit"}`))
	}))
	defer srv.Close()

	// Use a short context timeout to verify we do not hang for 86400s and
	// still respect ctx cancellation, while retryafter cap bounds the parsed wait.
	c := New("test-key", nil, WithBaseURL(srv.URL), WithMaxRetries(1))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.MovieDetails(ctx, 1)
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("request waited too long: %v", time.Since(start))
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}
