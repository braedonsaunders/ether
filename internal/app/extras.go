package app

import (
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/extras"
)

// buildExtras wires the trailers/extras service. It stays off (and says
// why, once) until the owner enables it and configures a playable source.
func (a *App) buildExtras() {
	a.extras = extras.New(extras.Options{
		Videos: a.tmdb,
		Config: func() config.Extras { return a.cfg.Read().Extras },
		Logger: a.log.With("component", "extras"),
	})
	if reason := a.extras.DisabledReason(); reason != "" {
		a.log.Info("extras off", "reason", reason)
	} else {
		a.log.Info("extras on", "command", a.extras.CommandAvailable())
	}
}
