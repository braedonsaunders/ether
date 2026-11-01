package admin

import "path/filepath"

func cacheVolumeMounted() bool { return CacheVolumeMounted() }

// ShouldAdoptCacheVolume is true on a fresh install whose cache folder is
// still the default inside the config directory and a /cache volume is mounted.
func ShouldAdoptCacheVolume(setupComplete bool, storage, dir, dataDir string, mounted bool) bool {
	if setupComplete || storage == "shared" || !mounted {
		return false
	}
	def := filepath.Join(dataDir, "cache")
	clean := filepath.Clean(dir)
	return clean == filepath.Clean(def) || clean == "." || dir == ""
}
