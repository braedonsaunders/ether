//go:build plexparity && conformlab

package app

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSiloLabCleanupFailureAndSessionAccounting(t *testing.T) {
	active, prepared := siloLabPartitionSessions([]*core.PlaySession{{ID: "active"}, {ID: "prepared", PreparedOnly: true}})
	if len(active) != 1 || active[0].ID != "active" || len(prepared) != 1 || prepared[0].ID != "prepared" {
		t.Fatalf("wrong session accounting: active=%+v prepared=%+v", active, prepared)
	}
	failure := errors.New("stop failed")
	called := ""
	err := siloLabStopActive(active, func(id string) error { called = id; return failure })
	if called != "active" || !errors.Is(err, failure) {
		t.Fatalf("stop failure lost: called=%q error=%v", called, err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		if siloLabCleanupStatus("logout", status) == nil {
			t.Errorf("status %d incorrectly accepted", status)
		}
	}
	if err := siloLabCleanupStatus("logout", http.StatusNoContent); err != nil {
		t.Fatal(err)
	}
}

func TestSiloLabProxyTracksNestedReferenceSession(t *testing.T) {
	var stops, logouts atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v2/auth/login":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"probe-owned-token"}`)
		case "POST /api/v2/playback/start":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"outcome":"playable","playback_plan":{"session_id":"nested-session"}}`)
		case "DELETE /api/v2/playback/nested-session":
			if r.Header.Get("Authorization") != "Bearer probe-owned-token" || r.Header.Get("X-Silo-Device-Id") != "reference-device" {
				t.Error("cleanup lost exact authority")
			}
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/v2/auth/logout":
			if r.Header.Get("Authorization") != "Bearer probe-owned-token" {
				t.Error("logout lost exact token")
			}
			logouts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", 500)
		}
	}))
	defer backend.Close()
	proxy, cleanup := siloLabProbeProxy(t, backend.URL, "reference-device", "installation")
	stop := sync.OnceFunc(cleanup)
	defer stop()
	for _, path := range []string{"/api/v2/auth/login", "/api/v2/playback/start"} {
		req, err := http.NewRequest("POST", proxy+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer probe-owned-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	stop()
	if stops.Load() != 1 || logouts.Load() != 1 {
		t.Fatalf("reference resources not cleaned: stops=%d logouts=%d", stops.Load(), logouts.Load())
	}
}
