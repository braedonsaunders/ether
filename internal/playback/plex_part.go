package playback

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
)

type plexPartKey struct{}

// PlexPartRequest selects the PMS byte-serving contract without changing the
// representation semantics of other frontends or the encoder loopback.
func PlexPartRequest(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), plexPartKey{}, true))
}

func plexPartMIME(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".ts":
		return "video/MP2T"
	case ".mpg", ".mpeg":
		return "video/mpeg"
	case ".flv":
		return "video/x-flv"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".asf":
		return "video/x-ms-asf"
	case ".avi", ".divx":
		return "video/x-msvideo"
	default:
		return "application/octet-stream"
	}
}

// PMS accepts one range window, ignoring the unit. Failed numeric conversions
// and list syntax have measured fallbacks rather than RFC multipart semantics.
func servePlexPart(w http.ResponseWriter, r *http.Request, rep representation) error {
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", rep.contentType)
	if rep.etag != "" {
		h.Set("ETag", rep.etag)
	}
	if !rep.modTime.IsZero() {
		h.Set("Last-Modified", rep.modTime.UTC().Format(http.TimeFormat))
	}
	status, start, length := http.StatusOK, int64(0), rep.size
	end := rep.size - 1
	_, spec, hasEqual := strings.Cut(r.Header.Get("Range"), "=")
	if hasEqual && spec != "-" {
		status = http.StatusPartialContent
		first, last, dash := strings.Cut(spec, "-")
		parse := func(s string) int64 {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
		if dash && !strings.ContainsAny(last, "-,") && !strings.Contains(first, ",") {
			if first == "" {
				n := parse(last)
				if n > 0 {
					if n < rep.size {
						start = rep.size - n
					}
					length = rep.size - start
				} else {
					end = 0
				}
			} else if n, err := strconv.ParseInt(first, 10, 64); err == nil {
				start = n
				if last != "" {
					end = min(parse(last), end)
				}
				length = end - start + 1
			}
		}
		if start < 0 || start >= rep.size || length <= 0 {
			h.Del("Accept-Ranges")
			h.Del("Content-Range")
			h.Del("Content-Length")
			h.Del("X-Plex-Local-Asset")
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			if r.Method != http.MethodHead {
				_, _ = fmt.Fprint(w, "<html><head><title>416 Requested Range Not Satisfiable</title></head><body><h1>416 Requested Range Not Satisfiable</h1></body></html>")
			}
			return nil
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, rep.size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	return serveSingleRangeBody(w, r, status, start, length, rep.deferStatusOnZeroRead, rep.body)
}
