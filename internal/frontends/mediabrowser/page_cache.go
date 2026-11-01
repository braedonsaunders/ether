package mediabrowser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const browseCacheLimit = 128
const browseCacheMaxBody = 1 << 20

type cachedBody struct {
	code  int
	ctype string
	etag  string
	body  []byte
	at    time.Time
}

type browseCache struct {
	mu sync.Mutex
	m  map[string]cachedBody
}

func newBrowseCache() *browseCache {
	return &browseCache{m: map[string]cachedBody{}}
}

func (c *browseCache) wrap(next http.Handler, fresh func() frontend.PageFreshness) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cacheableBrowse(r) || fresh == nil {
			next.ServeHTTP(w, r)
			return
		}
		key := browseCacheKey(fresh(), r)
		sum := sha256.Sum256([]byte(key))
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		if body, ok := c.get(key); ok {
			h := w.Header()
			h.Set("Content-Type", body.ctype)
			h.Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.WriteHeader(body.code)
			_, _ = w.Write(body.body)
			return
		}
		w.Header().Set("ETag", etag)
		cap := &bodyCapture{ResponseWriter: w}
		// A stored hash is joined inside the handler. Hashing that has not
		// finished stays on the background worker; the cache must not wait for it.
		next.ServeHTTP(cap, r.WithContext(frontend.WithoutSynchronousArtworkMetadata(r.Context())))
		if cap.code == http.StatusOK && cap.buf.Len() > 0 && cap.buf.Len() <= browseCacheMaxBody {
			c.put(key, cachedBody{code: cap.code, ctype: cap.Header().Get("Content-Type"), etag: etag, body: append([]byte(nil), cap.buf.Bytes()...), at: time.Now()})
		}
	})
}

func (c *browseCache) get(key string) (cachedBody, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, ok := c.m[key]
	return body, ok
}

func (c *browseCache) put(key string, body cachedBody) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= browseCacheLimit {
		var oldest string
		var at time.Time
		for k, v := range c.m {
			if oldest == "" || v.at.Before(at) {
				oldest, at = k, v.at
			}
		}
		delete(c.m, oldest)
	}
	c.m[key] = body
}

func cacheableBrowse(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	if strings.Contains(p, "/Images/") || strings.Contains(p, "/Playback") || strings.Contains(p, "/Download") || strings.Contains(p, "/Subtitles/") || strings.Contains(p, "/Videos/") {
		return false
	}
	// Library lists fill personalized shelves after the first response. Caching
	// that first body would keep serving Movies and TV Shows after the warm
	// finished, because personal identity is not part of page freshness.
	if strings.HasSuffix(p, "/Views") || strings.HasSuffix(p, "/UserViews") {
		return false
	}
	return strings.Contains(p, "/Items") || strings.HasSuffix(p, "/NextUp") || strings.HasSuffix(p, "/Search/Hints")
}

func browseCacheKey(f frontend.PageFreshness, r *http.Request) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		r.Header.Get("Authorization"),
		r.Header.Get("X-Emby-Token"),
		r.Header.Get("X-MediaBrowser-Token"),
		r.Header.Get("X-Emby-Device-Id"),
		r.Header.Get("X-Emby-Authorization"),
	}, "\n")))
	return fmt.Sprintf("%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%s|%s|%s", f.Catalog, f.Config, f.Watch, f.Favorite, f.Hidden, f.Listed, f.Images, f.Grants, f.Accounts, f.Safety, f.ArtBucket, r.URL.RequestURI(), hex.EncodeToString(sum[:8]), r.Header.Get("Accept-Language"))
}

type bodyCapture struct {
	http.ResponseWriter
	code  int
	buf   bytes.Buffer
	wrote bool
}

func (c *bodyCapture) WriteHeader(code int) {
	if c.wrote {
		return
	}
	c.code = code
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *bodyCapture) Write(p []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	c.buf.Write(p)
	return c.ResponseWriter.Write(p)
}
