package plex

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

func TestUnauthorizedLogThrottlePreservesWire(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "unchanged")
		writeHTTPError(w, http.StatusUnauthorized)
	})
	handler := s.requestLog(next)
	control := httptest.NewRecorder()
	next.ServeHTTP(control, httptest.NewRequest("GET", "/", nil))
	var wg sync.WaitGroup
	for n := 0; n < 100; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/secret?X-Plex-Token=secret", nil)
			got := httptest.NewRecorder()
			handler.ServeHTTP(got, r)
			if got.Code != control.Code || got.Body.String() != control.Body.String() || !reflect.DeepEqual(got.Header(), control.Header()) {
				t.Error("wire changed")
			}
		}()
	}
	wg.Wait()
	if bytes.Count(logs.Bytes(), []byte("authentication failed")) != 1 || bytes.Contains(logs.Bytes(), []byte("secret")) {
		t.Fatal(logs.String())
	}
}
