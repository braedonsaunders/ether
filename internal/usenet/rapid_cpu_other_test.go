//go:build !darwin && !linux

package usenet

import "testing"

// Other platforms retain throughput/allocation measurements without CPU time.
func benchmarkStartCPU(b *testing.B, size int) func() {
	b.ResetTimer()
	return b.StopTimer
}
