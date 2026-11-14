package app

import (
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/extras"
)

// buildExtras wires the trailers/extras service. It stays off (and says
// why, once) until the owner enables it and configures a playable source.
func (a *App) buildExtras() {
	var dl *extras.Downloader
	if a.onair != nil {
		dl = a.onair.Downloader()
	}
	cacheDir := ""
	if root := a.cfg.Read().Cache.Dir; root != "" {
		cacheDir = filepath.Join(root, "trailers")
	}
	a.extras = extras.New(extras.Options{
		Videos:     a.tmdb,
		CacheDir:   cacheDir,
		Config:     func() config.Extras { return a.cfg.Read().Extras },
		Logger:     a.log.With("component", "extras"),
		ToolsDir:   filepath.Join(a.opts.DataDir, "tools"),
		Downloader: dl,
	})
	if reason := a.extras.DisabledReason(); reason != "" {
		a.log.Info("extras off", "reason", reason)
	} else {
		a.log.Info("extras on", "command", a.extras.CommandAvailable())
	}
}
