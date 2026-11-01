//go:build !darwin

package main

import "fmt"

func launchdLabelDisabled(string) bool { return false }

func enableLaunchdLabel(string) error {
	return fmt.Errorf("launchd is only available on macOS")
}

func startServerDirect(string, string) error {
	return fmt.Errorf("direct start from the tray is only used on macOS")
}

func notifyUser(string, string) {}
