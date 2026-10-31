//go:build plexparity && conformlab

package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

type siloLabRequestReceipt struct {
	Phase, Side, Method, Path, Range string
	Status                           int
}
type siloLabCoverage struct {
	mu       sync.Mutex
	phase    string
	receipts []siloLabRequestReceipt
}

func (c *siloLabCoverage) setPhase(phase string) { c.mu.Lock(); c.phase = phase; c.mu.Unlock() }
func (c *siloLabCoverage) currentPhase() string  { c.mu.Lock(); defer c.mu.Unlock(); return c.phase }
func (c *siloLabCoverage) add(phase, side string, r *http.Request, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.receipts = append(c.receipts, siloLabRequestReceipt{Phase: phase, Side: side, Method: r.Method, Path: siloLabPath(r.URL.Path), Range: r.Header.Get("Range"), Status: status})
}
func siloLabPath(path string) string {
	if strings.HasSuffix(path, ".m3u8") {
		return "${stream_url}"
	}
	if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".m4s") {
		return "${segment_url}"
	}
	for _, known := range []string{"/api/v2/system/info", "/api/v2/auth/login", "/api/v2/auth/logout", "/api/v2/profiles", "/api/v2/home/sections", "/api/v2/user/libraries", "/api/v2/playback/start", "/api/v2/playback/capabilities"} {
		if path == known {
			return path
		}
	}
	if strings.HasPrefix(path, "/api/v2/library/") && strings.HasSuffix(path, "/sections") {
		return "/api/v2/library/${library_id}/sections"
	}
	if strings.HasPrefix(path, "/api/v2/catalog/items/") {
		if strings.HasSuffix(path, "/versions") {
			return "/api/v2/catalog/items/${item_id}/versions"
		}
		return "/api/v2/catalog/items/${item_id}"
	}
	if strings.HasPrefix(path, "/api/v2/playback/") {
		if strings.HasSuffix(path, "/progress") {
			return "/api/v2/playback/${play_session_id}/progress"
		}
		return "/api/v2/playback/${play_session_id}"
	}
	return "/<unclassified>"
}

type siloLabStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *siloLabStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *siloLabStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *siloLabStatusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (c *siloLabCoverage) handler(side string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase := c.currentPhase()
		tracked := &siloLabStatusWriter{ResponseWriter: w}
		next.ServeHTTP(tracked, r)
		if tracked.status == 0 {
			tracked.status = 200
		}
		c.add(phase, side, r, tracked.status)
	})
}
func (c *siloLabCoverage) save(t *testing.T, dir string, requests map[string][]conform.Request) {
	t.Helper()
	c.mu.Lock()
	receipts := append([]siloLabRequestReceipt(nil), c.receipts...)
	c.mu.Unlock()
	type summary struct {
		Phase, Side, Method, Path                                           string
		ExpectedLogical, ObservedNetwork, BlockedNetwork, UnobservedMinimum int
	}
	keys := map[string]*summary{}
	phases := []string{"compare/silo-apple", "compare/silo-android", "live/silo-apple", "live/silo-android"}
	key := func(phase, side, method, path string) string { return phase + " " + side + " " + method + " " + path }
	for _, phase := range phases {
		for _, side := range []string{"reference", "target"} {
			if side == "reference" && strings.HasPrefix(phase, "compare/") {
				continue
			}
			profile := strings.Split(phase, "/")[1]
			for _, req := range requests[profile] {
				if req.FixtureOnly {
					continue
				}
				k := key(phase, side, req.Method, req.Path)
				if keys[k] == nil {
					keys[k] = &summary{Phase: phase, Side: side, Method: req.Method, Path: req.Path}
				}
				keys[k].ExpectedLogical++
			}
		}
	}
	for _, receipt := range receipts {
		k := key(receipt.Phase, receipt.Side, receipt.Method, receipt.Path)
		if keys[k] == nil {
			keys[k] = &summary{Phase: receipt.Phase, Side: receipt.Side, Method: receipt.Method, Path: receipt.Path}
		}
		row := keys[k]
		row.ObservedNetwork++
		if receipt.Status >= 400 {
			row.BlockedNetwork++
		}
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	rows := make([]summary, 0, len(keys))
	for _, k := range ordered {
		row := keys[k]
		row.UnobservedMinimum = max(0, row.ExpectedLogical-row.ObservedNetwork)
		rows = append(rows, *row)
		t.Logf("silo_coverage phase=%s side=%s method=%s path=%s expected_logical=%d observed_network=%d blocked_network=%d unobserved_minimum=%d", row.Phase, row.Side, row.Method, row.Path, row.ExpectedLogical, row.ObservedNetwork, row.BlockedNetwork, row.UnobservedMinimum)
	}
	if dir == "" {
		return
	}
	raw, err := json.MarshalIndent(struct {
		Note     string
		Summary  []summary
		Requests []siloLabRequestReceipt
	}{Note: "Network receipts include range preflights and HLS master follows; counts are not one-to-one proof of logical probes. Unclassified or unobserved requests are explicit.", Summary: rows, Requests: receipts}, "", "  ")
	if err != nil {
		t.Error(err)
		return
	}
	if err = os.MkdirAll(dir, 0700); err == nil {
		err = os.WriteFile(filepath.Join(dir, "coverage.json"), raw, 0600)
	}
	if err != nil {
		t.Error(err)
	}
}

func siloLabLogout(t *testing.T, client *http.Client, base, token, device, label string) {
	t.Helper()
	call := func(method, path string, authorized bool) int {
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			return 0
		}
		req.Header.Set("X-Silo-Device-Id", device)
		if authorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			return 0
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		return res.StatusCode
	}
	before := call("GET", "/api/v2/profiles", true)
	logout := call("POST", "/api/v2/auth/logout", true)
	after := call("GET", "/api/v2/profiles", true)
	health := call("GET", "/api/v2/system/info", false)
	verified := before == 200 && logout >= 200 && logout < 300 && after == 401 && health == 200
	t.Logf("silo_credential_cleanup label=%s device=%s authenticated_before=%d logout_status=%d protected_after=%d anonymous_service_health=%d verified=%t", label, device, before, logout, after, health, verified)
	if !verified {
		t.Error(fmt.Errorf("Silo %s exact-token cleanup not verified", label))
	}
}
