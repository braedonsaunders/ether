//go:build !windows

package admin

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// CacheVolumeMounted reports whether /cache is a mount point. Docker installs
// bind the segment-cache volume there; a plain directory on the root disk is
// not the volume.
func CacheVolumeMounted() bool {
	const volume = "/cache"
	info, err := os.Stat(volume)
	if err != nil || !info.IsDir() {
		return false
	}
	var here, parent unix.Stat_t
	if unix.Stat(volume, &here) != nil || unix.Stat(filepath.Dir(volume), &parent) != nil {
		return false
	}
	return here.Dev != parent.Dev
}
