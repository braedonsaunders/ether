//go:build race

package livetv

import "time"

// Race instrumentation stretched the same indexed lookup from the 400ms
// ceiling to about 800ms. Give race builds four times the ceiling so the
// test still fails if the lookup scans the guide history.
func guideBound(d time.Duration) time.Duration { return d * 4 }
