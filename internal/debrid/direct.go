package debrid

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// Direct HTTP streams (core.ProtocolHTTP): a link a Stremio addon resolved
// itself, often through the addon owner's own debrid account. Ether needs
// no debrid account for them: the engine reads the link with the same
// block cache, read-ahead and stall handling as a debrid file.
//
// The link is untrusted remote data. Every request goes through a
// safefetch guard: only the owner-configured addon origin (Payload.Origin)
// may be a private address, every other destination and redirect hop must
// be public. The addon's request headers are sent only to its own link's
// origin and dropped on any redirect away from it. The link, its headers
// and the redirect target can carry credentials: they stay in memory and
// in the server's own payload rows, never in logs or errors.

// KindHTTP is the Payload.Kind of a direct HTTP stream.
const KindHTTP = "http"

const (
	// minDirectBytes: an addon answering an uncached torrent with a short
	// "downloading" clip, or an error page, is not the release.
	minDirectBytes = 32 << 20
	probeTimeout   = 20 * time.Second
)

// IsDirect reports a direct HTTP stream payload.
func (p Payload) IsDirect() bool { return p.Kind == KindHTTP }

// directClients are the guarded clients for direct streams: a primary
// and a hedge pool, like the engine's debrid clients.
type directClients struct {
	once    sync.Once
	guard   *safefetch.Guard
	primary *http.Client
	hedge   *http.Client
}

func (d *directClients) init() {
	d.once.Do(func() {
		g := d.guard
		if g == nil {
			g = &safefetch.Guard{}
		}
		mk := func() *http.Client {
			return g.Client(&http.Client{Transport: newStreamingTransport(nil), CheckRedirect: dropHeadersAcrossOrigins})
		}
		d.primary, d.hedge = mk(), mk()
	})
}

// dropHeadersAcrossOrigins keeps the addon's request headers on redirects
// within its link's origin only; elsewhere only Range and User-Agent go.
func dropHeadersAcrossOrigins(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && !safefetch.SameOrigin(via[0].URL.String(), req.URL.String()) {
		for k := range req.Header {
			if k != "Range" && k != "User-Agent" {
				req.Header.Del(k)
			}
		}
	}
	return nil
}

type directKey struct{}

// withDirect marks a media request as a direct stream request for p.
func withDirect(ctx context.Context, p Payload) context.Context {
	return context.WithValue(ctx, directKey{}, p)
}

// prepareDirect applies a direct payload's policy to a media request:
// the trusted origin and, on the link's own origin, the addon's headers.
func prepareDirect(req *http.Request, p Payload) *http.Request {
	req = req.WithContext(safefetch.WithTrustedOriginFor(req.Context(), p.Origin, req.URL.String()))
	if safefetch.SameOrigin(p.Link, req.URL.String()) {
		for k, v := range p.Headers {
			if k != "Range" && k != "Host" {
				req.Header.Set(k, v)
			}
		}
	}
	return req
}

// DirectPayload probes an HTTP stream release and returns its payload:
// one ranged request learns the file's exact size and final location and
// rejects what is not the release (an error page, a "downloading" clip, a
// server without range requests or a size). Every answer the addon gives
// is transient (core.ErrNotCached or core.ErrSourceUnavailable): an addon
// that is still fetching a file, or behind a misbehaving proxy, may serve
// it properly later, so none marks the release bad. Only a release whose
// own link is malformed is rejected (core.ErrReleaseRejected).
func (s *Service) DirectPayload(ctx context.Context, rel core.Release) ([]byte, error) {
	link, err := url.Parse(rel.StreamURL)
	if err != nil || (link.Scheme != "http" && link.Scheme != "https") || link.Host == "" {
		return nil, fmt.Errorf("debrid: %q has no HTTP stream link: %w", rel.Name, core.ErrReleaseRejected)
	}
	base := Payload{Key: payloadKey(rel), ReleaseID: rel.ID, Kind: KindHTTP, Link: rel.StreamURL, URL: rel.StreamURL,
		Headers: rel.StreamHeaders, Origin: rel.StreamOrigin, Release: rel.Name}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, rel.StreamURL, nil)
	if err != nil {
		return nil, fmt.Errorf("debrid: %q: invalid stream link: %w", rel.Name, core.ErrReleaseRejected)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", "Ether")
	s.direct.init()
	resp, err := s.direct.primary.Do(prepareDirect(req, base))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("debrid: stream from %s: %w: %v", link.Host, core.ErrSourceUnavailable, sanitize(err))
	}
	defer drain(resp)
	host := link.Host
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("debrid: stream from %s: HTTP %d: %w", host, resp.StatusCode, core.ErrSourceUnavailable)
	}
	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "json") || strings.Contains(ctype, "xml") {
		return nil, fmt.Errorf("debrid: stream from %s answered %s, not media: %w", host, ctype, core.ErrNotCached)
	}
	if resp.StatusCode == http.StatusOK {
		return nil, fmt.Errorf("debrid: stream from %s answered without range support: %w", host, core.ErrSourceUnavailable)
	}
	size := contentRangeTotal(resp.Header.Get("Content-Range"))
	if size <= 0 {
		return nil, fmt.Errorf("debrid: stream from %s gave no file size: %w", host, core.ErrSourceUnavailable)
	}
	if size < minDirectBytes || (rel.Size > 0 && size < rel.Size/2) {
		return nil, fmt.Errorf("debrid: stream from %s is %d bytes, not the %d byte release (still downloading?): %w", host, size, rel.Size, core.ErrNotCached)
	}
	base.Size = size
	base.Path = directFileName(rel.Name, resp, ctype)
	if final := resp.Request.URL.String(); final != "" {
		base.URL = final // the addon's redirect target; the link stays in Link
	}
	return base.Encode(), nil
}

// contentRangeTotal is the complete length in "bytes 0-0/12345", or 0.
func contentRangeTotal(v string) int64 {
	_, total, ok := strings.Cut(v, "/")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var videoTypes = map[string]string{
	"video/x-matroska": ".mkv", "video/matroska": ".mkv", "video/mp4": ".mp4", "video/webm": ".webm",
	"video/mp2t": ".ts", "video/x-msvideo": ".avi", "video/quicktime": ".mov",
}

// directFileName names the file so its container can be recognised: the
// served file name (Content-Disposition, then the final URL's base name)
// when it is a video file, else the release name with the extension its
// content type implies.
func directFileName(name string, resp *http.Response, ctype string) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if fn := path.Base(strings.ReplaceAll(params["filename"], "\\", "/")); videoFile(fn) {
			return fn
		}
	}
	if fn, err := url.PathUnescape(path.Base(resp.Request.URL.Path)); err == nil && videoFile(fn) {
		return fn
	}
	return name + videoTypes[ctype]
}

func videoFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv", ".mp4", ".m4v", ".webm", ".ts", ".m2ts", ".avi", ".mov":
		return true
	}
	return false
}

// directMediaError keeps a direct stream's failures from marking the
// release bad: an addon link that changes size or keeps failing may be a
// placeholder for a file the addon is still fetching, or an addon outage.
func directMediaError(p Payload, err error) error {
	if p.IsDirect() && (errors.Is(err, core.ErrArticleCorrupt) || errors.Is(err, core.ErrArticleMissing)) {
		return fmt.Errorf("debrid: addon stream %s is not serving the file now: %w", p.Name(), core.ErrSourceUnavailable)
	}
	return err
}
