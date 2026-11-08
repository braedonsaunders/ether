//go:build darwin || linux

package usenet

import (
	"testing"

	"golang.org/x/sys/unix"
)

func benchmarkProcessCPU(b *testing.B) int64 {
	b.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}

// Includes every process thread and GC. 100% means one fully occupied core.
func benchmarkStartCPU(b *testing.B, size int) func() {
	b.Helper()
	b.ResetTimer()
	start := benchmarkProcessCPU(b)
	return func() {
		b.StopTimer()
		used := float64(benchmarkProcessCPU(b) - start)
		b.ReportMetric(used/float64(b.N), "cpu-ns/op")
		b.ReportMetric(100*used/float64(b.Elapsed().Nanoseconds()), "cpu-%")
		b.ReportMetric(used/1e6/(float64(b.N)*float64(size)/(1<<30)), "cpu-ms/GiB")
	}
}
