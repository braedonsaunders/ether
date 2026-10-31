package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerFailsFastAfterRepeatedOutages(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New("k", nil, WithBaseURL(srv.URL), WithMaxRetries(0), WithRateLimit(1000, 1000))
	ctx := context.Background()
	for i := range breakerThreshold {
		if _, err := c.MovieDetails(ctx, int64(100+i)); err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatalf("request %d: err = %v, want the upstream failure", i, err)
		}
	}
	before := hits.Load()
	start := time.Now()
	if _, err := c.MovieDetails(ctx, 999); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if hits.Load() != before {
		t.Fatal("an open breaker still contacted TMDB")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("an open breaker did not fail fast")
	}
}

func TestBreakerIgnoresDefiniteAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status_message":"not found"}`))
	}))
	defer srv.Close()
	c := New("k", nil, WithBaseURL(srv.URL), WithMaxRetries(0), WithRateLimit(1000, 1000))
	for i := range breakerThreshold + 2 {
		if _, err := c.MovieDetails(context.Background(), int64(200+i)); errors.Is(err, ErrUnavailable) {
			t.Fatalf("404s opened the breaker at request %d", i)
		}
	}
}

func TestBreakerClosesAfterSuccess(t *testing.T) {
	var b breaker
	now := time.Now()
	for range breakerThreshold {
		b.failure(now)
	}
	if !b.open(now) {
		t.Fatal("breaker not open after threshold failures")
	}
	if b.open(now.Add(breakerCooldown)) {
		t.Fatal("breaker still open after the cooldown")
	}
	b.success()
	b.failure(now)
	if b.open(now) {
		t.Fatal("one failure after a success reopened the breaker")
	}
}
