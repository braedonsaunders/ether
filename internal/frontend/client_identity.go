package frontend

// JellyfinAndroidTVClient identifies the names used by official Android TV
// releases. The 0.18–0.20 SDKs rotate their device ID after login.
func JellyfinAndroidTVClient(client string) bool {
	switch client {
	case "Android TV", "Jellyfin Android TV", "Jellyfin Android TV (debug)", "Jellyfin for Android TV", "Jellyfin for Android TV (debug)":
		return true
	default:
		return false
	}
}
