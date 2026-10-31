package livetv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var hlsURI = regexp.MustCompile(`URI="([^"]+)"`)

// rewriteHLS resolves every URI against the final response URL after redirects,
// including encryption keys, initialization maps and rendition playlists. Tags
// such as DISCONTINUITY, BYTERANGE and MEDIA-SEQUENCE remain untouched.
func rewriteHLS(body []byte, base *url.URL, rewrite func(string) string) ([]byte, error) {
	resolve := func(raw string) (string, error) {
		u, err := url.Parse(raw)
		if err != nil {
			return "", err
		}
		u = base.ResolveReference(u)
		if !validHTTP(u.String()) {
			return "", fmt.Errorf("unsupported HLS URI")
		}
		return rewrite(u.String()), nil
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if !strings.HasPrefix(trim, "#") {
			u, err := resolve(trim)
			if err != nil {
				return nil, err
			}
			lines[i] = u
			continue
		}
		var rewriteErr error
		lines[i] = hlsURI.ReplaceAllStringFunc(line, func(m string) string {
			parts := hlsURI.FindStringSubmatch(m)
			u, err := resolve(parts[1])
			if err != nil {
				rewriteErr = err
				return m
			}
			return `URI="` + u + `"`
		})
		if rewriteErr != nil {
			return nil, rewriteErr
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}
func (s *Service) upstream(ctx context.Context, raw string, r *http.Request) (*http.Response, error) {
	return s.upstreamWithHeaders(ctx, raw, r, nil)
}

func (s *Service) upstreamWithHeaders(ctx context.Context, raw string, r *http.Request, headers http.Header) (*http.Response, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
			if v := r.Header.Get(name); v != "" {
				req.Header.Set(name, v)
			}
		}
		res, err := s.o.HTTPClient.Do(req)
		if err == nil && res.StatusCode < 500 && res.StatusCode != 429 {
			return res, nil
		}
		if res != nil {
			res.Body.Close()
		}
		last = err
		if last == nil {
			last = fmt.Errorf("upstream unavailable")
		}
		if attempt < 2 {
			t := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
	}
	return nil, last
}
func (s *Service) ServeResource(w http.ResponseWriter, r *http.Request, id, key string, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	raw, ok := ss.resources[key]
	ss.mu.Unlock()
	if !ok {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	var res *http.Response
	var body []byte
	// Buffer one bounded HLS object before publishing headers. A dropped
	// connection can then be retried without corrupting the client response.
	for attempt := 0; attempt < 3; attempt++ {
		res, err = s.upstreamWithHeaders(r.Context(), raw, r, ss.inputHeaders)
		if err != nil {
			s.recordStreamFailure(ss, r.Context(), err)
			writeStreamError(w, err)
			return
		}
		if res.StatusCode != 200 && res.StatusCode != 206 {
			res.Body.Close()
			if res.StatusCode >= 500 || res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 404 || res.StatusCode == 410 || res.StatusCode == 429 {
				s.recordStreamFailure(ss, r.Context(), core.ErrNotAvailable)
			}
			http.Error(w, "upstream stream unavailable", res.StatusCode)
			return
		}
		body, err = io.ReadAll(io.LimitReader(res.Body, (64<<20)+1))
		res.Body.Close()
		if len(body) > 64<<20 {
			http.Error(w, "upstream segment exceeds buffer limit", 502)
			return
		}
		if err == nil {
			break
		}
		if r.Context().Err() != nil {
			writeStreamError(w, r.Context().Err())
			return
		}
	}
	if err != nil {
		s.recordStreamFailure(ss, r.Context(), err)
		writeStreamError(w, err)
		return
	}
	content := res.Header.Get("Content-Type")
	// Some providers label their binary AES keys as application/vnd.apple.mpegurl.
	// The bytes identify a playlist; the MIME type alone cannot do so.
	manifest := bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U"))
	if manifest {
		b := body
		if len(b) > 4<<20 {
			http.Error(w, "upstream playlist too large", 502)
			return
		}
		base := res.Request.URL
		rewritten, err := rewriteHLS(b, base, func(raw string) string {
			resource := stableID(raw)
			ss.mu.Lock()
			if len(ss.resources) > 8192 { // Sessions expire; evict old references rather than grow without bound.
				for k := range ss.resources {
					if k != "index.m3u8" {
						delete(ss.resources, k)
						if len(ss.resources) < 4096 {
							break
						}
					}
				}
			}
			ss.resources[resource] = raw
			ss.mu.Unlock()
			return withAuth("/livetv/sessions/"+id+"/0/resource/"+resource, ss.streamGrant)
		})
		if err != nil {
			http.Error(w, "unsupported upstream playlist", 502)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "HEAD" {
			armWriteDeadline(w)
			w.Write(rewritten)
		}
		return
	}
	if r.Method != http.MethodHead && len(body) > 0 {
		s.touchMedia(ss)
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := res.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	if strings.Contains(strings.ToLower(content), "mpegurl") {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, max-age=10")
	armWriteDeadline(w)
	w.WriteHeader(res.StatusCode)
	if r.Method != "HEAD" {
		_, _ = w.Write(body)
	}
}

// resourceWriteTimeout bounds writing one buffered upstream object (up to
// 64 MiB held in memory) to the client. Servers have no WriteTimeout, so
// without it a client that stops reading would pin the buffer forever.
const resourceWriteTimeout = 5 * time.Minute

func armWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(resourceWriteTimeout))
}
