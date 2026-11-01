package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeFrontendImages struct {
	path  string
	width int
	calls int
}

func (f *fakeFrontendImages) Get(_ context.Context, path string, width int) ([]byte, string, error) {
	f.path, f.width, f.calls = path, width, f.calls+1
	return []byte("image"), "image/jpeg", nil
}

func TestFrontendImages(t *testing.T) {
	source := &fakeFrontendImages{}
	h := frontendImages{source: source}
	req := httptest.NewRequest(http.MethodGet, "/?path=%2Fposter.jpg&w=300", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "image" || source.path != "/poster.jpg" || source.width != 300 || w.Header().Get("Content-Type") != "image/jpeg" || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("response %d %s source=%+v", w.Code, w.Body, source)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/?path=%2Fposter.jpg&w=99999", nil))
	if w.Code != http.StatusOK || w.Body.Len() != 0 || source.width != 500 || w.Header().Get("Content-Length") != "5" {
		t.Fatal(w.Code, w.Body.String(), source.width)
	}
	before := source.calls
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?path=https%3A%2F%2Fexample.com%2Fx", nil))
	if w.Code != http.StatusBadRequest || source.calls != before {
		t.Fatal("unsafe path forwarded", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/?path=%2Fposter.jpg", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}
