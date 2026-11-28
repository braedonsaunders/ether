package cardigann

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/safefetch"
)

func TestRedirectDropsCredentialsAndRefusesNonPublicTarget(t *testing.T) {
	var gotKey, gotAuth, gotMethod, gotBody string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer other.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer site.Close()

	// Cross-host redirect to another host on loopback/private address is refused by default.
	s := newHTTPSession(&Definition{Name: "t"}, nil, nil, nil)
	siteURL, _ := url.Parse(site.URL)
	otherURL, _ := url.Parse(other.URL)
	siteLocalhost := "http://localhost:" + siteURL.Port()
	_, err := s.do(context.Background(), request{
		method:  http.MethodPost,
		url:     siteLocalhost,
		raw:     "user=a&pass=b",
		headers: http.Header{"X-Api-Key": {"SECRET"}, "Authorization": {"Bearer tok"}},
		follow:  true,
	})
	if !errors.Is(err, safefetch.ErrBlocked) {
		t.Fatalf("redirect to %s: err = %v, want safefetch.ErrBlocked", otherURL.Host, err)
	}

	// When a cross-host redirect target is allowed, credentials and POST body are dropped.
	s.redirectCheck = func(context.Context, *url.URL) error { return nil }
	_, err = s.do(context.Background(), request{
		method:  http.MethodPost,
		url:     site.URL,
		raw:     "user=a&pass=b",
		headers: http.Header{"X-Api-Key": {"SECRET"}, "Authorization": {"Bearer tok"}},
		follow:  true,
	})
	if err != nil {
		t.Fatalf("allowed redirect failed: %v", err)
	}
	if gotKey != "" || gotAuth != "" {
		t.Fatalf("cross-host redirect forwarded headers: X-Api-Key=%q Authorization=%q", gotKey, gotAuth)
	}
	if gotMethod != http.MethodGet || gotBody != "" {
		t.Fatalf("cross-host redirect forwarded POST body: method=%q body=%q", gotMethod, gotBody)
	}
}
