//go:build plexparity

package transcode

import "time"

// parityManifestTime fixes only the two wall-clock attributes of live DASH
// manifests in staged parity binaries; production transcoder timing is unchanged.
func parityManifestTime() time.Time {
	return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
}
