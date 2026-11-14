package wasm

import (
	"context"
	"strings"
	"time"
)

// ViewerRequestPath identifies calls on a viewer's browse/search/playback path.
// Administrative actions, setup and background jobs use owner-granted limits.
func ViewerRequestPath(path string) bool {
	for _, prefix := range []string{"/stream/", "/catalog/", "/meta/", "/subtitles/", "/ether/open", "/ether/metadata/", "/ether/subtitles/", "/ether/markers"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return strings.HasPrefix(path, "/ether/v1/kinds/")
}

// WaitBackground yields before starting work and at each host-service boundary.
func WaitBackground(ctx context.Context, busy func() bool) error {
	for busy != nil && busy() {
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func limitsForRequest(l Limits, path string) Limits {
	if ViewerRequestPath(path) {
		l.Deadline = min(l.Deadline, 30*time.Second)
		l.UnitBudget = min(l.UnitBudget, int64(10000))
	}
	return l
}
