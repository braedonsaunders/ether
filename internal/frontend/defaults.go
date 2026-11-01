package frontend

// DefaultPort returns the listener port for protocols that require one.
// Silo uses its Compose default; the reference lab's 8098 is an override.
func DefaultPort(id Protocol) (int, bool) {
	switch id {
	case "plex":
		return 32400, true
	case "jellyfin":
		return 8096, true
	case "emby":
		return 8097, true
	case "silo":
		return 8090, true
	default:
		return 0, false
	}
}
