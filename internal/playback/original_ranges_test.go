package playback

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestOriginalBoundedRanges(t *testing.T) {
	data := []byte("0123456789abcdef")
	for _, tc := range []struct {
		rangeHeader, validator string
		status                 int
		body, contentRange     string
	}{
		{"bytes=1-3", "", 206, "123", "bytes 1-3/16"},
		{"bytes=-4", "", 206, "cdef", "bytes 12-15/16"},
		{"bytes=12-", "", 206, "cdef", "bytes 12-15/16"},
		{"bytes=100-", "", 416, "", "bytes */16"},
		{"bytes=bad", "", 416, "", "bytes */16"},
		{"bytes=1-3", `"changed"`, 412, "", ""},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Range", tc.rangeHeader)
		r.Header.Set("If-Range", tc.validator)
		r = r.WithContext(core.WithRangeResponse(r.Context(), core.RangeResponse{StrictRanges: true}))
		out := httptest.NewRecorder()
		read := int64(0)
		err := serveRepresentation(out, r, representation{size: int64(len(data)), etag: `"source"`, body: func(w io.Writer, start, length int64) error {
			read += length
			_, e := io.Copy(w, bytes.NewReader(data[start:start+length]))
			return e
		}})
		if err != nil || out.Code != tc.status || out.Header().Get("Content-Range") != tc.contentRange {
			t.Fatalf("%s: %d %v %v", tc.rangeHeader, out.Code, out.Header(), err)
		}
		if tc.status == 206 && (out.Body.String() != tc.body || read != int64(len(tc.body)) || out.Header().Get("Content-Length") == "") {
			t.Fatalf("unbounded read: %d %q", read, out.Body.String())
		}
		if tc.status != 206 && read != 0 {
			t.Fatal("invalid range read source")
		}
	}
}
