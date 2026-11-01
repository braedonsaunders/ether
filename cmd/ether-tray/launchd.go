package main

import (
	"fmt"
	"strings"
)

// serviceLabel is the launchd label of the server (not the tray login item).
const serviceLabel = "ether"

const leftoverLaunchReason = "Couldn't start: a leftover launch setting is blocking it."

// startNotice is the durable tray explanation for a failed Start.
// refreshStatus keeps showing it until the server is actually running.
type startNotice struct {
	Reason    string
	CanEnable bool
}

func (a *app) serverBinary() string {
	if a.serverExe != "" {
		return a.serverExe
	}
	return serverExecutable()
}

func (a *app) labelDisabled() bool {
	if a.launchdDisabled != nil {
		return a.launchdDisabled(serviceLabel)
	}
	return launchdLabelDisabled(serviceLabel)
}

func (a *app) currentNotice() startNotice {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	return a.notice
}

func (a *app) clearStartNotice() {
	a.noticeMu.Lock()
	a.notice = startNotice{}
	a.noticeMu.Unlock()
}

func (a *app) recordStartFailure(err error) {
	reason, can := explainStartFailure(err, a.labelDisabled())
	a.noticeMu.Lock()
	changed := a.notice.Reason != reason
	a.notice = startNotice{Reason: reason, CanEnable: can}
	a.noticeMu.Unlock()
	if !changed || reason == "" {
		return
	}
	body := reason
	if can {
		body += " Choose Fix and start, or Start directly."
	} else {
		body += " Choose Start directly."
	}
	if a.notify != nil {
		a.notify("Ether", body)
		return
	}
	if a.log != nil {
		notifyUser("Ether", body)
		a.log.Warn("tray: start server", "err", err, "reason", reason)
	}
}

func explainStartFailure(err error, disabled bool) (string, bool) {
	if disabled {
		return leftoverLaunchReason, true
	}
	if err == nil {
		return "Couldn't start the server.", false
	}
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 140 {
		msg = msg[:137] + "..."
	}
	if msg == "" {
		return "Couldn't start the server.", false
	}
	return "Couldn't start: " + msg, false
}

// disabledOverride reports whether launchctl print-disabled marks label off.
// Both current phrasings count: `"ether" => disabled` and `"ether" => true`.
func disabledOverride(output, label string) bool {
	quoted := `"` + label + `"`
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, quoted) {
			continue
		}
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "=> false"), strings.Contains(lower, "=> enabled"):
			return false
		case strings.Contains(lower, "=> disabled"), strings.Contains(lower, "=> true"):
			return true
		}
	}
	return false
}

// fixAndStart clears a disabled launchd override for the server label and
// tries the login item again. The override is only changed when the owner
// chooses this; a failed start never enables it by itself.
func (a *app) fixAndStart() error {
	if a.svc == nil {
		return fmt.Errorf("service control unavailable on this system")
	}
	var err error
	if a.enableLaunchd != nil {
		err = a.enableLaunchd(serviceLabel)
	} else {
		err = enableLaunchdLabel(serviceLabel)
	}
	if err != nil {
		a.recordStartFailure(err)
		return err
	}
	if err = a.svc.Start(); err != nil {
		a.recordStartFailure(err)
		return err
	}
	a.clearStartNotice()
	return nil
}

// startDirectly runs the server binary without launchd. It does not move or
// delete the app bundle.
func (a *app) startDirectly() error {
	if err := a.launchDirect(); err != nil {
		a.recordStartFailure(err)
		return err
	}
	a.clearStartNotice()
	return nil
}

func (a *app) launchDirect() error {
	if a.launchDirectFn != nil {
		return a.launchDirectFn()
	}
	return startServerDirect(a.serverBinary(), a.dataDir)
}
