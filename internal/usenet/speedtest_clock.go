//go:build !windows

package usenet

import "time"

var speedClockOrigin = time.Now()

func speedTransferTime() time.Duration {
	return time.Since(speedClockOrigin)
}
