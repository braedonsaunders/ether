//go:build windows

package admin

// CacheVolumeMounted is always false on Windows: the /cache volume is a
// Docker (Linux) bind mount.
func CacheVolumeMounted() bool { return false }
