//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func launchdLabelDisabled(label string) bool {
	out, err := exec.Command("launchctl", "print-disabled", fmt.Sprintf("gui/%d", os.Getuid())).Output()
	if err != nil {
		return false
	}
	return disabledOverride(string(out), label)
}

func enableLaunchdLabel(label string) error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
	if out, err := exec.Command("launchctl", "enable", target).CombinedOutput(); err != nil {
		msg := string(out)
		if msg == "" {
			return fmt.Errorf("enable launch setting: %w", err)
		}
		return fmt.Errorf("enable launch setting: %s", msg)
	}
	return nil
}

func startServerDirect(exe, dataDir string) error {
	if exe == "" {
		return fmt.Errorf("server executable not found")
	}
	if root := bundleRoot(exe); root != "" && appTranslocated(exe) {
		// Starting the translocated binary is fine. Deleting or relocating
		// the bundle is not.
		_ = root
	}
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	if dataDir != "" {
		cmd.Env = append(os.Environ(), "ETHER_DATA_DIR="+dataDir)
		logDir := filepath.Join(dataDir, "logs")
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			if f, err := os.OpenFile(filepath.Join(logDir, "server-direct.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				cmd.Stdout = f
				cmd.Stderr = f
			}
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func notifyUser(title, body string) {
	_ = exec.Command("osascript", "-e", fmt.Sprintf("display notification %q with title %q", body, title)).Start()
}
