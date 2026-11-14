package livetv

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The input relay hands ffmpeg and ffprobe a loopback URL for a session's
// selected input instead of the provider URL and headers. Those would
// otherwise sit on the tools' argv, which every local user can read through
// ps, and in their logs.
//
// Each tune attempt gets one grant: an unguessable token bound to that
// stream source. A grant serves the source URL itself and only the
// resources named by the HLS playlists and DASH manifests it served; it is
// not a proxy for anything else. Provider headers go only to the source's
// own origin; explicitly declared channel identity/referrer may follow media.
const (
	relayPathPrefix    = "/livetv/input/"
	relayManifestLimit = 4 << 20
	relayResourceLimit = 8192
	relayGrantLimit    = 1024
	relayConcurrency   = 8
)

// relayIOTimeout matches the -rw_timeout ffmpeg applies to HTTP inputs: the
// longest wait for response headers or for one upstream read.
var relayIOTimeout = 30 * time.Second

var (
	errRelayUnsupported = errors.New("livetv input relay: unsupported manifest")
	relayExt            = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)
)

type inputRelay struct {
	taps   tapRegistry // shared provider connections (upstream_tap.go)
	mu     sync.Mutex
	host   string // 127.0.0.1:port; "" until first use
	grants map[[sha256.Size]byte]*inputGrant
	bySess map[*session]*inputGrant
	// health mirrors bySess for inputHealth, which must never wait on mu:
	// grant changes hold it across a synchronous provider close.
	health sync.Map // *session -> *inputGrant
}

type inputGrant struct {
	log          *slog.Logger // immutable, credential-free session correlation
	token        string
	origin       string      // the only origin that receives authentication
	headers      http.Header // provider headers; immutable
	mediaHeaders http.Header // explicit per-channel playback headers; immutable
	ctx          context.Context
	cancel       context.CancelFunc
	slots        chan struct{}
	source       string // the capped provider account behind this input; "" none
	// reserve is how far behind the live edge this input's reader plays
	// (stabilityReserve for a Steady input, else zero); immutable.
	reserve time.Duration
	lead    time.Duration // extra join depth for a start window (grantSteadyInputLead); immutable
	steady  bool          // feeds a real-time consumer (grantSteadyInput); immutable
	// histChannel/histSrc name the source in its history (the session's
	// original source entry, not the resolved URL); immutable.
	histChannel string
	histSrc     StreamSource
	reported    atomic.Bool // the grant's session has been counted in history
	service     *Service

	mu               sync.Mutex
	tap              *upstreamTap // the shared connection the root input reads through
	hls              *hlsInput    // provider playlist state when the root input is HLS
	watchers         map[*func(UpstreamEvent)]struct{}
	snap             atomic.Pointer[healthSnap] // published health; see inputHealth
	resources        map[string]*relayResource
	byKey            map[string]string
	order            []string
	next             int
	upstreamFailures int
	upstreamPending  int
	upstreamRetryAt  time.Time
}

// relayResource is an exact upstream URL, or a DASH SegmentTemplate whose
// identifiers are filled from the request path.
type relayResource struct {
	key   string // registration key, for eviction
	name  string
	url   string
	vars  []mpdVar
	scope *mpdScope
}

func relayOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

func relayName(stem, p string) string {
	if ext := strings.ToLower(path.Ext(p)); relayExt.MatchString(ext) {
		return stem + ext
	}
	return stem
}

// relaySegmentExt are the media extensions a provider may carry in a query
// value (a redirect target) for an extensionless segment URL.
var relaySegmentExt = map[string]bool{".ts": true, ".m4s": true, ".mp4": true, ".m4a": true, ".m4v": true, ".aac": true, ".ac3": true, ".ec3": true, ".mp3": true, ".vtt": true, ".webvtt": true, ".cmfv": true, ".cmfa": true, ".m3u8": true}

// relayMediaName names a resource a playlist refers to. ffmpeg refuses an HLS
// segment whose URL has no media extension (allowed_segment_extensions), and
// FAST providers (Amagi, ad-insertion beacons) name segments without one, so
// such a resource takes the extension its URL carries elsewhere (a
// redirect_url query value), else ".ts". ffmpeg still probes the bytes.
func relayMediaName(u *url.URL) string {
	if ext := strings.ToLower(path.Ext(u.Path)); relayExt.MatchString(ext) {
		return "media" + ext
	}
	for _, vals := range u.Query() {
		for _, v := range vals {
			if inner, err := url.Parse(v); err == nil && inner.Host != "" {
				if ext := strings.ToLower(path.Ext(inner.Path)); relaySegmentExt[ext] {
					return "media" + ext
				}
			}
		}
	}
	return "media.ts"
}

func tokenKey(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// grantInput replaces the session's grant with one for src. The token is
// the only credential; it reaches ffmpeg as a path component.
func (s *Service) grantInput(ss *session, src StreamSource, headers http.Header) error {
	return s.grantInputFor(ss, src, headers, false, 0)
}

// grantSteadyInput is grantInput for a consumer that reads its input at
// real time: Steady's frame clock, or an owned relay whose timeline paces
// its output. A reader started behind the live edge then stays there, and
// that distance is the channel's stability reserve. A consumer that reads
// as fast as bytes arrive (an unpaced relay) would only turn a reserve into
// a burst of segments; it uses grantInput.
func (s *Service) grantSteadyInput(ss *session, src StreamSource, headers http.Header) error {
	return s.grantInputFor(ss, src, headers, true, 0)
}

// grantSteadyInputLead is grantSteadyInput for a consumer that publishes a
// start window ahead of real time as soon as it starts (an owned relay's
// first segments): its reader joins lead deeper than the reserve, so the
// reserve is still whole once that window is out. No lead is added when the
// channel's preset holds no reserve (Fastest).
func (s *Service) grantSteadyInputLead(ss *session, src StreamSource, headers http.Header, lead time.Duration) error {
	return s.grantInputFor(ss, src, headers, true, max(lead, 0))
}

func (s *Service) grantInputFor(ss *session, src StreamSource, headers http.Header, steady bool, lead time.Duration) error {
	c := ss.info.Channel
	source := src.AuthSourceID
	if source == "" && c.SourceID != "" && (src.ID == stableID(c.ID, "playlist") || len(c.StreamSources) <= 1) {
		source = c.SourceID
	}
	u, err := url.Parse(src.URL)
	if err != nil || !validHTTP(src.URL) {
		return core.ErrNotAvailable
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	var reserve time.Duration
	if steady {
		reserve = s.stabilityReserve(c)
	}
	if reserve == 0 {
		lead = 0
	}
	ctx, cancel := context.WithCancel(s.serviceContext())
	histSrc := src
	if ss.inputSource.ID != "" {
		histSrc = ss.inputSource
	}
	g := &inputGrant{reserve: reserve, lead: lead, steady: steady, service: s, histChannel: c.ID, histSrc: histSrc,
		log:   s.sessionLog(ss).With("input", src.ID),
		token: base64.RawURLEncoding.EncodeToString(b[:]), origin: relayOrigin(u), headers: headers.Clone(), mediaHeaders: redirectMediaHeaders(src.Headers),
		ctx: ctx, cancel: cancel, slots: make(chan struct{}, relayConcurrency), source: source,
		resources: map[string]*relayResource{"0": {name: relayName("stream", u.Path), url: src.URL}},
		byKey:     map[string]string{},
	}
	r := &s.inputRelay
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.bySess[ss]; old != nil {
		r.dropLocked(ss, old)
	}
	if err := s.startRelayLocked(); err != nil {
		cancel()
		return err
	}
	if len(r.grants) >= relayGrantLimit {
		cancel()
		return core.ErrNotAvailable
	}
	r.grants[tokenKey(g.token)] = g
	r.bySess[ss] = g
	r.health.Store(ss, g)
	return nil
}

// transferInput re-keys a grant created for a staged session copy to the
// real session once the staged tune is published. The token (and therefore
// the URL ffmpeg/ffprobe already received) is unchanged.
func (s *Service) transferInput(from, to *session) {
	r := &s.inputRelay
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.bySess[from]
	if g == nil {
		return
	}
	delete(r.bySess, from)
	if old := r.bySess[to]; old != nil && old != g {
		r.dropLocked(to, old)
	}
	r.bySess[to] = g
	r.health.Delete(from)
	r.health.Store(to, g)
}

// revokeInput ends the session's grant and any relay request using it.
func (s *Service) revokeInput(ss *session) {
	r := &s.inputRelay
	r.mu.Lock()
	if g := r.bySess[ss]; g != nil {
		r.dropLocked(ss, g)
	}
	r.mu.Unlock()
}

func (r *inputRelay) dropLocked(ss *session, g *inputGrant) {
	r.health.CompareAndDelete(ss, g)
	if h := g.hlsIfAny(); h != nil {
		h.flushEvidence()
	}
	g.cancel()
	delete(r.grants, tokenKey(g.token))
	delete(r.bySess, ss)
	// Synchronous: a retune that follows finds the old connection closed.
	r.taps.release(g)
}

// relayInput is the URL ffmpeg and ffprobe read the session's input from.
func (s *Service) relayInput(ss *session) (string, error) {
	r := &s.inputRelay
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.bySess[ss]
	if g == nil || g.ctx.Err() != nil {
		return "", core.ErrNotAvailable
	}
	return "http://" + r.host + relayPathPrefix + g.token + "/0/" + g.resources["0"].name, nil
}

func (s *Service) startRelayLocked() error {
	r := &s.inputRelay
	if s.serviceContext().Err() != nil {
		return core.ErrNotAvailable
	}
	if r.host != "" {
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("livetv input relay: %w", err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.serveInputRelay),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// net/http's own messages can quote request paths, which hold tokens.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	r.host = ln.Addr().String()
	r.grants, r.bySess = map[[sha256.Size]byte]*inputGrant{}, map[*session]*inputGrant{}
	go srv.Serve(ln)
	go func() {
		<-s.serviceContext().Done()
		srv.Close()
	}()
	return nil
}

// relayURL registers an exact upstream URL with the grant.
func (g *inputGrant) relayURL(host, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		u = &url.URL{}
	}
	return g.register(host, "e\x00"+raw, &relayResource{name: relayMediaName(u), url: raw})
}

func (g *inputGrant) register(host, key string, res *relayResource) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.byKey[key]
	if !ok {
		g.next++
		id = fmt.Sprintf("%x", g.next)
		g.byKey[key] = id
		g.order = append(g.order, id)
		// Live manifests keep naming new segments; forget the oldest.
		for len(g.order) > relayResourceLimit {
			old := g.order[0]
			g.order = g.order[1:]
			delete(g.byKey, g.resources[old].key)
			delete(g.resources, old)
		}
	}
	res.key = key
	g.resources[id] = res
	tail := res.name
	if len(res.vars) > 0 {
		parts := make([]string, 0, len(res.vars)+1)
		for _, v := range res.vars {
			parts = append(parts, "$"+v.raw+"$")
		}
		tail = strings.Join(append(parts, res.name), "/")
	}
	return "http://" + host + relayPathPrefix + g.token + "/" + id + "/" + tail
}

// lookup maps a request path to its grant and upstream URL. root reports
// the grant's own input (resource 0) rather than a manifest's resource.
func (r *inputRelay) lookup(p string) (g *inputGrant, raw string, root, ok bool) {
	g, raw, ok = r.lookupResource(p)
	if ok {
		parts := strings.SplitN(strings.TrimPrefix(p, relayPathPrefix), "/", 3)
		root = len(parts) == 3 && parts[1] == "0"
	}
	return g, raw, root, ok
}

func (r *inputRelay) lookupResource(p string) (*inputGrant, string, bool) {
	parts := strings.Split(strings.TrimPrefix(p, relayPathPrefix), "/")
	if !strings.HasPrefix(p, relayPathPrefix) || len(parts) < 3 {
		return nil, "", false
	}
	r.mu.Lock()
	g := r.grants[tokenKey(parts[0])]
	r.mu.Unlock()
	if g == nil || g.ctx.Err() != nil {
		return nil, "", false
	}
	g.mu.Lock()
	res := g.resources[parts[1]]
	g.mu.Unlock()
	tail := parts[2:]
	if res == nil || len(tail) != len(res.vars)+1 || tail[len(tail)-1] != res.name {
		return nil, "", false
	}
	if len(res.vars) == 0 {
		return g, res.url, true
	}
	raw, ok := res.fill(tail[:len(tail)-1])
	return g, raw, ok
}

func (s *Service) serveInputRelay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	s.inputRelay.mu.Lock()
	want := s.inputRelay.host
	s.inputRelay.mu.Unlock()
	if ip == nil || !ip.IsLoopback() || r.Host != want {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	g, raw, root, ok := s.inputRelay.lookup(r.URL.EscapedPath())
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// The input itself is read through its shared provider connection.
	if root && s.serveTapped(w, r, g, raw) {
		return
	}
	// A Steady grant's segments may already be in memory.
	if !root && s.servePrefetched(w, r, g, raw) {
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(g.ctx, cancel)()
	segment := false
	if h := g.hlsIfAny(); h != nil {
		segment = h.isSegment(raw)
	}
	res, err := s.relayFetchAs(ctx, cancel, g, raw, r, segment)
	if err == nil && res.StatusCode >= 400 || err != nil && r.Context().Err() == nil {
		if h := g.hlsIfAny(); h != nil {
			h.deliver(h.failedFetch(raw))
			if segment {
				h.segmentFailed()
			}
		}
	}
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			code = http.StatusGatewayTimeout
		}
		http.Error(w, "upstream unavailable", code)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		code := res.StatusCode
		if code < 400 || code > 599 {
			code = http.StatusBadGateway
		}
		http.Error(w, "upstream unavailable", code)
		return
	}
	idle := time.AfterFunc(relayIOTimeout, cancel)
	defer idle.Stop()
	body := bufio.NewReaderSize(&idleReader{r: res.Body, t: idle}, 32<<10)
	// Some providers gzip a playlist without saying so (no Content-Encoding),
	// and Go's transport only decodes declared gzip. Decode it here, or ffmpeg
	// reads compressed bytes as a playlist ("Invalid data").
	if magic, _ := body.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		packed, _ := io.ReadAll(io.LimitReader(body, relayManifestLimit+1))
		if len(packed) <= relayManifestLimit {
			if zr, err := gzip.NewReader(bytes.NewReader(packed)); err == nil {
				plain, err := io.ReadAll(io.LimitReader(zr, relayManifestLimit+1))
				zr.Close()
				if err == nil && len(plain) <= relayManifestLimit {
					if t := bytes.TrimLeft(bytes.TrimPrefix(plain, []byte("\xef\xbb\xbf")), " \t\r\n"); bytes.HasPrefix(t, []byte("#EXTM3U")) {
						s.serveRelayManifest(w, g, raw, res.Request.URL, bytes.NewReader(plain), true)
						return
					}
				}
			}
		}
		// Not a compressed playlist: relay the bytes untouched.
		body = bufio.NewReaderSize(io.MultiReader(bytes.NewReader(packed), body), 32<<10)
	}
	head := sniffHead(body)
	ctype := strings.ToLower(res.Header.Get("Content-Type"))
	hls := bytes.HasPrefix(head, []byte("#EXTM3U"))
	mpd := bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<MPD")) ||
		(bytes.HasPrefix(head, []byte("<")) && strings.Contains(ctype, "xml"))
	if hls || mpd {
		s.serveRelayManifest(w, g, raw, res.Request.URL, body, hls)
		return
	}
	if h := g.hlsIfAny(); h != nil {
		h.servedSegment(raw)
	}
	for _, name := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := res.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	if mt, params, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil {
		w.Header().Set("Content-Type", mime.FormatMediaType(mt, params))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(res.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

func (s *Service) serveRelayManifest(w http.ResponseWriter, g *inputGrant, raw string, base *url.URL, body io.Reader, hls bool) {
	b, err := io.ReadAll(io.LimitReader(body, relayManifestLimit+1))
	if err != nil || len(b) > relayManifestLimit {
		http.Error(w, "upstream manifest unavailable", http.StatusBadGateway)
		return
	}
	if hls {
		// Health, and for Steady the reserve window (reserve_hls.go).
		var events []UpstreamEvent
		h := g.hlsState()
		b, events = h.media(raw, b, base)
		h.deliver(events)
	}
	s.inputRelay.mu.Lock()
	host := s.inputRelay.host
	s.inputRelay.mu.Unlock()
	exact := func(raw string) (string, error) {
		// A playlist fetched over TLS may name plain-HTTP segment hosts (an
		// https origin serving http://<ip>:80 segments). The provider chose
		// them, and every IPTV player fetches them, exactly as with an http
		// redirect hop (scopeInputRedirects). The source's credentials stay
		// on its own origin: relayFetchAs sends them only there.
		if _, err := url.Parse(raw); err != nil || !validHTTP(raw) {
			return "", errRelayUnsupported
		}
		return g.relayURL(host, raw), nil
	}
	var out []byte
	ctype := "application/dash+xml"
	if hls {
		ctype = "application/vnd.apple.mpegurl"
		var rerr error
		out, err = rewriteHLS(b, base, func(raw string) string {
			u, err := exact(raw)
			if err != nil {
				rerr = err
			}
			return u
		})
		if err == nil {
			err = rerr
		}
	} else {
		out, err = rewriteMPD(b, base, exact, func(tmpl string, vars []mpdVar, scope *mpdScope) (string, error) {
			u, err := url.Parse(tmpl)
			if err != nil || !validHTTP(tmpl) {
				return "", errRelayUnsupported
			}
			return g.register(host, "t\x00"+tmpl, &relayResource{name: relayName("media", u.Path), url: tmpl, vars: vars, scope: scope}), nil
		})
	}
	if err != nil {
		http.Error(w, "unsupported upstream manifest", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", fmt.Sprint(len(out)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// scopeInputRedirects confines authentication to the source origin while
// forwarding only explicitly supplied per-channel playback headers.
func scopeInputRedirects(client *http.Client, origin string, headers http.Header, ua string, public ...http.Header) {
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if checkRedirect != nil {
			if err := checkRedirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		// Providers often bounce through a plain-HTTP edge on their CDN
		// (https panel -> http CDN -> https CDN), and every IPTV player
		// follows it. The redirect itself arrived over TLS, so the provider
		// chose the hop. An http URL is never the source's https origin, so
		// authentication and synthesized Referer are removed below. Explicit
		// channel playback headers are then reapplied.
		if relayOrigin(req.URL) != origin {
			// Go synthesizes Referer from the previous URL, which can carry
			// source query credentials even without a configured header.
			req.Header.Del("Referer")
			for name := range headers {
				req.Header.Del(name)
			}
			if ua != "" && req.Header.Get("User-Agent") == "" {
				req.Header.Set("User-Agent", ua)
			}
		}
		if len(public) > 0 {
			applyMediaHeaders(req, public[0])
		}
		return nil
	}
}

// relayFetch fetches one upstream object. Headers wait at most
// relayIOTimeout; the body is then bounded by the caller's idle reader.
// Errors never carry the upstream URL to the caller's response.
func (s *Service) relayFetch(ctx context.Context, cancel context.CancelFunc, g *inputGrant, raw string, in *http.Request) (*http.Response, error) {
	return s.relayFetchAs(ctx, cancel, g, raw, in, false)
}

// relayFetchAs fetches one upstream object. segment marks a media segment
// named by a live HLS playlist: it is a short request that holds no provider
// connection, so it is outside the grant's refusal accounting (whose 1 s
// spacing exists for refused stream opens). One quick retry, then the error
// goes straight back to ffmpeg, whose HLS demuxer skips the segment while
// the reserve plays on; a slow answer would stall the decoder instead.
func (s *Service) relayFetchAs(ctx context.Context, cancel context.CancelFunc, g *inputGrant, raw string, in *http.Request, segment bool) (*http.Response, error) {
	client := *s.o.HTTPClient
	client.Timeout = 0 // a live input is endless; idle time is bounded instead
	ua := defaultUserAgent()
	scopeInputRedirects(&client, g.origin, g.headers, ua, g.mediaHeaders)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errRelayUnsupported
	}
	// A provider that sells connections counts every attempt; it gets one,
	// not a burst of retries.
	attempts := 3
	if segment {
		attempts = 2
	} else if g.source != "" && s.sourceConnectionLimit(g.source) > 0 {
		attempts = 1
	}
	// Headers wait at most relayIOTimeout, and never past the opener's
	// startup deadline.
	budget := func() time.Duration {
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < relayIOTimeout {
			return max(time.Until(dl), 0)
		}
		return relayIOTimeout
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, errRelayUnsupported
		}
		if relayOrigin(u) == g.origin {
			req.Header = g.headers.Clone()
		}
		applyMediaHeaders(req, g.mediaHeaders)
		if req.Header.Get("User-Agent") == "" && ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		for _, name := range []string{"Range", "If-Range"} {
			if v := in.Header.Get(name); v != "" {
				req.Header.Set(name, v)
			}
		}
		if !segment && !g.reserveUpstreamAttempt(ctx) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("live provider retry limit reached")
		}
		// Backoff consumes the caller's startup deadline, not the per-I/O
		// response-header timeout for the request that follows it.
		var timedOut atomic.Bool
		t := time.AfterFunc(budget(), func() { timedOut.Store(true); cancel() })
		res, err := client.Do(req)
		if !segment {
			g.finishUpstreamAttempt(err != nil || res.StatusCode >= 400)
		}
		t.Stop()
		if timedOut.Load() || errors.Is(err, context.DeadlineExceeded) {
			if res != nil {
				res.Body.Close()
			}
			return nil, context.DeadlineExceeded
		}
		if err == nil && res.StatusCode < 500 && res.StatusCode != 429 {
			return res, nil
		}
		if res != nil {
			res.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, Diagnose("upstream_headers", ctx.Err())
		}
		if err != nil {
			last = Diagnose("upstream_headers", err)
		} else {
			last = providerResponseError(res.StatusCode)
		}
		if attempt < attempts-1 {
			select {
			case <-ctx.Done():
				return nil, core.ErrNotAvailable
			case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			}
		}
	}
	return nil, last
}

// sniffHead returns the start of a body after any BOM and whitespace, reading
// only until it can tell a manifest from media: a slow live stream must not
// wait for a fixed-size prefix.
func sniffHead(br *bufio.Reader) []byte {
	bom := []byte("\xef\xbb\xbf")
	for n := 1; ; n++ {
		b, err := br.Peek(n)
		if err != nil || n >= 512 {
			return bytes.TrimLeft(bytes.TrimPrefix(b, bom), " \t\r\n")
		}
		if bytes.HasPrefix(bom, b) {
			continue
		}
		t := bytes.TrimLeft(bytes.TrimPrefix(b, bom), " \t\r\n")
		if len(t) >= 7 || len(t) > 0 && t[0] != '#' && t[0] != '<' {
			return t
		}
	}
}

// idleReader cancels the upstream request when one read waits longer than
// relayIOTimeout. Time spent writing to a slow reader does not count.
type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	r.t.Reset(relayIOTimeout)
	n, err := r.r.Read(p)
	r.t.Stop()
	return n, err
}

// Bound a refusal episode across relay and media-tool retries. Successful
// responses replenish it; concurrent requests reserve before touching the panel.
func (g *inputGrant) reserveUpstreamAttempt(ctx context.Context) bool {
	g.mu.Lock()
	if g.upstreamFailures+g.upstreamPending >= 28 {
		g.mu.Unlock()
		return false
	}
	g.upstreamPending++
	at := g.upstreamRetryAt
	if g.upstreamFailures > 0 {
		if at.Before(time.Now()) {
			at = time.Now()
		}
		g.upstreamRetryAt = at.Add(time.Second)
	}
	g.mu.Unlock()
	if wait := time.Until(at); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			g.mu.Lock()
			g.upstreamPending--
			g.mu.Unlock()
			return false
		}
	}
	return true
}
func (g *inputGrant) finishUpstreamAttempt(failed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.upstreamPending--
	if failed {
		g.upstreamFailures++
		if at := time.Now().Add(time.Second); g.upstreamRetryAt.Before(at) {
			g.upstreamRetryAt = at
		}
	} else {
		g.upstreamFailures = 0
		g.upstreamRetryAt = time.Time{}
	}
}

// joinDepth is how far behind the live edge a new reader of this grant
// starts: the reserve plus any start window.
func (g *inputGrant) joinDepth() time.Duration { return g.reserve + g.lead }

// noteEvidence adds evidence to the history of the grant's source, counting
// the grant's session once it has received media.
func (g *inputGrant) noteEvidence(ev SourceEvidence) {
	if g.service == nil || g.histSrc.ID == "" || ev.empty() {
		return
	}
	if ev.Media > 0 && g.reported.CompareAndSwap(false, true) {
		ev.newSession = true
	}
	g.service.noteSourceEvidence(g.histChannel, g.histSrc, ev)
}
