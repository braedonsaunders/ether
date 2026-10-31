package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDetectRestart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		container   bool
		goos        string
		platform    string
		want        Restart
	}{
		// Finding 8: systemd (RestartSec=120 in kardianos units) and launchd
		// restart in place, with no supervisor delay.
		{"systemd", false, false, "linux", "linux-systemd", Restart{RestartInPlace, "systemd"}},
		{"launchd", false, false, "darwin", "darwin-launchd", Restart{RestartInPlace, "launchd"}},
		{"windows service", false, false, "windows", "windows-service", Restart{RestartSupervisor, "windows-service"}},
		{"other manager", false, false, "linux", "linux-upstart", Restart{RestartInPlace, "service"}},
		// Docker runs Ether as PID 1, which kardianos sees as interactive,
		// or under tini, which it sees as supervised: both restart in place.
		{"container pid 1", true, true, "linux", "linux-systemd", Restart{RestartInPlace, "container"}},
		{"container under init", false, true, "linux", "linux-systemd", Restart{RestartInPlace, "container"}},
		{"terminal", true, false, "linux", "linux-systemd", Restart{RestartInPlace, "terminal"}},
		{"mac terminal", true, false, "darwin", "darwin-launchd", Restart{RestartInPlace, "terminal"}},
		{"windows terminal", true, false, "windows", "windows-service", Restart{RestartSpawn, "terminal"}},
	} {
		if got := detectRestart(tc.interactive, tc.container, tc.goos, tc.platform); got != tc.want {
			t.Errorf("%s: detectRestart = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestInContainerOverride(t *testing.T) {
	t.Setenv("ETHER_IN_CONTAINER", "1")
	if !InContainer() {
		t.Fatal("ETHER_IN_CONTAINER=1 not honoured")
	}
}

func TestProgramRestartsInPlaceBeforeExiting(t *testing.T) {
	rec := newExitRecorder()
	tried := make(chan struct{}, 1)
	p := &program{stopTimeout: time.Second, exit: rec.exit,
		run:     func(context.Context) error { return fmt.Errorf("restore: %w", ErrRestartRequested) },
		inPlace: func() error { tried <- struct{}{}; return errors.New("exec failed") },
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	// A failed in-place restart still exits for the supervisor.
	if code := rec.wait(t); code != ExitCodeRestart {
		t.Fatalf("exit code = %d, want %d", code, ExitCodeRestart)
	}
	select {
	case <-tried:
	default:
		t.Fatal("in-place restart not attempted")
	}
	<-p.done
}
