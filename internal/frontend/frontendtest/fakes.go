// Package frontendtest supplies configurable fakes for protocol implementations.
package frontendtest

import (
	"context"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// Experience implements frontend.Experience with optional function hooks.
type Experience struct {
	ViewsFunc    func(context.Context, *core.Viewer) ([]frontend.View, error)
	RowsFunc     func(context.Context, frontend.RowRequest) ([]frontend.Row, error)
	ItemFunc     func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error)
	ChildrenFunc func(context.Context, *core.Viewer, core.Device, core.RatingKey, frontend.ItemQuery) (core.Page, error)
	SearchFunc   func(context.Context, *core.Viewer, core.Device, string, frontend.ItemQuery) (core.Page, error)
	UserDataFunc func(context.Context, *core.Viewer, []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error)
}

func (f Experience) Views(c context.Context, v *core.Viewer) ([]frontend.View, error) {
	if f.ViewsFunc != nil {
		return f.ViewsFunc(c, v)
	}
	return nil, nil
}
func (f Experience) Rows(c context.Context, r frontend.RowRequest) ([]frontend.Row, error) {
	if f.RowsFunc != nil {
		return f.RowsFunc(c, r)
	}
	return nil, nil
}
func (f Experience) Item(c context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
	if f.ItemFunc != nil {
		return f.ItemFunc(c, r)
	}
	return frontend.ItemDetail{}, nil
}
func (f Experience) Children(c context.Context, v *core.Viewer, d core.Device, k core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
	if f.ChildrenFunc != nil {
		return f.ChildrenFunc(c, v, d, k, q)
	}
	return core.Page{}, nil
}
func (f Experience) Search(c context.Context, v *core.Viewer, d core.Device, s string, q frontend.ItemQuery) (core.Page, error) {
	if f.SearchFunc != nil {
		return f.SearchFunc(c, v, d, s, q)
	}
	return core.Page{}, nil
}
func (f Experience) UserData(c context.Context, v *core.Viewer, k []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	if f.UserDataFunc != nil {
		return f.UserDataFunc(c, v, k)
	}
	return nil, nil
}

// Sessions implements frontend.Sessions with optional function hooks.
type Sessions struct {
	PlanFunc         func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error)
	OpenFunc         func(context.Context, frontend.OpenRequest) (frontend.Session, error)
	StartStreamFunc  func(context.Context, frontend.SessionAccess, string, frontend.StreamRequest) (string, error)
	ServeMediaFunc   func(http.ResponseWriter, *http.Request)
	ReportFunc       func(context.Context, frontend.SessionAccess, string, frontend.Timeline) error
	StopFunc         func(context.Context, frontend.SessionAccess, string) error
	SessionsFunc     func(context.Context, frontend.SessionAccess) ([]frontend.Session, error)
	RecoverFunc      func(context.Context, string, string, time.Duration) error
	TranscodeForFunc func(context.Context, string) (core.HLSSession, bool)
	EventsFunc       func(context.Context, frontend.EventScope) (<-chan frontend.Event, error)
}

func (f Sessions) Plan(c context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
	if f.PlanFunc != nil {
		return f.PlanFunc(c, r)
	}
	return frontend.PlayPlan{}, nil
}
func (f Sessions) Open(c context.Context, r frontend.OpenRequest) (frontend.Session, error) {
	if f.OpenFunc != nil {
		return f.OpenFunc(c, r)
	}
	return frontend.Session{}, nil
}
func (f Sessions) StartStream(c context.Context, a frontend.SessionAccess, id string, r frontend.StreamRequest) (string, error) {
	if f.StartStreamFunc != nil {
		return f.StartStreamFunc(c, a, id, r)
	}
	return "", nil
}
func (f Sessions) ServeMedia(w http.ResponseWriter, r *http.Request) {
	if f.ServeMediaFunc != nil {
		f.ServeMediaFunc(w, r)
		return
	}
	http.NotFound(w, r)
}
func (f Sessions) Report(c context.Context, a frontend.SessionAccess, id string, t frontend.Timeline) error {
	if f.ReportFunc != nil {
		return f.ReportFunc(c, a, id, t)
	}
	return nil
}
func (f Sessions) Stop(c context.Context, a frontend.SessionAccess, id string) error {
	if f.StopFunc != nil {
		return f.StopFunc(c, a, id)
	}
	return nil
}
func (f Sessions) Sessions(c context.Context, a frontend.SessionAccess) ([]frontend.Session, error) {
	if f.SessionsFunc != nil {
		return f.SessionsFunc(c, a)
	}
	return nil, nil
}
func (f Sessions) Recover(c context.Context, id, url string, at time.Duration) error {
	if f.RecoverFunc != nil {
		return f.RecoverFunc(c, id, url, at)
	}
	return nil
}
func (f Sessions) TranscodeFor(c context.Context, id string) (core.HLSSession, bool) {
	if f.TranscodeForFunc != nil {
		return f.TranscodeForFunc(c, id)
	}
	return nil, false
}
func (f Sessions) Events(c context.Context, s frontend.EventScope) (<-chan frontend.Event, error) {
	if f.EventsFunc != nil {
		return f.EventsFunc(c, s)
	}
	ch := make(chan frontend.Event)
	close(ch)
	return ch, nil
}

var _ frontend.Experience = Experience{}
var _ frontend.Sessions = Sessions{}
