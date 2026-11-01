//go:build scorecard && !plexparity

package app

import (
	"testing"
	"time"
)

func TestScorecardLiveClockAdvances(t *testing.T) {
	frozen := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if got := parityClock()(); !got.Equal(frozen) {
		t.Fatalf("fixture clock = %s", got)
	}
	t.Setenv("SCORECARD_LIVE_CLOCK", "1")
	start := parityClock()()
	time.Sleep(20 * time.Millisecond)
	if got := parityClock()(); !got.After(start) {
		t.Fatalf("live clock = %s, start %s", got, start)
	}
}
