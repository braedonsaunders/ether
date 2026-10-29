package mediabrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type lazyStub struct {
	path       string
	seen       frontend.PlanRequest
	credential string
}

func (s *lazyStub) Issue(_ context.Context, credential string, plan frontend.PlanRequest) (string, error) {
	s.credential = credential
	s.seen = plan
	return s.path, nil
}
func (s *lazyStub) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func TestLazySourceURLUsesTrustedSurfaceAndScopedGrant(t *testing.T) {
	stub := &lazyStub{path: "/sessions/lazy/scoped-secret/file"}
	i := &instance{host: frontend.Host{LazyStreams: stub, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example/jellyfin", nil }}}
	req := httptest.NewRequest("GET", "http://attacker.invalid/Items/1", nil)
	plan := frontend.PlanRequest{Protocol: "jellyfin", Item: 5, Version: "version-id"}
	got := i.lazySourceURL(req, "account-secret", plan)
	if got != "https://ether.example/jellyfin/sessions/lazy/scoped-secret/file" || strings.Contains(got, "account-secret") || stub.credential != "account-secret" || stub.seen.Version != "version-id" {
		t.Fatal(got, stub.seen)
	}
	for _, invalid := range []string{"https://attacker.invalid/file", "/sessions/lazy/../file", "/sessions/lazy/grant/file?api_key=account-secret", "/sessions/lazy/grant/other"} {
		stub.path = invalid
		if got := i.lazySourceURL(req, "account-secret", plan); got != "" {
			t.Fatalf("accepted %q", got)
		}
	}
	rec := httptest.NewRecorder()
	i.lazyMedia(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
}

func TestLazySourceURLFailsClosedWithoutTrustedBase(t *testing.T) {
	for _, base := range []string{"", "ftp://ether.example", "http://user:pass@ether.example", "http://ether.example?token=secret", "//attacker.invalid"} {
		stub := &lazyStub{path: "/sessions/lazy/grant/file"}
		i := &instance{host: frontend.Host{LazyStreams: stub, SurfaceURL: func(context.Context, *http.Request) (string, error) { return base, nil }}}
		got := i.lazySourceURL(httptest.NewRequest("GET", "http://attacker.invalid/", nil), "account-secret", frontend.PlanRequest{})
		if got != "" || stub.credential != "" {
			t.Fatalf("accepted invalid base %q: %q", base, got)
		}
	}
}
