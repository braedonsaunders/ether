//go:build !race

package livetv

import "time"

// guideBound is the latency ceiling for an indexed current-programme lookup.
// Race builds use a wider ceiling; this one stays the normal-run bound.
func guideBound(d time.Duration) time.Duration { return d }
