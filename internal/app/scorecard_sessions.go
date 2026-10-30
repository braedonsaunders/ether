//go:build scorecard && !plexparity

package app

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type embeddedScorecardSessions interface{ frontend.Sessions }
type scorecardSessionWrapper struct {
	embeddedScorecardSessions
	app *App
}

func scorecardSessions(a *App, s frontend.Sessions) frontend.Sessions {
	return scorecardSessionWrapper{embeddedScorecardSessions: s, app: a}
}
func scorecardErrorCategory(err error) string {
	for _, entry := range []struct {
		err  error
		name string
	}{{core.ErrNotAvailable, "not_available"}, {core.ErrNotFound, "not_found"}, {core.ErrUnauthorized, "unauthorized"}, {core.ErrStreamLimit, "stream_limit"}, {frontend.ErrTranscodeCapacity, "transcode_capacity"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "canceled"}} {
		if errors.Is(err, entry.err) {
			return entry.name
		}
	}
	return "other"
}
func (s scorecardSessionWrapper) failure(stage string, err error) {
	if err != nil {
		s.app.log.Info("scorecard session stage failure", "stage", stage, "category", scorecardErrorCategory(err))
	}
}
func (s scorecardSessionWrapper) Plan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	v, e := s.embeddedScorecardSessions.Plan(ctx, r)
	s.failure("plan", e)
	return v, e
}
func (s scorecardSessionWrapper) DetailPlan(ctx context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if planner, ok := s.embeddedScorecardSessions.(frontend.DetailPlanner); ok {
		v, e := planner.DetailPlan(ctx, r)
		s.failure("detail_plan", e)
		return v, e
	}
	return frontend.PlayPlan{}, core.ErrNotAvailable
}
func (s scorecardSessionWrapper) Open(ctx context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	v, e := s.embeddedScorecardSessions.Open(ctx, r)
	s.failure("open", e)
	return v, e
}
func (s scorecardSessionWrapper) StartStream(ctx context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	v, e := s.embeddedScorecardSessions.StartStream(ctx, a, id, r)
	s.failure("start_stream", e)
	return v, e
}
