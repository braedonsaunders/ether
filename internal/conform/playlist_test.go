package conform

import (
	"net/http"
	"testing"
)

func TestComparePlaylistIgnoresSegmentURLsButChecksStructure(t *testing.T) {
	h := http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}
	a := Response{Status: 200, Headers: h, Body: []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:5,\nsegment-a.ts?st=token\n")}
	b := Response{Status: 200, Headers: h, Body: []byte("#EXTM3U\n#EXT-X-VERSION:4\n#EXTINF:10,\nsegment-b.ts\n")}
	if r := ComparePlaylist(a, b); r.Blocking() != 0 {
		t.Fatalf("media values caused diff: %+v", r)
	}
	b.Body = []byte("#EXTM3U\n#EXT-X-VERSION:4\n")
	if ComparePlaylist(a, b).Blocking() == 0 {
		t.Fatal("missing segment structure accepted")
	}
}
