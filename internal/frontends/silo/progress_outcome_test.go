package silo

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFinalSampleUsesAtomicBrokerOutcome(t *testing.T) {
	for _, outcome := range []frontend.ReportOutcome{frontend.ReportApplied, frontend.ReportReplayed, frontend.ReportStale, frontend.ReportConflict} {
		t.Run(string(outcome), func(t *testing.T) {
			viewer := &frontend.Viewer{ID: 7}
			sessions := frontendtest.Sessions{ReportSampleFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, sample frontend.ProgressSample) (frontend.ReportOutcome, error) {
				if !sample.Stop {
					t.Fatal("terminal intent lost")
				}
				return outcome, nil
			}, StopFunc: func(context.Context, frontend.SessionAccess, string) error {
				t.Fatal("sample and stop split across gates")
				return nil
			}}
			inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: testDeviceIdentity(frontendtest.Identity{}, viewer), Sessions: sessions})
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"installation_id":%q,"stop_id":"stop","sequence":2,"position":120,"is_paused":false}`, inst.(*instance).installationID())
			r := httptest.NewRequest("DELETE", "/api/v2/playback/8c2d329e-71d2-4ea1-9b57-95187baeb0aa", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer grant")
			r.Header.Set("X-Silo-Device-Id", "tv")
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			expected := string(outcome)
			status := 200
			if outcome == frontend.ReportApplied || outcome == frontend.ReportReplayed {
				expected = "stopped"
			}
			if outcome == frontend.ReportConflict {
				status = 409
			}
			if w.Code != status || !strings.Contains(w.Body.String(), `"outcome":"`+expected+`"`) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}
