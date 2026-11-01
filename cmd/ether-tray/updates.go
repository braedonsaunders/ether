package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/updater"
)

// UpdateChecker is the slice of *updater.Client the tray needs to notice a
// release. The tray never installs the server binary itself: it runs as
// the desktop user (no rights to the install dir or to restart the
// service) and cannot tell whether anyone is streaming. The server's own
// update manager installs updates at a safe moment; the tray only points
// the owner at it.
type UpdateChecker interface {
	Check(ctx context.Context) (*updater.Update, error)
}

// InstallerLauncher downloads, verifies and launches a platform installer
// (the macOS .dmg), which then owns replacing the app.
type InstallerLauncher interface {
	Apply(ctx context.Context, upd *updater.Update, opts updater.ApplyOptions) (string, error)
}

// updateOptions builds feed-client options from whatever settings the tray
// could read; an unreadable or odd config falls back to the stable channel.
func updateOptions(cfg config.Updates, goos string, log *slog.Logger) updater.Options {
	opts := updater.Options{FeedURL: cfg.FeedURL, Channel: cfg.Channel, GOOS: goos, Logger: log}
	if opts.Channel != updater.ChannelBeta {
		opts.Channel = updater.ChannelStable
	}
	if goos == "darwin" {
		// The .dmg is the product on macOS; a bare binary is only news.
		opts.PreferKinds = []string{updater.KindInstaller, updater.KindBinary, updater.KindArchive}
	}
	return opts
}

func newUpdateClient(cfg config.Updates, log *slog.Logger) (*updater.Client, error) {
	return updater.NewClient(updateOptions(cfg, runtime.GOOS, log))
}

func (a *app) platform() string {
	if a.goos != "" {
		return a.goos
	}
	return runtime.GOOS
}

// updatePageURL is the web UI page showing update status and "Update now".
func (a *app) updatePageURL() string { return a.webUIURL() + "maintenance/updates" }

// checkUpdates reports a newer release, if any. It never applies anything.
func (a *app) checkUpdates(ctx context.Context) (*updater.Update, error) {
	if a.updates == nil {
		return nil, fmt.Errorf("update checks unavailable in this build")
	}
	return a.updates.Check(ctx)
}

// openUpdate handles a click on "Update available". On macOS an installer
// release is downloaded, verified and launched (launched=true); everything
// else opens the web UI, where the server installs the update itself.
func (a *app) openUpdate(ctx context.Context, upd *updater.Update) (launched bool, err error) {
	if upd != nil && upd.Artifact != nil && upd.Artifact.Kind == updater.KindInstaller &&
		a.platform() == "darwin" && a.installer != nil {
		if _, err := a.installer.Apply(ctx, upd, updater.ApplyOptions{DownloadDir: installerDir()}); err != nil {
			return false, fmt.Errorf("launch installer for %s: %w", upd.Version(), err)
		}
		return true, nil
	}
	return false, a.openURL(a.updatePageURL())
}

// installerDir is a per-user download location (the tray cannot write to
// the service's data dir). Empty: the system temp dir.
func installerDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "Ether", "updates")
	}
	return ""
}
