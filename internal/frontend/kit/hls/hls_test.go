package hls

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPlaylists(t *testing.T) {
	a, err := NewPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPath()
	if err != nil || a == b {
		t.Fatal(b, err)
	}
	var out bytes.Buffer
	if err := Master(&out, a, 1000); err != nil || !strings.Contains(out.String(), a+"/index.m3u8") {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	if err := Media(&out, a, []time.Duration{1500 * time.Millisecond, 2 * time.Second}); err != nil || !strings.Contains(out.String(), "#EXT-X-TARGETDURATION:2") || !strings.Contains(out.String(), a+"/1.ts") {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	if err := Media(&out, a, []time.Duration{0}); err == nil || out.Len() != 0 {
		t.Fatal(err)
	}
	if err := Master(&out, a+"?token=secret", 1000); err == nil {
		t.Fatal("accepted URI injection")
	}
}
func FuzzMedia(f *testing.F) {
	f.Add(int64(1500000000))
	f.Fuzz(func(t *testing.T, n int64) {
		var out bytes.Buffer
		err := Media(&out, strings.Repeat("a", 64), []time.Duration{time.Duration(n)})
		if n <= 0 && (err == nil || out.Len() != 0) {
			t.Fatal(err)
		}
	})
}
