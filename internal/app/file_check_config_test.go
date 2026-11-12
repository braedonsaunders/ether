package app

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestValidateFileCheckTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, 0, 15 * time.Second} {
		cfg := config.Config{}
		cfg.Plex.FileCheckTimeout = config.Duration(timeout)
		var rejected bool
		for _, err := range validatePlaybackConfig(cfg) {
			if err.Field == "plex.fileCheckTimeout" {
				rejected = true
			}
		}
		if rejected != (timeout < 0) {
			t.Errorf("timeout %v: rejected = %v", timeout, rejected)
		}
	}
}
