// Package quality exposes the transcoder's published H.264 rung targets
// without a frontend importing the transcoder.
package quality

import "github.com/braedonsaunders/ether/internal/transcode"

// ProfileVideoKbps is the H.264 video target Decide uses for a quality height.
func ProfileVideoKbps(height int) int { return transcode.ProfileVideoKbps(height) }
