package transcode

import (
	"strconv"
	"testing"
	"time"
)

func BenchmarkMediaPlaylist(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			name := func(i int) string { return strconv.Itoa(i) + ".ts" }
			span := time.Duration(n) * 4 * time.Second
			b.ReportAllocs()
			for b.Loop() {
				_ = mediaPlaylist(span, 4*time.Second, "mpegts", name)
			}
		})
	}
}

func BenchmarkEventPlaylist(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			done := make([]int, n)
			for i := range done {
				done[i] = i
			}
			name := func(i int) string { return strconv.Itoa(i) + ".ts" }
			b.ReportAllocs()
			for b.Loop() {
				_ = eventPlaylist(done, 4*time.Second, "mpegts", false, name)
			}
		})
	}
}
