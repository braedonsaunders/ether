package livetv

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/faillog"
)

func TestMaintenancePanicVisibilityAndIsolation(t *testing.T) {
	var logs bytes.Buffer
	now := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	s := &Service{o: Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Now: func() time.Time { return now }}}
	tracker := faillog.New(s.logger(), "livetv maintenance ensureGraphics")
	tracker.Now = func() time.Time { return now }
	for range 20 {
		s.maintenanceStep("ensureGraphics", tracker, func() { panic("scene index out of range") })
	}
	if strings.Count(logs.String(), `"level":"WARN"`) != 1 || !strings.Contains(logs.String(), "maintenanceStep") || !strings.Contains(logs.String(), "stack") {
		t.Fatalf("panic stack missing or repeated without throttle: %s", logs.String())
	}
	progressed := false
	s.maintenanceStep("ensureGraphics", tracker, func() { progressed = true })
	if !progressed || !strings.Contains(logs.String(), "recovered") {
		t.Fatal("maintenance did not recover")
	}
	steps := s.maintenanceSteps()
	steps("expireBroadcasts", func() { panic("expiry panic") })
	now = now.Add(time.Minute)
	steps("refreshReels", func() { panic("refresh panic") })
	steps("ensureGraphics", func() { progressed = true })
	request := httptest.NewRequest("GET", "/diagnostics", nil)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	var body struct {
		Maintenance MaintenanceDiagnostics `json:"maintenance"`
	}
	if response.Code != 200 {
		t.Fatalf("diagnostics status %d", response.Code)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	d := body.Maintenance
	if d.PanicCount != 22 || d.LastPanicSite != "refreshReels" || d.LastPanicMessage != "refresh panic" || !d.LastPanicAt.Equal(now) {
		t.Fatalf("panic diagnostics lost: %+v", d)
	}
	// Failures at different sites must each emit a stack immediately.
	if strings.Count(logs.String(), `"level":"WARN"`) != 3 {
		t.Fatal("maintenance sites share failure throttling")
	}
}
