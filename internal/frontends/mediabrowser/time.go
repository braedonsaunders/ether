package mediabrowser

import "time"

// wireTime preserves Jellyfin and Emby's 100 ns timestamp precision in UTC.
func wireTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.0000000Z")
}
