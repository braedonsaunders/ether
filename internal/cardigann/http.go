package cardigann

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
)

const (
	defaultUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	defaultHTTPTimeout = 30 * time.Second
	maxResponseBytes   = 10 << 20
	maxRedirects       = 10
	flareMaxTimeout    = 60 * time.Second
	// maxRequestDelay caps a definition's requestDelay.
	maxRequestDelay = time.Minute
)

// ErrCloudflare reports a site behind a Cloudflare (or similar) browser
// challenge that could not be passed.
var ErrCloudflare = errors.New("site is protected by a browser challenge")

// ErrCaptcha reports a login that needs a captcha.
var ErrCaptcha = errors.New("the site's sign-in requires a captcha, which Ether cannot solve")

// httpSession is one indexer's HTTP state: cookie jar, user agent,
// request pacing and challenge solving.
type httpSession struct {
	client  *http.Client
	jar     *resettableJar
	enc     encoding.Encoding // site charset; nil = UTF-8
	delay   time.Duration
	flare   func() string // FlareSolverr base URL ("" = not configured)
	flareHC *http.Client
	log     *slog.Logger
	site    string // display name for errors

	mu   sync.Mutex
	ua   string
	last time.Time
}

func newHTTPSession(def *Definition, hc *http.Client, flare func() string, log *slog.Logger) *httpSession {
	jar := newResettableJar()
	base := hc
	if base == nil {
		base = egress.Client(egress.CapIndexers, defaultHTTPTimeout)
	}
	c := &http.Client{
		Transport: base.Transport,
		Timeout:   base.Timeout,
		Jar:       jar,
		// Redirects are followed by hand so magnet redirects, login
		// detection and POST→GET semantics are under our control.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if c.Timeout == 0 {
		c.Timeout = defaultHTTPTimeout
	}
	s := &httpSession{client: c, jar: jar, flare: flare, log: log, ua: defaultUserAgent, site: def.Name, flareHC: egress.Client(egress.CapDirect, flareMaxTimeout+30*time.Second)}
	if def.RequestDelay > 0 {
		// Bounded before converting: an absurd value (1e30, .inf) would
		// overflow the Duration and stall or skip pacing.
		s.delay = time.Duration(min(def.RequestDelay, maxRequestDelay.Seconds()) * float64(time.Second))
	}
	if def.Encoding != "" && !strings.EqualFold(def.Encoding, "utf-8") {
		if e, err := htmlindex.Get(def.Encoding); err == nil {
			s.enc = e
		}
	}
	return s
}

// request is one HTTP request a definition issues.
type request struct {
	method  string
	url     string
	form    []KV   // encoded in the site charset
	raw     string // pre-encoded query appended as is ($raw inputs)
	headers http.Header
	follow  bool
}

// response is a fully read HTTP response.
type response struct {
	status   int
	header   http.Header
	body     []byte
	url      *url.URL
	location string // redirect target that was not followed
}

func (r *response) redirected() bool { return r.location != "" }

// text decodes the body to UTF-8 using the site charset or the response's
// declared charset.
func (s *httpSession) text(r *response) string {
	enc := s.enc
	if enc == nil {
		if _, params, err := mime.ParseMediaType(r.header.Get("Content-Type")); err == nil {
			if cs := params["charset"]; cs != "" && !strings.EqualFold(cs, "utf-8") {
				if e, err := htmlindex.Get(cs); err == nil {
					enc = e
				}
			}
		}
	}
	if enc != nil {
		if b, err := enc.NewDecoder().Bytes(r.body); err == nil {
			return string(b)
		}
	}
	return string(r.body)
}

// encodeValue encodes a form value in the site charset.
func (s *httpSession) encodeValue(v string) string {
	if s.enc != nil {
		if b, err := s.enc.NewEncoder().String(v); err == nil {
			v = b
		}
	}
	return url.QueryEscape(v)
}

func (s *httpSession) encodeForm(form []KV) string {
	parts := make([]string, 0, len(form))
	for _, kv := range form {
		parts = append(parts, s.encodeValue(kv.Key)+"="+s.encodeValue(kv.Value))
	}
	return strings.Join(parts, "&")
}

func (s *httpSession) userAgent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ua
}

// pace waits out the definition's request delay.
func (s *httpSession) pace(ctx context.Context) error {
	if s.delay <= 0 {
		return nil
	}
	s.mu.Lock()
	wait := time.Until(s.last.Add(s.delay))
	if wait < 0 {
		wait = 0
	}
	s.last = time.Now().Add(wait)
	s.mu.Unlock()
	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// do issues a request, following redirects when asked and passing a
// Cloudflare challenge through FlareSolverr when one is configured.
func (s *httpSession) do(ctx context.Context, req request) (*response, error) {
	resp, err := s.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if !isChallenge(resp) {
		return resp, nil
	}
	flare := ""
	if s.flare != nil {
		flare = strings.TrimSpace(s.flare())
	}
	if flare == "" {
		return nil, fmt.Errorf("%s: %w; set a FlareSolverr URL under Sources › Indexers to reach it", s.site, ErrCloudflare)
	}
	target := req.url
	if resp.url != nil {
		target = resp.url.String()
	}
	solved, err := s.solve(ctx, flare, target)
	if err != nil {
		return nil, err
	}
	resp, err = s.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if isChallenge(resp) {
		if strings.EqualFold(req.method, http.MethodGet) || req.method == "" {
			// The clearance cookie did not carry over (some challenges bind
			// it to the solver's TLS fingerprint): use the solver's page.
			if solved.Response != "" {
				return &response{status: http.StatusOK, header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, body: []byte(solved.Response), url: resp.url}, nil
			}
		}
		return nil, fmt.Errorf("%s: %w even after FlareSolverr solved it", s.site, ErrCloudflare)
	}
	return resp, nil
}

func (s *httpSession) roundTrip(ctx context.Context, req request) (*response, error) {
	method := strings.ToUpper(req.method)
	if method == "" {
		method = http.MethodGet
	}
	target, err := url.Parse(req.url)
	if err != nil {
		return nil, fmt.Errorf("bad URL: %w", err)
	}
	var body string
	if len(req.form) > 0 || req.raw != "" {
		enc := s.encodeForm(req.form)
		if req.raw != "" {
			if enc != "" {
				enc += "&"
			}
			enc += strings.TrimPrefix(req.raw, "&")
		}
		if method == http.MethodPost {
			body = enc
		} else if enc != "" {
			if target.RawQuery != "" {
				target.RawQuery += "&" + enc
			} else {
				target.RawQuery = enc
			}
		}
	}
	for hop := 0; ; hop++ {
		if err := s.pace(ctx); err != nil {
			return nil, err
		}
		var rd io.Reader
		if method == http.MethodPost {
			rd = strings.NewReader(body)
		}
		hreq, err := http.NewRequestWithContext(ctx, method, target.String(), rd)
		if err != nil {
			return nil, err
		}
		for k, vs := range req.headers {
			for _, v := range vs {
				hreq.Header.Add(k, v)
			}
		}
		if hreq.Header.Get("User-Agent") == "" {
			hreq.Header.Set("User-Agent", s.userAgent())
		}
		if hreq.Header.Get("Accept") == "" {
			hreq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json;q=0.8,*/*;q=0.7")
		}
		if method == http.MethodPost && hreq.Header.Get("Content-Type") == "" {
			hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if s.log != nil {
			s.log.Debug("cardigann request", "site", s.site, "method", method, "host", target.Host, "path", target.Path)
		}
		hresp, err := s.client.Do(hreq)
		if err != nil {
			// url.Error embeds the URL, which may carry passkeys.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return nil, fmt.Errorf("%s: request to %s failed: %w", s.site, target.Host, err)
		}
		data, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes+1))
		hresp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: read response: %w", s.site, err)
		}
		if len(data) > maxResponseBytes {
			return nil, fmt.Errorf("%s: response exceeds %d bytes", s.site, maxResponseBytes)
		}
		resp := &response{status: hresp.StatusCode, header: hresp.Header, body: data, url: target}
		loc := hresp.Header.Get("Location")
		if hresp.StatusCode/100 != 3 || loc == "" {
			return resp, nil
		}
		next, err := target.Parse(loc)
		if err != nil {
			resp.location = loc
			return resp, nil
		}
		if next.Scheme == "magnet" || !req.follow || hop >= maxRedirects {
			resp.location = next.String()
			return resp, nil
		}
		if hresp.StatusCode != http.StatusTemporaryRedirect && hresp.StatusCode != http.StatusPermanentRedirect {
			method, body = http.MethodGet, ""
		}
		target = next
	}
}

// isChallenge recognises Cloudflare and DDoS-Guard browser challenges.
func isChallenge(r *response) bool {
	if r.status != http.StatusForbidden && r.status != http.StatusServiceUnavailable && r.status != 429 {
		return false
	}
	server := strings.ToLower(r.header.Get("Server"))
	head := r.body
	if len(head) > 64<<10 {
		head = head[:64<<10]
	}
	lower := bytes.ToLower(head)
	strong := []string{"cf_chl_opt", "challenge-platform", "cf-browser-verification", "__cf_chl_", "ddos-guard/js-challenge"}
	for _, m := range strong {
		if bytes.Contains(lower, []byte(m)) {
			return true
		}
	}
	if strings.Contains(server, "cloudflare") || strings.Contains(server, "ddos-guard") {
		for _, m := range []string{"<title>just a moment", "<title>attention required", "cf-challenge", "<title>ddos-guard", "checking your browser"} {
			if bytes.Contains(lower, []byte(m)) {
				return true
			}
		}
	}
	return false
}

// flareSolution is the part of FlareSolverr's reply Ether uses.
type flareSolution struct {
	URL       string `json:"url"`
	Status    int    `json:"status"`
	Response  string `json:"response"`
	UserAgent string `json:"userAgent"`
	Cookies   []struct {
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Domain   string  `json:"domain"`
		Path     string  `json:"path"`
		Expires  float64 `json:"expires"`
		HTTPOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
	} `json:"cookies"`
}

// solve asks FlareSolverr to load target in its browser, then adopts the
// clearance cookies and user agent so direct requests pass.
func (s *httpSession) solve(ctx context.Context, flare, target string) (*flareSolution, error) {
	endpoint := strings.TrimRight(flare, "/")
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/v1"
	}
	payload, _ := json.Marshal(map[string]any{"cmd": "request.get", "url": target, "maxTimeout": int(flareMaxTimeout / time.Millisecond)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("FlareSolverr URL: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.log != nil {
		s.log.Info("cardigann: solving browser challenge through FlareSolverr", "site", s.site)
	}
	resp, err := s.flareHC.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s: FlareSolverr unreachable: %w", s.site, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%s: FlareSolverr: %w", s.site, err)
	}
	var out struct {
		Status   string        `json:"status"`
		Message  string        `json:"message"`
		Solution flareSolution `json:"solution"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: FlareSolverr returned an unexpected reply (HTTP %d)", s.site, resp.StatusCode)
	}
	if out.Status != "ok" {
		msg := out.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s: FlareSolverr could not solve the challenge: %s", s.site, msg)
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	var cookies []*http.Cookie
	for _, c := range out.Solution.Cookies {
		ck := &http.Cookie{Name: c.Name, Value: c.Value, Domain: strings.TrimPrefix(c.Domain, "."), Path: c.Path, HttpOnly: c.HTTPOnly, Secure: c.Secure}
		if ck.Path == "" {
			ck.Path = "/"
		}
		if c.Expires > 0 {
			ck.Expires = time.Unix(int64(c.Expires), 0)
		}
		cookies = append(cookies, ck)
	}
	s.jar.SetCookies(u, cookies)
	if ua := strings.TrimSpace(out.Solution.UserAgent); ua != "" {
		s.mu.Lock()
		s.ua = ua
		s.mu.Unlock()
	}
	return &out.Solution, nil
}

// savedCookie is a persisted session cookie.
type savedCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// exportCookies returns the cookies the jar would send to the site.
func (s *httpSession) exportCookies(site *url.URL) []savedCookie {
	var out []savedCookie
	for _, c := range s.jar.Cookies(site) {
		out = append(out, savedCookie{Name: c.Name, Value: c.Value})
	}
	return out
}

// importCookies restores persisted cookies for the site.
func (s *httpSession) importCookies(site *url.URL, cookies []savedCookie) {
	var list []*http.Cookie
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		list = append(list, &http.Cookie{Name: c.Name, Value: c.Value, Path: "/"})
	}
	if len(list) > 0 {
		s.jar.SetCookies(site, list)
	}
}

// setCookieString adds "a=b; c=d" cookies (cookie login).
func (s *httpSession) setCookieString(site *url.URL, raw string) int {
	var list []*http.Cookie
	for _, part := range strings.Split(raw, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		list = append(list, &http.Cookie{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value), Path: "/"})
	}
	if len(list) > 0 {
		s.jar.SetCookies(site, list)
	}
	return len(list)
}

// clearCookies drops the session's cookies before a fresh login, keeping
// browser-challenge clearance cookies (cf_*, __cf*, __ddg*) for the site.
func (s *httpSession) clearCookies(site *url.URL) {
	var keep []*http.Cookie
	for _, c := range s.jar.Cookies(site) {
		n := strings.ToLower(c.Name)
		if strings.HasPrefix(n, "cf_") || strings.HasPrefix(n, "__cf") || strings.HasPrefix(n, "__ddg") {
			keep = append(keep, &http.Cookie{Name: c.Name, Value: c.Value, Path: "/"})
		}
	}
	s.jar.reset()
	if len(keep) > 0 {
		s.jar.SetCookies(site, keep)
	}
}

// resettableJar is a cookie jar that can be emptied while requests run.
type resettableJar struct {
	mu  sync.RWMutex
	jar *cookiejar.Jar
}

func newResettableJar() *resettableJar {
	j, _ := cookiejar.New(nil)
	return &resettableJar{jar: j}
}

func (j *resettableJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	j.jar.SetCookies(u, cookies)
}

func (j *resettableJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.jar.Cookies(u)
}

func (j *resettableJar) reset() {
	fresh, _ := cookiejar.New(nil)
	j.mu.Lock()
	j.jar = fresh
	j.mu.Unlock()
}
