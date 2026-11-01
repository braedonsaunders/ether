//go:build scorecard && !plexparity

package app

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestScorecardProfileDiagnosticOnlyReportsAllowedEnums(t *testing.T) {
	logs := admin.NewLogBuffer(20)
	a := &App{logs: logs, log: slog.New(logs.Handler())}
	callback := scorecardProfileDiagnostic(a)
	values := []sessions.ProfileDiagnostic{sessions.ProfileRequiredConditionUnknown, sessions.ProfileDirectRejected, sessions.ProfileDeliveryRejected, sessions.ProfileFallbackTranscode, sessions.ProfileFallbackRemux, sessions.ProfileNoDelivery}
	want := []string{"required_condition_unknown", "direct_rejected", "delivery_rejected", "fallback_transcode", "fallback_remux", "no_delivery"}
	for _, v := range values {
		callback(v)
	}
	callback(sessions.ProfileDiagnostic("secret-token /private/path"))
	events := scorecardEvents(logs.Entries(20, "", ""))
	if len(events) != len(want) {
		t.Fatalf("event count %d", len(events))
	}
	for i, e := range events {
		if e.Message != "scorecard profile decision" || e.Category != want[i] {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	unsafe := []admin.LogEntry{{Message: "scorecard profile decision", Attrs: map[string]string{"category": "secret-token /private/path", "err": "secret-token /private/path", "release_id": "private-release"}}}
	encoded, err := json.Marshal(scorecardEvents(unsafe))
	if err != nil || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "/private") || strings.Contains(string(encoded), "private-release") {
		t.Fatalf("unsafe event: %s %v", encoded, err)
	}
}
