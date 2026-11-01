package artwork

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestUntrustedArtworkURLCannotReachLoopback(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	}))
	defer srv.Close()
	images := NewImages(ImagesOptions{HTTPClient: srv.Client()})
	if _, _, err := images.GetUntrustedURL(context.Background(), srv.URL); err == nil || requests.Load() != 0 {
		t.Fatal("untrusted logo reached a loopback service")
	}
	if _, _, err := images.GetURL(context.Background(), srv.URL); err != nil || requests.Load() != 1 {
		t.Fatalf("owner-configured artwork changed: %v", err)
	}
}
