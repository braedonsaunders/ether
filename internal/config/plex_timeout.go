package config

import "time"

// FileCheckTimeoutOrDefault keeps the frontend and playback check budgets in
// sync, including installations whose saved config predates the setting.
func (p Plex) FileCheckTimeoutOrDefault() time.Duration {
	if p.FileCheckTimeout > 0 {
		return p.FileCheckTimeout.D()
	}
	return 2 * time.Second
}
