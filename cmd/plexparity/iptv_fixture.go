package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// iptvFixture serves one finite, real MPEG-TS segment entirely on loopback.
// Unknown paths are hard failures, including requests made by FFmpeg.
func iptvFixture(seedDir string, tracker *fixtureTracker) (http.Handler, error) {
	segment, err := os.ReadFile(filepath.Join(seedDir, "iptv.ts"))
	if err != nil {
		return nil, fmt.Errorf("read IPTV fixture segment: %w", err)
	}
	if len(segment) == 0 {
		return nil, fmt.Errorf("empty IPTV fixture segment")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tracker.record(r.Method + " " + r.URL.RequestURI())
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:20\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:20.000,\n/segment.ts\n#EXT-X-ENDLIST\n")
		case "/segment.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			http.ServeContent(w, r, "segment.ts", fixtureNow, bytes.NewReader(segment))
		default:
			tracker.record(r.Method + " " + r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}), nil
}
