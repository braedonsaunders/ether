package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/plextv"
	"time"
)

func (a *App) refreshCompatibility(ctx context.Context) {
	if !a.cfg.Read().Plex.TrackLatestVersion {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version, err := plextv.LatestPublicVersion(ctx, nil, "")
	if err != nil {
		a.log.Debug("PMS compatibility release check", "err", err)
		return
	}
	current := a.cfg.Read().Plex.LatestCompatibilityVersion
	if current == version || current != "" && !plextv.NewerPMSVersion(version, current) {
		return
	}
	if err = a.cfg.Update(func(c *config.Config) error { c.Plex.LatestCompatibilityVersion = version; return nil }); err != nil {
		a.log.Warn("save compatibility version", "err", err)
	}
}
