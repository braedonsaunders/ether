package hls

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func FuzzHLSWriters(f *testing.F) {
	f.Add(strings.Repeat("a", 64), int64(4000000000), int64(8000000))
	f.Add("\n#EXT-X-KEY:evil", int64(-1), int64(0))
	f.Fuzz(func(t *testing.T, path string, duration, bandwidth int64) {
		var master, media bytes.Buffer
		if e := Master(&master, path, int(bandwidth)); e == nil {
			if !validPath(path) || !strings.HasPrefix(master.String(), "#EXTM3U\n") {
				t.Fatal("invalid master")
			}
		}
		if e := Media(&media, path, []time.Duration{time.Duration(duration)}); e == nil {
			if !validPath(path) || duration <= 0 || strings.Count(media.String(), "#EXTINF:") != 1 {
				t.Fatal("invalid media")
			}
		}
	})
}
