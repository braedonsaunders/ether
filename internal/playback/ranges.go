package playback

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"strconv"
	"strings"
	"time"
)

// byteRange is one satisfiable range of a representation.
type byteRange struct {
	start, length int64
}

func (r byteRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size)
}

var (
	// errRangeInvalid means the Range header is malformed; RFC 9110 says to
	// ignore it and serve the full representation.
	errRangeInvalid = errors.New("invalid range")
	// errRangeUnsatisfiable means no range overlaps the representation.
	errRangeUnsatisfiable = errors.New("range not satisfiable")
)

// maxRanges bounds multi-range requests; more than this is treated as
// malformed (and the full body is served) to avoid amplification.
const maxRanges = 16

// parseRange parses a Range header value against a representation of size
// bytes. It returns nil, nil when the header is empty.
func parseRange(h string, size int64) ([]byteRange, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return nil, nil
	}
	unit, set, ok := strings.Cut(h, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return nil, errRangeInvalid
	}
	var out []byteRange
	specs := 0
	for _, spec := range strings.Split(set, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		specs++
		if specs > maxRanges {
			return nil, errRangeInvalid
		}
		first, last, ok := strings.Cut(spec, "-")
		if !ok {
			return nil, errRangeInvalid
		}
		first, last = strings.TrimSpace(first), strings.TrimSpace(last)
		if first == "" {
			// Suffix range: the last N bytes.
			n, err := parseDigits(last)
			if err != nil {
				return nil, errRangeInvalid
			}
			if n == 0 || size == 0 {
				continue // unsatisfiable on its own
			}
			if n > size {
				n = size
			}
			out = append(out, byteRange{size - n, n})
			continue
		}
		start, err := parseDigits(first)
		if err != nil {
			return nil, errRangeInvalid
		}
		end := size - 1
		if last != "" {
			e, err := parseDigits(last)
			if err != nil || e < start {
				return nil, errRangeInvalid
			}
			if e < end {
				end = e
			}
		}
		if start >= size {
			continue // unsatisfiable on its own
		}
		out = append(out, byteRange{start, end - start + 1})
	}
	if specs == 0 {
		return nil, errRangeInvalid
	}
	if len(out) == 0 {
		return nil, errRangeUnsatisfiable
	}
	return out, nil
}

func parseDigits(s string) (int64, error) {
	if s == "" {
		return 0, errRangeInvalid
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errRangeInvalid
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// ifRangeMatches evaluates an If-Range precondition (RFC 9110 13.1.5). An
// entity tag must match strongly; a date must equal Last-Modified exactly.
func ifRangeMatches(v, etag string, modTime time.Time) bool {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return true
	case strings.HasPrefix(v, "W/"):
		return false // weak tags never match for ranges
	case strings.HasPrefix(v, `"`):
		return etag != "" && v == etag
	}
	t, err := http.ParseTime(v)
	if err != nil || modTime.IsZero() {
		return false
	}
	return t.Equal(modTime.UTC().Truncate(time.Second))
}

// representation describes a byte stream served with range support.
type representation struct {
	size                  int64
	etag                  string
	modTime               time.Time
	contentType           string
	deferStatusOnZeroRead bool
	// body writes length bytes starting at start. An error ends the response
	// early (the client sees a short body and re-requests).
	body func(w io.Writer, start, length int64) error
}

type lazyHeaderWriter struct {
	w      http.ResponseWriter
	status int
	wrote  bool
}

func (l *lazyHeaderWriter) Write(p []byte) (int, error) {
	if !l.wrote {
		l.wrote = true
		l.w.WriteHeader(l.status)
	}
	return l.w.Write(p)
}

func serveSingleRangeBody(w http.ResponseWriter, r *http.Request, status int, start, length int64, deferStatusOnZeroRead bool, body func(w io.Writer, start, length int64) error) error {
	if r.Method == http.MethodHead || length == 0 {
		w.WriteHeader(status)
		return nil
	}
	if !deferStatusOnZeroRead {
		w.WriteHeader(status)
		return body(w, start, length)
	}
	lw := &lazyHeaderWriter{w: w, status: status}
	err := body(lw, start, length)
	if !lw.wrote {
		if err == nil {
			w.WriteHeader(status)
			return nil
		}
		if r.Context().Err() != nil {
			return err
		}
		h := w.Header()
		h.Del("Content-Range")
		h.Del("Content-Length")
		h.Set("Cache-Control", "no-store")
		if errors.Is(err, errEndResponse) {
			http.Error(w, "source changed; restart playback", http.StatusConflict)
			return err
		}
		h.Set("Retry-After", "2")
		http.Error(w, "media source temporarily unavailable", http.StatusServiceUnavailable)
		return err
	}
	return err
}

// serveRepresentation answers GET and HEAD with full Range and If-Range
// semantics, including multipart/byteranges for multi-range requests. It
// never reads outside the requested ranges.
func serveRepresentation(w http.ResponseWriter, r *http.Request, rep representation) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	if rep.etag != "" {
		h.Set("ETag", rep.etag)
	}
	if !rep.modTime.IsZero() {
		h.Set("Last-Modified", rep.modTime.UTC().Format(http.TimeFormat))
	}
	ctype := rep.contentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	var ranges []byteRange
	if rh := r.Header.Get("Range"); rh != "" && ifRangeMatches(r.Header.Get("If-Range"), rep.etag, rep.modTime) {
		rs, err := parseRange(rh, rep.size)
		switch {
		case errors.Is(err, errRangeUnsatisfiable):
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", rep.size))
			http.Error(w, "requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return nil
		case err == nil:
			ranges = rs
		}
		// errRangeInvalid: ignore the header and serve the full body.
	}

	switch len(ranges) {
	case 0:
		h.Set("Content-Type", ctype)
		h.Set("Content-Length", strconv.FormatInt(rep.size, 10))
		return serveSingleRangeBody(w, r, http.StatusOK, 0, rep.size, rep.deferStatusOnZeroRead, rep.body)
	case 1:
		br := ranges[0]
		h.Set("Content-Type", ctype)
		h.Set("Content-Range", br.contentRange(rep.size))
		h.Set("Content-Length", strconv.FormatInt(br.length, 10))
		return serveSingleRangeBody(w, r, http.StatusPartialContent, br.start, br.length, rep.deferStatusOnZeroRead, rep.body)
	}

	mw := multipart.NewWriter(w)
	h.Set("Content-Type", "multipart/byteranges; boundary="+mw.Boundary())
	h.Del("Content-Length")
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return nil
	}
	for _, br := range ranges {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":  {ctype},
			"Content-Range": {br.contentRange(rep.size)},
		})
		if err != nil {
			return err
		}
		if err := rep.body(pw, br.start, br.length); err != nil {
			return err
		}
	}
	return mw.Close()
}

// contentTypeFor picks a MIME type from the container name or file extension.
func contentTypeFor(container, name string) string {
	switch strings.ToLower(container) {
	case "mkv", "matroska":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	case "mp4", "mov", "m4v":
		return "video/mp4"
	case "mpegts", "ts", "m2ts":
		return "video/mp2t"
	case "avi":
		return "video/x-msvideo"
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".ts", ".m2ts", ".mts":
		return "video/mp2t"
	case ".avi":
		return "video/x-msvideo"
	case ".mpg", ".mpeg":
		return "video/mpeg"
	}
	return "application/octet-stream"
}

// spliceable reports whether a container tolerates a byte stream switching
// to a different file mid-response. Only MPEG-TS does: it is a sequence of
// self-contained 188-byte packets with in-band codec parameters, so a
// demuxer resynchronises on the next sync byte. Matroska, MP4 and AVI carry
// codec setup and seek tables in headers the client already parsed.
func spliceable(container, name string) bool {
	switch strings.ToLower(container) {
	case "mpegts", "ts", "m2ts":
		return true
	case "":
		switch strings.ToLower(path.Ext(name)) {
		case ".ts", ".m2ts", ".mts":
			return true
		}
	}
	return false
}
