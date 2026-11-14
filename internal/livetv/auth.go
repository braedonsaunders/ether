package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// redactedSecret is what the owner API shows in place of a stored secret.
// Sending it back unchanged keeps the stored value (same convention as the
// admin config API).
const redactedSecret = "••••"

// SourceAuth is how Ether authenticates to an IPTV or guide provider. It is
// used for the source's playlist and guide fetches and, through
// upstreamHeaders, for the streams of channel inputs that belong to it.
type SourceAuth struct {
	// Type: "" (none), "basic", "bearer", "headers" or "xtream".
	Type     string `json:"type,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // secret
	Token    string `json:"token,omitempty"`    // secret (bearer)
	// UserAgent and Referer apply to every type; many providers reject
	// unknown clients.
	UserAgent string `json:"userAgent,omitempty"`
	Referer   string `json:"referer,omitempty"`
	// Headers are extra request headers (cookies, API keys); values are secret.
	Headers map[string]string `json:"headers,omitempty"`
	// Xtream Codes: the panel URL. Playlist and guide URLs are derived from
	// it and the credentials, and never returned by the API.
	Server string `json:"server,omitempty"`
	// Output selects the Xtream stream format: "ts" (default) or "m3u8".
	Output string `json:"output,omitempty"`
}

var authTypes = map[string]bool{"": true, "basic": true, "bearer": true, "headers": true, "xtream": true}

// forbiddenHeaders are managed by the HTTP stack or by the proxy itself.
var forbiddenHeaders = map[string]bool{
	"Host": true, "Content-Length": true, "Transfer-Encoding": true, "Connection": true,
	"Range": true, "If-Range": true, "Te": true, "Upgrade": true, "X-Plex-Token": true,
}

func (a *SourceAuth) empty() bool {
	return a == nil || (a.Type == "" && a.UserAgent == "" && a.Referer == "" && len(a.Headers) == 0)
}

func (a *SourceAuth) validate() error {
	if a == nil {
		return nil
	}
	if !authTypes[a.Type] {
		return fmt.Errorf("unknown source authentication %q", a.Type)
	}
	switch a.Type {
	case "basic":
		if a.Username == "" {
			return fmt.Errorf("basic authentication needs a username")
		}
	case "bearer":
		if a.Token == "" {
			return fmt.Errorf("bearer authentication needs a token")
		}
	case "headers":
		if len(a.Headers) == 0 {
			return fmt.Errorf("add at least one header")
		}
	case "xtream":
		if !validHTTP(a.Server) {
			return fmt.Errorf("the Xtream server must be an HTTP(S) URL")
		}
		if a.Username == "" || a.Password == "" {
			return fmt.Errorf("Xtream needs a username and password")
		}
		if a.Output != "" && a.Output != "ts" && a.Output != "m3u8" {
			return fmt.Errorf("Xtream output must be ts or m3u8")
		}
	}
	if len(a.Headers) > 32 {
		return fmt.Errorf("at most 32 custom headers")
	}
	for k, v := range a.Headers {
		name := http.CanonicalHeaderKey(strings.TrimSpace(k))
		if name == "" || strings.ContainsAny(name, " :\r\n") || forbiddenHeaders[name] {
			return fmt.Errorf("header %q can't be set on a source", k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("header %q has an invalid value", k)
		}
	}
	if strings.ContainsAny(a.UserAgent+a.Referer, "\r\n") {
		return fmt.Errorf("invalid user agent or referer")
	}
	return nil
}

// header renders the request headers for this provider.
func (a *SourceAuth) header() http.Header {
	h := http.Header{}
	if a == nil {
		return h
	}
	if a.UserAgent != "" {
		h.Set("User-Agent", a.UserAgent)
	} else if a.Type == "xtream" {
		// One identity for the panel's API, guide and streams: panels that
		// bind a login to a device compare them.
		h.Set("User-Agent", defaultUserAgent())
	}
	if a.Referer != "" {
		h.Set("Referer", a.Referer)
	}
	for k, v := range a.Headers {
		h.Set(strings.TrimSpace(k), v)
	}
	switch a.Type {
	case "basic":
		r := &http.Request{Header: http.Header{}}
		r.SetBasicAuth(a.Username, a.Password)
		h.Set("Authorization", r.Header.Get("Authorization"))
	case "bearer":
		h.Set("Authorization", "Bearer "+a.Token)
	}
	return h
}

// xtreamURLs derives an Xtream Codes panel's lineup (its JSON API, see
// xtream.go) and XMLTV URLs.
func (a *SourceAuth) xtreamURLs() (lineup, guide string) {
	base := strings.TrimRight(strings.TrimSpace(a.Server), "/")
	q := url.Values{"username": {a.Username}, "password": {a.Password}}
	return base + "/player_api.php?" + q.Encode(), base + "/xmltv.php?" + q.Encode()
}

// defaultUserAgent names Ether to providers when the owner sets no user
// agent. Panels commonly refuse Go's generic client name outright.
func defaultUserAgent() string { return sourceUserAgent }

// redacted returns a copy safe for the owner API.
func (a *SourceAuth) redacted() *SourceAuth {
	if a == nil {
		return nil
	}
	c := *a
	if c.Password != "" {
		c.Password = redactedSecret
	}
	if c.Token != "" {
		c.Token = redactedSecret
	}
	if len(a.Headers) > 0 {
		c.Headers = make(map[string]string, len(a.Headers))
		for k, v := range a.Headers {
			if v != "" {
				v = redactedSecret
			}
			c.Headers[k] = v
		}
	}
	return &c
}

// keepSecrets replaces redacted placeholders with the stored secrets.
func (a *SourceAuth) keepSecrets(old *SourceAuth) {
	if a == nil {
		return
	}
	if old == nil {
		old = &SourceAuth{}
	}
	if a.Password == redactedSecret {
		a.Password = old.Password
	}
	if a.Token == redactedSecret {
		a.Token = old.Token
	}
	for k, v := range a.Headers {
		if v == redactedSecret {
			a.Headers[k] = old.Headers[k]
		}
	}
}

// hasRedacted reports whether a carries a redacted placeholder to restore.
func (a *SourceAuth) hasRedacted() bool {
	if a.Password == redactedSecret || a.Token == redactedSecret {
		return true
	}
	for _, v := range a.Headers {
		if v == redactedSecret {
			return true
		}
	}
	return false
}

// prepareSourceAuth runs before an import: it restores secrets the owner
// API sent back redacted, validates the settings and derives Xtream URLs.
func (s *Service) prepareSourceAuth(ctx context.Context, src *Source) error {
	if src.Auth.empty() {
		src.Auth = nil
		return nil
	}
	old, err := s.storedSourceByID(ctx, src.ID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	if src.Auth.hasRedacted() && old.Auth != nil && sourceMoved(src, old) {
		return errSourceSecretMoved
	}
	src.Auth.keepSecrets(old.Auth)
	if err := src.Auth.validate(); err != nil {
		return err
	}
	if src.Auth.Type == "xtream" {
		playlist, guide := src.Auth.xtreamURLs()
		src.PlaylistURL, src.Playlist = playlist, ""
		// An owner-chosen guide URL wins over the panel's XMLTV export.
		if src.GuideURL == "" || isXtreamURL(src.GuideURL, src.Auth.Server) {
			src.GuideURL, src.Guide = guide, ""
		}
	}
	return nil
}

func isXtreamURL(raw, server string) bool {
	return strings.HasPrefix(raw, strings.TrimRight(server, "/")+"/xmltv.php")
}

// redactSourceAuth hides secrets (and Xtream URLs, which embed them) from a
// source about to be returned by the owner API.
func redactSourceAuth(src *Source) {
	if src.Kind == legacyStremioKind && src.PlaylistURL != "" {
		src.PlaylistURL = redactedSecret
	}
	if src.Auth == nil {
		return
	}
	if src.Auth.Type == "xtream" {
		src.PlaylistURL = ""
		if isXtreamURL(src.GuideURL, src.Auth.Server) {
			src.GuideURL = ""
		}
	}
	src.Auth = src.Auth.redacted()
}

// readURLWith fetches a source document with the provider's headers.
// kind names the document in limit and timeout errors ("playlist" or "guide").
func (s *Service) readURLWith(ctx context.Context, raw string, max int64, h http.Header, kind string) (string, error) {
	rc, err := s.openURL(ctx, raw, h)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := readLimited(rc, max, kind)
	if err != nil {
		return "", sourceReadFailure(err, kind)
	}
	return string(b), nil
}

// sourceDocumentTimeout bounds the wait for a source to start answering. An
// Xtream panel composes a playlist or guide on demand, so the wait can be
// minutes; once bytes flow, sourceIdleTimeout bounds a silent body and the
// document may take as long as it needs.
const sourceDocumentTimeout = refreshTimeout

// sourceIdleTimeout is how long a document may go silent mid-transfer before
// the import fails.
const sourceIdleTimeout = 2 * time.Minute

// errSourceStalled marks a document whose body stopped flowing.
var errSourceStalled = errors.New("source stopped sending data")

// documentClient returns the client for a source document. It keeps the
// service client's transport (and so its live egress routing) but has no total
// timeout: openURL bounds the header wait and a silent body itself.
func (s *Service) documentClient() *http.Client {
	c := *s.o.HTTPClient
	c.Timeout = 0
	return &c
}

// sourceBody counts a document's bytes for the progress endpoint and cancels
// a body that stops flowing.
type sourceBody struct {
	io.ReadCloser
	timer    *time.Timer
	cancel   context.CancelFunc
	stalled  *atomic.Bool
	progress *progressTracker
}

func (b *sourceBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(sourceIdleTimeout)
		if b.progress != nil {
			b.progress.addRead(int64(n))
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			b.timer.Stop()
			return n, err
		}
		if b.stalled.Load() {
			return n, errSourceStalled
		}
		b.timer.Stop()
	}
	return n, err
}

func (b *sourceBody) Close() error {
	b.timer.Stop()
	b.cancel()
	return b.ReadCloser.Close()
}

// sourceFetchFailure explains a transport failure without echoing the request
// URL: source URLs, Xtream's derived ones in particular, carry the provider's
// credentials, and callers store and surface these errors. The cause itself
// (DNS, refused connection, TLS, timeout) is what tells an owner how to fix
// the source.
func sourceFetchFailure(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("source fetch failed: the source did not answer within %s", sourceDocumentTimeout)
	}
	return fmt.Errorf("source fetch failed: %w", err)
}

// sourceReadFailure wraps a body that stalled or timed out in the same
// sentence as a failed fetch. Any other error (a size limit, malformed XML)
// already says what went wrong.
func sourceReadFailure(err error, kind string) error {
	if errors.Is(err, errSourceStalled) {
		return fmt.Errorf("source fetch failed: the source stopped sending the %s for %s", kind, sourceIdleTimeout)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("source fetch failed: the source did not finish sending the %s within %s", kind, sourceDocumentTimeout)
	}
	return err
}

// openURL fetches a source document and leaves the body to the caller. The
// header wait is sourceDocumentTimeout and a silent body is sourceIdleTimeout;
// a document that keeps sending data may take as long as it needs.
func (s *Service) openURL(ctx context.Context, raw string, h http.Header) (io.ReadCloser, error) {
	if !validHTTP(raw) {
		return nil, fmt.Errorf("source must use HTTP(S)")
	}
	ctx, cancel := context.WithCancel(ctx)
	var stalled atomic.Bool
	timer := time.AfterFunc(sourceDocumentTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent())
	}
	res, err := s.documentClient().Do(req)
	if err != nil {
		timer.Stop()
		cancel()
		if stalled.Load() {
			return nil, fmt.Errorf("source fetch failed: the source did not answer within %s", sourceDocumentTimeout)
		}
		return nil, sourceFetchFailure(err)
	}
	closeBody := func() {
		timer.Stop()
		cancel()
		res.Body.Close()
	}
	switch {
	case res.StatusCode == 401 || res.StatusCode == 403:
		closeBody()
		return nil, fmt.Errorf("source refused the credentials (HTTP %d)", res.StatusCode)
	case res.StatusCode != 200:
		closeBody()
		return nil, fmt.Errorf("source returned HTTP %d", res.StatusCode)
	}
	if stalled.Load() {
		closeBody()
		return nil, fmt.Errorf("source fetch failed: the source did not answer within %s", sourceDocumentTimeout)
	}
	timer.Reset(sourceIdleTimeout)
	body := &sourceBody{ReadCloser: res.Body, timer: timer, cancel: cancel, stalled: &stalled}
	if p := importProgress(ctx); p != nil {
		p.setTotal(res.ContentLength)
		body.progress = p
	}
	return body, nil
}

// readGuide streams a guide URL, gunzipping when the body is gzip, and keeps
// only the filtered channel and programme entries.
func (s *Service) readGuide(ctx context.Context, raw string, h http.Header, f guideFilter) (XMLGuide, error) {
	rc, err := s.openURL(ctx, raw, h)
	if err != nil {
		return XMLGuide{}, err
	}
	defer rc.Close()
	rd, err := maybeGunzip(rc, maxGuideBytes, "guide")
	if err != nil {
		return XMLGuide{}, sourceReadFailure(err, "guide")
	}
	defer closeReader(rd)
	g, _, err := parseXMLTV(rd, f)
	if err != nil {
		err = sourceReadFailure(err, "guide")
	}
	return g, err
}

// authCache keeps provider settings for the stream path, which asks for them
// on every segment. Imports and deletes drop the entry and advance gen so an
// in-flight database read from before the mutation never repopulates stale data.
type authCache struct {
	mu  sync.Mutex
	gen uint64
	m   map[string]authEntry
}

type authEntry struct {
	auth    *SourceAuth
	limit   int           // connection limit; 0 is none
	gap     time.Duration // least time between new provider streams
	perHour int           // new provider streams per hour; 0 is no cap
	at      time.Time
	// disabled caches a paused source, so channel listings and policy checks
	// need no database read per channel. forgetSourceAuth clears it.
	disabled bool
}

const authCacheTTL = 30 * time.Second

func (s *Service) forgetSourceAuth(id string) {
	c := &s.authCache
	c.mu.Lock()
	c.gen++
	delete(c.m, id)
	c.mu.Unlock()
}

func (s *Service) setSourceEntry(id string, e authEntry) {
	c := &s.authCache
	c.mu.Lock()
	c.gen++
	if c.m == nil {
		c.m = map[string]authEntry{}
	}
	c.m[id] = e
	c.mu.Unlock()
}

func (s *Service) sourceAuth(ctx context.Context, id string) (*SourceAuth, error) {
	e, err := s.sourceEntry(ctx, id)
	return e.auth, err
}

// sourceConnectionLimit is how many connections the source's provider
// allows at once (an Xtream panel's max_connections); 0 means no limit.
func (s *Service) sourceConnectionLimit(id string) int {
	e, err := s.sourceEntry(s.ctx, id)
	if err != nil {
		return 0
	}
	return e.limit
}

// sourcePaused reports whether the owner disabled the source that owns c.
// A paused source keeps its settings and channels, but its channels are not
// listed, guided or tuned until it is enabled again.
func (s *Service) sourcePaused(ctx context.Context, c Channel) bool {
	if c.SourceID == "" {
		return false
	}
	_, err := s.sourceEntry(ctx, c.SourceID)
	return errors.Is(err, core.ErrNotAvailable)
}

// defaultTuneGap spaces new streams on a connection-limited account: a
// viewer flicking through channels must not look like a scan.
const defaultTuneGap = 2 * time.Second

func (s *Service) sourceEntry(ctx context.Context, id string) (authEntry, error) {
	if id == "" {
		return authEntry{}, nil
	}
	c := &s.authCache
	c.mu.Lock()
	e, ok := c.m[id]
	gen := c.gen
	c.mu.Unlock()
	if ok && s.o.Now().Sub(e.at) < authCacheTTL {
		if e.disabled {
			return authEntry{}, core.ErrNotAvailable
		}
		return e, nil
	}
	src, err := s.storedSourceByID(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		src, err = Source{}, nil
	}
	if err != nil {
		return authEntry{}, err
	}
	if src.Disabled {
		c.mu.Lock()
		if c.gen == gen {
			if c.m == nil {
				c.m = map[string]authEntry{}
			}
			c.m[id] = authEntry{at: s.o.Now(), disabled: true}
		}
		c.mu.Unlock()
		return authEntry{}, core.ErrNotAvailable
	}
	e = authEntry{auth: src.Auth, at: s.o.Now(), perHour: max(src.TunesPerHour, 0)}
	if src.Account != nil {
		e.limit = src.Account.MaxConnections
	}
	if src.MaxConnections > 0 && (e.limit == 0 || src.MaxConnections < e.limit) {
		e.limit = src.MaxConnections
	}
	switch {
	case src.TuneGapSeconds > 0:
		e.gap = time.Duration(src.TuneGapSeconds) * time.Second
	case src.TuneGapSeconds == 0 && e.limit > 0:
		e.gap = defaultTuneGap
	}
	c.mu.Lock()
	if c.gen == gen {
		if c.m == nil {
			c.m = map[string]authEntry{}
		}
		c.m[id] = e
	} else if cur, exists := c.m[id]; exists && !cur.disabled && s.o.Now().Sub(cur.at) < authCacheTTL {
		e = cur
	}
	c.mu.Unlock()
	return e, nil
}

// upstreamHeaders returns the request headers for fetching one channel input
// (its playlists, keys and segments, and ffmpeg's input). Credentials come
// from the input's AuthSourceID. Without one, only the playlist's own entry
// (the import-owned input, or a legacy channel's single input) inherits the
// channel's source; owner-added backups stay anonymous, so an input never
// receives another provider's credentials. The input's own headers (an
// add-on's proxy headers) apply last. The result is empty, never nil, when
// no authentication applies.
func (s *Service) upstreamHeaders(ctx context.Context, c Channel, input StreamSource) (http.Header, error) {
	id := input.AuthSourceID
	if id == "" && c.SourceID != "" && (input.ID == stableID(c.ID, "playlist") || len(c.StreamSources) <= 1) {
		id = c.SourceID
	}
	a, err := s.sourceAuth(ctx, id)
	if err != nil {
		return http.Header{}, err
	}
	h := a.header()
	for k, v := range input.Headers {
		h.Set(k, v)
	}
	return h, nil
}
