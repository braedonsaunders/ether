package silo

import (
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
)

func TestSemanticPlaylistAcceptsDifferentValidSegmentation(t *testing.T) {
	h := http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}}
	a := conform.Response{Status: 200, Headers: h, Body: []byte("#EXTM3U\n#EXT-X-DEFINE:NAME=\"q\",VALUE=\"s=1\"\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\na.ts\n#EXTINF:2,\nb.ts\n#EXT-X-ENDLIST\n")}
	b := conform.Response{Status: 200, Headers: h, Body: []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-DISCONTINUITY\n#EXTINF:4,\nc.ts\n#EXT-X-ENDLIST\n")}
	r := compareSiloPlaylist(a, b)
	if r.Blocking() != 0 || len(r.Differences) == 0 {
		t.Fatalf("valid playlists: %+v", r.Differences)
	}
	b.Body = []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:4,\nc.ts\n#EXT-X-ENDLIST\n")
	if compareSiloPlaylist(a, b).Blocking() == 0 {
		t.Fatal("accepted segment exceeding target duration")
	}
	b.Body = []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:3,\nc.ts\n#EXT-X-ENDLIST\n")
	if compareSiloPlaylist(a, b).Blocking() != 0 {
		t.Fatal("one-second duration tolerance rejected")
	}
	b.Body = []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:2,\nc.ts\n#EXT-X-ENDLIST\n")
	if compareSiloPlaylist(a, b).Blocking() == 0 {
		t.Fatal("accepted different completed duration")
	}
}

func TestSemanticPlaylistValidatesVariantAttributes(t *testing.T) {
	if _, err := parsePlaylist([]byte("#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=640x360\nlow.m3u8\n")); err == nil {
		t.Fatal("accepted variant without bandwidth")
	}
	if _, err := parsePlaylist([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS=\"avc1.1,mp4a.2\"\nlow.m3u8\n")); err != nil {
		t.Fatal(err)
	}
}
