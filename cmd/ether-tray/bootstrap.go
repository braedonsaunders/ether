package main

import (
	"context"
	"fmt"
	"runtime"

	"github.com/braedonsaunders/ether/internal/service"
	"github.com/braedonsaunders/ether/internal/updater"
)

// A Mac app has no installer to register its server. Bootstrap a per-user
// service on launch, while leaving an already-running development server alone.
func (a *app) ensureMacServer(ctx context.Context) error {
	goos := a.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" || a.svc == nil {
		return nil
	}
	// App Translocation runs a quarantined app from a temporary mount that
	// disappears when the process exits. Pointing launchd at that path, or
	// moving either copy, is what makes the app look deleted. Start the
	// server from the copy macOS already opened and leave both in place.
	if appTranslocated(a.serverBinary()) {
		if a.log != nil {
			a.log.Warn("tray: macOS opened a temporary copy; starting the server directly and leaving the app in place")
		}
		if err := a.launchDirect(); err != nil {
			a.recordStartFailure(err)
			return err
		}
		a.clearStartNotice()
		return nil
	}
	if a.status != nil {
		if st, err := a.pollStatus(ctx); err == nil && st.Running {
			return nil
		}
	}
	st, err := a.svc.Status()
	if err == nil && st == service.StatusRunning {
		return nil
	}
	if err != nil || st == service.StatusUnknown {
		installer, ok := a.svc.(interface{ Install() error })
		if !ok {
			return fmt.Errorf("server service cannot be installed")
		}
		if err := installer.Install(); err != nil {
			a.recordStartFailure(err)
			return fmt.Errorf("install server service: %w", err)
		}
	}
	if err := a.svc.Start(); err != nil {
		a.recordStartFailure(err)
		return err
	}
	a.clearStartNotice()
	return nil
}

// trayVersion is the release this tray was built as ("dev" for development
// builds).
func (a *app) trayVersion() string {
	if a.version != "" {
		return a.version
	}
	return updater.Version
}

// serverIsStale reports whether the running server is an older release than
// the tray. The tray ships in the same bundle or install directory as the
// server, so a newer tray means the bundle was upgraded underneath a server
// process that is still running the previous binary: on macOS the
// LaunchAgent keeps the old, deleted executable (and its bundled ffmpeg
// paths) alive until logout. Only "older" counts: a server that updated its
// own binary is legitimately newer than the tray next to it, and restarting
// it would change nothing. Development builds and unparseable versions
// never qualify.
func serverIsStale(trayVersion string, st Status) bool {
	if !st.Running || st.VersionIsPMS || st.Version == "" || trayVersion == "" || trayVersion == "dev" {
		return false
	}
	cmp, err := updater.CompareVersions(trayVersion, st.Version)
	return err == nil && cmp > 0
}

// restartStaleServer restarts the server service when it still runs an
// older release than this tray (see serverIsStale). The decision is made
// once per tray start, on the first poll that names the server's Ether
// release, so a restart that does not bring the version in line cannot
// loop. A server the service manager is not running (a development server
// started by hand) is left alone.
func (a *app) restartStaleServer(st Status) (restarted bool, err error) {
	if a.svc == nil || !st.Running || st.VersionIsPMS || st.Version == "" {
		return false, nil // not conclusive yet; decide on a later poll
	}
	if !a.staleChecked.CompareAndSwap(false, true) {
		return false, nil
	}
	if !serverIsStale(a.trayVersion(), st) {
		return false, nil
	}
	if svcSt, err := a.svc.Status(); err != nil || svcSt != service.StatusRunning {
		return false, nil
	}
	if a.log != nil {
		a.log.Info("tray: server runs an older release than this install; restarting it",
			"server", st.Version, "installed", a.trayVersion())
	}
	if err := a.svc.Restart(); err != nil {
		return false, fmt.Errorf("restart server after upgrade: %w", err)
	}
	return true, nil
}
