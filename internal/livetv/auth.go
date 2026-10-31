package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

// xtreamURLs derives an Xtream Codes panel's M3U and XMLTV URLs.
func (a *SourceAuth) xtreamURLs() (playlist, guide string) {
	base := strings.TrimRight(strings.TrimSpace(a.Server), "/")
	q := url.Values{"username": {a.Username}, "password": {a.Password}}
	output := a.Output
	if output == "" {
		output = "ts"
	}
	pq := url.Values{"username": {a.Username}, "password": {a.Password}, "type": {"m3u_plus"}, "output": {output}}
	return base + "/get.php?" + pq.Encode(), base + "/xmltv.php?" + q.Encode()
}

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
// kind names the document in limit errors ("playlist" or "guide").
func (s *Service) readURLWith(ctx context.Context, raw string, max int64, h http.Header, kind string) (string, error) {
	rc, err := s.openURL(ctx, raw, h)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := readLimited(rc, max, kind)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// openURL fetches a source document and leaves the body to the caller.
func (s *Service) openURL(ctx context.Context, raw string, h http.Header) (io.ReadCloser, error) {
	if !validHTTP(raw) {
		return nil, fmt.Errorf("source must use HTTP(S)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	res, err := s.o.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("source fetch failed")
	}
	switch {
	case res.StatusCode == 401 || res.StatusCode == 403:
		res.Body.Close()
		return nil, fmt.Errorf("source refused the credentials (HTTP %d)", res.StatusCode)
	case res.StatusCode != 200:
		res.Body.Close()
		return nil, fmt.Errorf("source returned HTTP %d", res.StatusCode)
	}
	return res.Body, nil
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
		return XMLGuide{}, err
	}
	defer closeReader(rd)
	g, _, err := parseXMLTV(rd, f)
	return g, err
}

// authCache keeps provider settings for the stream path, which asks for them
// on every segment. Imports and deletes drop the entry.
type authCache struct {
	mu sync.Mutex
	m  map[string]authEntry
}

type authEntry struct {
	auth *SourceAuth
	at   time.Time
}

const authCacheTTL = 30 * time.Second

func (s *Service) forgetSourceAuth(id string) {
	c := &s.authCache
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

func (s *Service) sourceAuth(ctx context.Context, id string) (*SourceAuth, error) {
	if id == "" {
		return nil, nil
	}
	c := &s.authCache
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok && s.o.Now().Sub(e.at) < authCacheTTL {
		return e.auth, nil
	}
	src, err := s.storedSourceByID(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		src, err = Source{}, nil
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]authEntry{}
	}
	c.m[id] = authEntry{auth: src.Auth, at: s.o.Now()}
	c.mu.Unlock()
	return src.Auth, nil
}

// upstreamHeaders returns the request headers for fetching one channel input
// (its playlists, keys and segments, and ffmpeg's input). Credentials come
// from the input's AuthSourceID. Without one, only the playlist's own entry
// (the import-owned input, or a legacy channel's single input) inherits the
// channel's source; owner-added backups stay anonymous, so an input never
// receives another provider's credentials. The result is empty, never nil,
// when no authentication applies.
func (s *Service) upstreamHeaders(ctx context.Context, c Channel, input StreamSource) (http.Header, error) {
	id := input.AuthSourceID
	if id == "" && c.SourceID != "" && (input.ID == stableID(c.ID, "playlist") || len(c.StreamSources) <= 1) {
		id = c.SourceID
	}
	a, err := s.sourceAuth(ctx, id)
	if err != nil {
		return http.Header{}, err
	}
	return a.header(), nil
}
