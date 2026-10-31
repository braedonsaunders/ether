package markers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeAcceptsPublicAndKeyedCalls(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if strings.Contains(gotAuth, "bad") {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := Probe(context.Background(), srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Fatalf("public probe sent auth %q", gotAuth)
	}
	if err := Probe(context.Background(), srv.URL, "good-key"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer good-key" {
		t.Fatalf("auth %q", gotAuth)
	}
	if err := Probe(context.Background(), srv.URL, "bad"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("bad key: %v", err)
	}
}
