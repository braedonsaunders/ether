//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTurnOnAndStartBootstrapsATemporaryLabel proves the menu action's
// launchd sequence: a disabled user-domain label refuses bootstrap, enable
// clears that override, and bootstrap then runs the job. The label is
// temporary and removed afterward. It must never be the owner's "ether"
// service, whose disabled override is left untouched.
func TestTurnOnAndStartBootstrapsATemporaryLabel(t *testing.T) {
	label := fmt.Sprintf("io.github.braedonsaunders.ether.fixstart-test.%d", os.Getpid())
	if label == serviceLabel || strings.HasSuffix(label, "/"+serviceLabel) {
		t.Fatal("refusing to use the ether label")
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/" + label
	etherBefore := launchdLabelDisabled(serviceLabel)

	dir := t.TempDir()
	plistPath := filepath.Join(dir, label+".plist")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>/bin/sleep</string><string>30</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>
`, label)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		launchctl(t, "bootout", target)
		// enable clears a disabled override for this temporary label only.
		launchctl(t, "enable", target)
	})

	launchctl(t, "disable", target)
	if !launchdLabelDisabled(label) {
		t.Fatal("temporary label should be disabled before Turn it on and start")
	}
	if err := bootstrapLaunchAgent(plistPath); err == nil {
		t.Fatal("bootstrap should fail while the temporary label is disabled")
	}

	if err := enableLaunchdLabel(label); err != nil {
		t.Fatal(err)
	}
	if launchdLabelDisabled(label) {
		t.Fatal("enable should clear the temporary label's disabled override")
	}
	if err := bootstrapLaunchAgent(plistPath); err != nil {
		t.Fatal(err)
	}
	if launchdLabelDisabled(serviceLabel) != etherBefore {
		t.Fatal("the ether launchd override changed")
	}
	var out string
	deadline := time.Now().Add(2 * time.Second)
	for {
		out = launchctl(t, "print", target)
		if strings.Contains(out, "state = running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bootstrapped job is not running:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func launchctl(t *testing.T, args ...string) string {
	t.Helper()
	for _, arg := range args {
		if arg == serviceLabel || strings.HasSuffix(arg, "/"+serviceLabel) {
			t.Fatalf("refusing launchctl %q", args)
		}
	}
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil && args[0] != "bootout" && args[0] != "enable" {
		t.Fatalf("launchctl %v: %v\n%s", args, err, out)
	}
	return string(out)
}
