//go:build plexparity && conformlab

package app

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			var stops, logouts, profileReads atomic.Int32
			var revoked atomic.Bool
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				resource := func(body string) {
					w.Header().Set("Content-Type", "application/json")
					if encoding == "gzip" {
						w.Header().Set("Content-Encoding", "gzip")
						zipped := gzip.NewWriter(w)
						io.WriteString(zipped, body)
						zipped.Close()
					} else {
						io.WriteString(w, body)
					}
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /api/v2/auth/login":
					resource(`{"access_token":"probe-owned-token"}`)
				case "POST /api/v2/playback/start":
					resource(`{"outcome":"playable","playback_plan":{"session_id":"nested-session"}}`)
				case "DELETE /api/v2/playback/nested-session":
					if r.Header.Get("Authorization") != "Bearer probe-owned-token" || r.Header.Get("X-Silo-Device-Id") != "reference-device" {
						t.Error("cleanup lost exact authority")
					}
					stops.Add(1)
					w.WriteHeader(http.StatusNoContent)
				case "GET /api/v2/profiles":
					profileReads.Add(1)
					if r.Header.Get("Authorization") != "Bearer probe-owned-token" {
						t.Error("protected request lost exact token")
					}
					if revoked.Load() {
						w.WriteHeader(http.StatusUnauthorized)
					} else {
						io.WriteString(w, `{"items":[{"id":"1"}]}`)
					}
				case "GET /api/v2/system/info":
					if r.Header.Get("Authorization") != "" {
						t.Error("health check was authenticated")
					}
					io.WriteString(w, `{}`)
				case "POST /api/v2/auth/logout":
					if r.Header.Get("Authorization") != "Bearer probe-owned-token" {
						t.Error("logout lost exact token")
					}
					revoked.Store(true)
					logouts.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected request", 500)
				}
			}))
			defer backend.Close()
			coverage := &siloLabCoverage{}
			coverage.setPhase("test")
			proxy, cleanup := siloLabProbeProxy(t, backend.URL, "reference-device", "installation", coverage)
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
				var body map[string]any
				err = json.NewDecoder(res.Body).Decode(&body)
				res.Body.Close()
				if err != nil || res.StatusCode != 200 {
					t.Fatalf("proxy altered compressed response: status=%d error=%v", res.StatusCode, err)
				}
			}
			stop()
			if stops.Load() != 1 || logouts.Load() != 1 || profileReads.Load() != 2 {
				t.Fatalf("reference resources not verified: stops=%d logouts=%d profiles=%d", stops.Load(), logouts.Load(), profileReads.Load())
			}
			if len(coverage.receipts) != 2 {
				t.Fatalf("missing method/path/status receipts: %+v", coverage.receipts)
			}
			for _, receipt := range coverage.receipts {
				if receipt.Method != "POST" || receipt.Status != 200 {
					t.Fatal(receipt)
				}
			}
		})
	}
}

func TestSiloLabCoverageExposesBlockedAndUnobservedRequests(t *testing.T) {
	coverage := &siloLabCoverage{}
	coverage.setPhase("live/silo-apple")
	handler := coverage.handler("reference", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "blocked", 503) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v2/auth/login", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/private/secret-grant", nil))
	dir := t.TempDir()
	coverage.save(t, dir, map[string][]conform.Request{"silo-apple": {{Method: "POST", Path: "/api/v2/auth/login"}, {Method: "POST", Path: "/api/v2/playback/start"}}})
	raw, err := os.ReadFile(filepath.Join(dir, "coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-grant") {
		t.Fatal("coverage leaked an unclassified capability path")
	}
	var data struct {
		Summary []struct {
			Phase, Side, Method, Path                                           string
			ExpectedLogical, ObservedNetwork, BlockedNetwork, UnobservedMinimum int
		}
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	login, start := false, false
	for _, row := range data.Summary {
		if row.Phase != "live/silo-apple" || row.Side != "reference" {
			continue
		}
		if row.Path == "/api/v2/auth/login" {
			login = row.ExpectedLogical == 1 && row.ObservedNetwork == 1 && row.BlockedNetwork == 1
		}
		if row.Path == "/api/v2/playback/start" {
			start = row.ExpectedLogical == 1 && row.ObservedNetwork == 0 && row.UnobservedMinimum == 1
		}
	}
	if !login || !start {
		t.Fatalf("blocked/unobserved probes hidden: %s", raw)
	}
}
