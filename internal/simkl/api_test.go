package simkl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIRequestsAndRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_id") != "id" || r.URL.Query().Get("app-name") != "ether" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing request credentials")
		}
		if r.URL.Path == "/limited" {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(`{"all":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	c := AuthClient{BaseURL: srv.URL, ClientID: "id"}
	var got struct {
		All string `json:"all"`
	}
	if err := c.request(context.Background(), "GET", "/sync/activities", "secret", nil, nil, &got); err != nil || got.All == "" {
		t.Fatalf("request: %v %+v", err, got)
	}
	err := c.request(context.Background(), "GET", "/limited", "secret", nil, nil, nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 429 || he.RetryAfter != "30" {
		t.Fatalf("rate limit: %v", err)
	}
}
