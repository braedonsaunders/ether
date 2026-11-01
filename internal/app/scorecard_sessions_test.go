//go:build scorecard && !plexparity

package app

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestScorecardSessionFailureReportsOnlySanitizedStage(t *testing.T) {
	logs := admin.NewLogBuffer(10)
	a := &App{logs: logs, log: slog.New(logs.Handler())}
	fake := frontendtest.Sessions{PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
		return frontend.PlayPlan{}, fmt.Errorf("secret-token /private/path: %w", core.ErrNotAvailable)
	}}
	_, err := scorecardSessions(a, fake).Plan(context.Background(), frontend.PlanRequest{})
	if err == nil {
		t.Fatal("failure lost")
	}
	events := scorecardEvents(logs.Entries(10, "", ""))
	if len(events) != 1 || events[0].Stage != "plan" || events[0].Category != "not_available" {
		t.Fatalf("wrong diagnostic: %+v", events)
	}
}
