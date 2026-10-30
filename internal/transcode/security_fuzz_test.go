package transcode

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"
)

func FuzzHLSMediaWriter(f *testing.F) {
	f.Add(uint32(120), uint16(4), uint8(0))
	f.Fuzz(func(t *testing.T, span uint32, segment uint16, format uint8) {
		// Bound output to a realistic finite playlist; numeric mutations still cover zero and fractional durations.
		d := time.Duration(segment%1000+1) * time.Millisecond
		duration := time.Duration(span%128) * d
		container := []string{ContainerFMP4, ContainerMPEGTS, ContainerMatroska}[int(format)%3]
		body := mediaPlaylist(duration, d, container, func(n int) string { return SegmentName(n, container) })
		if !bytes.HasPrefix(body, []byte("#EXTM3U\n")) || !bytes.HasSuffix(body, []byte("#EXT-X-ENDLIST\n")) {
			t.Fatal("invalid framing")
		}
		if strings.Count(string(body), "#EXTINF:") != segmentCount(duration, d) {
			t.Fatal("segment count")
		}
	})
}
func FuzzDASHManifestWriter(f *testing.F) {
	f.Add("session/safe/header", "session/safe/$Number$.m4s", "avc1.640028", int64(120))
	f.Add(`a"<&`, "media?x=1&y=2", "mp4a.40.2", int64(0))
	f.Fuzz(func(t *testing.T, initURL, mediaURL, codec string, duration int64) {
		// Production URLs and codec identifiers are UTF-8; encoding/xml rejects arbitrary binary input.
		if !xmlText(initURL) || !xmlText(mediaURL) || !xmlText(codec) {
			t.Skip()
		}
		body := renderMPD(time.Duration(duration%100000)*time.Millisecond, time.Second, initURL, mediaURL, []dashRep{{ID: 0, Kind: "video", Codecs: codec, Bandwidth: 8000000}, {ID: 1, Kind: "audio", Codecs: "mp4a.40.2", Bandwidth: 128000}})
		decoder := xml.NewDecoder(bytes.NewReader(body))
		templates := 0
		for {
			token, e := decoder.Token()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			if start, ok := token.(xml.StartElement); ok && start.Name.Local == "SegmentTemplate" {
				templates++
				for _, a := range start.Attr {
					if a.Name.Local == "initialization" && a.Value != initURL {
						t.Fatal("initialization escaped incorrectly")
					}
					if a.Name.Local == "media" && a.Value != mediaURL {
						t.Fatal("media escaped incorrectly")
					}
				}
			}
		}
		if templates != 2 {
			t.Fatal("missing templates")
		}
	})
}
func xmlText(s string) bool {
	for _, r := range s {
		if !(r == 9 || r == 10 || r == 13 || r >= 32 && r <= 0xd7ff || r >= 0xe000 && r <= 0xfffd || r >= 0x10000 && r <= 0x10ffff) {
			return false
		}
	}
	return strings.ToValidUTF8(s, "") == s && !strings.ContainsAny(s, "\r\n\t")
}
