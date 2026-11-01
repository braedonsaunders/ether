package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/service"
)

type bootstrapService struct {
	fakeSvc
	state                           service.Status
	statusErr, installErr, startErr error
}

func (s *bootstrapService) Start() error {
	s.actions = append(s.actions, "start")
	return s.startErr
}

func (s *bootstrapService) Status() (service.Status, error) { return s.state, s.statusErr }
func (s *bootstrapService) Install() error {
	s.actions = append(s.actions, "install")
	return s.installErr
}

func TestMacLauncherBootstrapsServer(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		state                 service.Status
		statusErr, installErr error
		want                  []string
		wantErr               bool
	}{
		{name: "fresh", state: service.StatusUnknown, statusErr: errors.New("not installed"), want: []string{"install", "start"}},
		{name: "stopped", state: service.StatusStopped, want: []string{"start"}},
		{name: "running", state: service.StatusRunning},
		{name: "install failed", state: service.StatusUnknown, installErr: errors.New("permission denied"), want: []string{"install"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &bootstrapService{state: tc.state, statusErr: tc.statusErr, installErr: tc.installErr}
			a := &app{goos: "darwin", svc: svc}
			err := a.ensureMacServer(context.Background())
			if (err != nil) != tc.wantErr || !slices.Equal(svc.actions, tc.want) {
				t.Fatalf("actions=%v err=%v", svc.actions, err)
			}
		})
	}
}

func TestMacLauncherLeavesExistingServerAlone(t *testing.T) {
	srv := testServer(t, false)
	svc := &bootstrapService{state: service.StatusUnknown}
	a := &app{goos: "darwin", svc: svc, status: NewStatusClient(srv.URL, srv.Client())}
	if err := a.ensureMacServer(context.Background()); err != nil || len(svc.actions) != 0 {
		t.Fatalf("actions=%v err=%v", svc.actions, err)
	}
	a.goos = "windows"
	a.status = nil
	if err := a.ensureMacServer(context.Background()); err != nil || len(svc.actions) != 0 {
		t.Fatalf("Windows launch actions=%v err=%v", svc.actions, err)
	}
}
