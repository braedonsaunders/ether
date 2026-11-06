package playback

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPlexPartRangeAndConditionalContract(t *testing.T) {
	data := []byte("0123456789")
	for _, tc := range []struct {
		value    string
		status   int
		body, cr string
	}{
		{"", 200, "0123456789", ""}, {"bytes=-", 200, "0123456789", ""},
		{"bytes=0-0", 206, "0", "bytes 0-0/10"}, {"bytes=-1", 206, "9", "bytes 9-9/10"},
		{"bytes=8-", 206, "89", "bytes 8-9/10"}, {"bytes=8-99", 206, "89", "bytes 8-9/10"},
		{"bytes=10-", 416, "", ""}, {"bytes=5-4", 416, "", ""}, {"bytes=5-abc", 416, "", ""},
		{"items=1-2", 206, "12", "bytes 1-2/10"}, {"BYTES=1-2", 206, "12", "bytes 1-2/10"},
		{"bytes=abc-def", 206, "0123456789", "bytes 0-9/10"},
		{"bytes=0-abc", 206, "0", "bytes 0-0/10"},
		{"bytes=-0", 206, "0123456789", "bytes 0-0/10"},
		{"bytes=-x", 206, "0123456789", "bytes 0-0/10"},
		{"bytes=- ", 206, "0123456789", "bytes 0-0/10"},
		{"bytes=0-1,5-6", 206, "0123456789", "bytes 0-9/10"},
		{"bytes=1-2-3", 206, "0123456789", "bytes 0-9/10"},
		{"bytes", 200, "0123456789", ""},
	} {
		t.Run(tc.value, func(t *testing.T) {
			for _, conditional := range []string{"If-Range", "If-None-Match", "If-Match", "If-Modified-Since"} {
				r := httptest.NewRequest(http.MethodGet, "/part", nil)
				r.Header.Set("Range", tc.value)
				r.Header.Set(conditional, `"wrong-tag"`)
				w := httptest.NewRecorder()
				err := servePlexPart(w, r, representation{size: 10, etag: `"tag"`, modTime: time.Unix(1, 0), contentType: "video/mp4", body: func(w io.Writer, start, length int64) error {
					_, e := io.Copy(w, bytes.NewReader(data[start:start+length]))
					return e
				}})
				if err != nil || w.Code != tc.status || w.Header().Get("Content-Range") != tc.cr {
					t.Fatalf("%s: status=%d range=%q err=%v", conditional, w.Code, w.Header().Get("Content-Range"), err)
				}
				if tc.status != 416 && w.Body.String() != tc.body {
					t.Fatalf("body=%q want %q", w.Body.String(), tc.body)
				}
				if tc.status == 416 && (w.Header().Get("Accept-Ranges") != "" || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("Content-Type") != "text/html") {
					t.Fatalf("416 headers %v", w.Header())
				}
			}
		})
	}
}

func TestPlexPartMIMETable(t *testing.T) {
	for ext, want := range map[string]string{"mkv": "video/x-matroska", "webm": "video/webm", "mp4": "video/mp4", "m4v": "video/mp4", "mov": "video/mp4", "ts": "video/MP2T", "mpg": "video/mpeg", "mpeg": "video/mpeg", "flv": "video/x-flv", "wmv": "video/x-ms-wmv", "asf": "video/x-ms-asf", "divx": "video/x-msvideo", "avi": "video/x-msvideo", "m2ts": "application/octet-stream", "mts": "application/octet-stream", "ogv": "application/octet-stream", "3gp": "application/octet-stream", "unknown": "application/octet-stream"} {
		if got := plexPartMIME("stored." + ext); got != want {
			t.Errorf("%s: %s != %s", ext, got, want)
		}
	}
}
