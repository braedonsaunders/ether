package main

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/service"
)

func TestDisabledOverride(t *testing.T) {
	out := `
disabled services = {
	"com.apple.foo" => disabled
	"ether" => disabled
	"io.github.braedonsaunders.ether-tray" => enabled
}
`
	if !disabledOverride(out, "ether") {
		t.Fatal("ether should be disabled")
	}
	if disabledOverride(out, "io.github.braedonsaunders.ether-tray") {
		t.Fatal("tray login item is not the server label")
	}
	if disabledOverride(`"ether" => true`, "ether") != true {
		t.Fatal("=> true is disabled")
	}
	if disabledOverride(`"ether" => false`, "ether") {
		t.Fatal("=> false is not disabled")
	}
}

func TestExplainStartFailure(t *testing.T) {
	reason, can := explainStartFailure(errors.New("launchctl load: input/output error"), true)
	if reason != serviceOffReason || !can {
		t.Fatalf("reason=%q can=%v", reason, can)
	}
	reason, can = explainStartFailure(errors.New("permission denied"), false)
	if can || !strings.Contains(reason, "permission denied") {
		t.Fatalf("reason=%q can=%v", reason, can)
	}
}

func TestStartFailureStaysVisible(t *testing.T) {
	svc := &bootstrapService{state: service.StatusStopped, startErr: errors.New("input/output error")}
	var notes []string
	a := &app{
		goos:            "darwin",
		svc:             svc,
		launchdDisabled: func(string) bool { return true },
		notify:          func(_, body string) { notes = append(notes, body) },
	}
	if err := a.serviceAction("start"); err == nil {
		t.Fatal("expected start error")
	}
	n := a.currentNotice()
	if n.Reason != serviceOffReason || !n.CanEnable {
		t.Fatalf("notice=%+v", n)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Turn it on and start") || !strings.Contains(notes[0], "Start once") {
		t.Fatalf("notes=%v", notes)
	}
	// A later status refresh must not be the only thing the menu can show.
	if statusLine(Status{}) == n.Reason {
		t.Fatal("status line for a down server must stay distinct so the notice is explicit")
	}
}

func TestFixAndStartEnablesThenStarts(t *testing.T) {
	svc := &bootstrapService{state: service.StatusStopped}
	var enabled []string
	a := &app{
		svc: svc,
		enableLaunchd: func(label string) error {
			enabled = append(enabled, label)
			return nil
		},
		launchdDisabled: func(string) bool { return true },
	}
	if err := a.fixAndStart(); err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0] != serviceLabel || !slicesHas(svc.actions, "start") {
		t.Fatalf("enabled=%v actions=%v", enabled, svc.actions)
	}
	if a.currentNotice().Reason != "" {
		t.Fatal("notice should clear after a successful start")
	}
}

func TestTranslocatedBundleStartsDirectly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS App Translocation paths only exist on macOS")
	}
	svc := &bootstrapService{state: service.StatusUnknown, statusErr: errors.New("not installed")}
	direct := 0
	a := &app{
		goos:      "darwin",
		svc:       svc,
		serverExe: "/private/var/folders/xx/AppTranslocation/yy/d/Ether.app/Contents/Resources/ether",
		launchDirectFn: func() error {
			direct++
			return nil
		},
	}
	if err := a.ensureMacServer(t.Context()); err != nil || direct != 1 || len(svc.actions) != 0 {
		t.Fatalf("direct=%d actions=%v err=%v", direct, svc.actions, err)
	}
	if bundleRoot(a.serverExe) != "/private/var/folders/xx/AppTranslocation/yy/d/Ether.app" {
		t.Fatal(bundleRoot(a.serverExe))
	}
}

func TestStartDirectlyDoesNotTouchLaunchd(t *testing.T) {
	svc := &bootstrapService{state: service.StatusStopped, startErr: errors.New("blocked")}
	direct := 0
	a := &app{
		svc: svc,
		launchDirectFn: func() error {
			direct++
			return nil
		},
		launchdDisabled: func(string) bool { return true },
	}
	if err := a.serviceAction("start"); err == nil {
		t.Fatal("expected failure")
	}
	if err := a.startDirectly(); err != nil || direct != 1 || slicesHas(svc.actions, "start") && len(svc.actions) != 1 {
		t.Fatalf("direct=%d actions=%v err=%v", direct, svc.actions, err)
	}
	if a.currentNotice().Reason != "" {
		t.Fatal("direct start should clear the notice")
	}
}

func slicesHas(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
