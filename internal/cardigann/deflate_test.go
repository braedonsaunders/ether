package cardigann

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HTTP "deflate" is zlib-wrapped; raw DEFLATE from non-conforming servers
// must still decode.
func TestDeflateResponseZlibAndRaw(t *testing.T) {
	const body = "<html>deflated release list</html>"
	for name, enc := range map[string]func(*bytes.Buffer) interface {
		Write([]byte) (int, error)
		Close() error
	}{
		"zlib": func(b *bytes.Buffer) interface {
			Write([]byte) (int, error)
			Close() error
		} {
			return zlib.NewWriter(b)
		},
		"raw": func(b *bytes.Buffer) interface {
			Write([]byte) (int, error)
			Close() error
		} {
			w, _ := flate.NewWriter(b, flate.DefaultCompression)
			return w
		},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			w := enc(&buf)
			w.Write([]byte(body))
			w.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Content-Encoding", "deflate")
				rw.Write(buf.Bytes())
			}))
			defer srv.Close()
			s := newHTTPSession(&Definition{Name: "t"}, nil, nil, nil)
			// A manual Accept-Encoding disables the transport's own decoding.
			resp, err := s.do(context.Background(), request{method: http.MethodGet, url: srv.URL, headers: http.Header{"Accept-Encoding": {"deflate"}}})
			if err != nil {
				t.Fatal(err)
			}
			if string(resp.body) != body {
				t.Fatalf("body = %q", resp.body)
			}
		})
	}
}
