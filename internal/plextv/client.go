// Package plextv is Ether's plex.tv client: PIN sign-in, server claim,
// connection publishing, resources, shared users and their server access
// tokens, and plex.direct certificates. It also holds the Registrar that runs
// these periodically.
package plextv

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

const (
	// DefaultBaseURL is plex.tv's API host.
	DefaultBaseURL = "https://plex.tv"
	// DefaultServersURL is the PMS account API host observed in current PMS traffic.
	DefaultServersURL = "https://servers.plex.tv"
	// DefaultAuthAppURL is the hosted sign-in page PIN codes are approved on.
	DefaultAuthAppURL = "https://app.plex.tv/auth"
	// DefaultServerVersion is the PMS version Ether reports towards plex.tv
	// and clients. Official apps gate features on server version, so it
	// tracks a recent PMS release rather than Ether's own version.
	DefaultServerVersion = "1.43.4.10903-e5521bd8c"
	// ServerProduct is the product name of a Plex Media Server device.
	ServerProduct = "Plex Media Server"
	// AppProduct is the product shown for the setup wizard's sign-in.
	AppProduct = "Ether"
)

// ErrRateLimited matches plex.tv 429 responses (errors.Is).
var ErrRateLimited = errors.New("plex.tv rate limit exceeded")

// Identity is the set of X-Plex-* headers a client sends to plex.tv.
type Identity struct {
	Product          string
	Version          string
	ClientIdentifier string
	DeviceName       string
	Device           string
	Platform         string
	PlatformVersion  string
	// Provides is "server" for the server identity, empty for the wizard.
	Provides string
}

// ServerIdentity is how Ether presents itself as a server: the same headers
// PMS sends, with the machine identifier as client identifier, because
// plex.tv keys a server's device record (and clients' resources entries) on
// it.
func ServerIdentity(cfg config.Config, version string) Identity {
	if version == "" {
		version = PMSVersion(cfg)
	}
	name := cfg.Server.FriendlyName
	if name == "" {
		name = "Ether"
	}
	return Identity{
		Product:          ServerProduct,
		Version:          version,
		ClientIdentifier: cfg.Server.MachineIdentifier,
		DeviceName:       name,
		Device:           platformName(),
		Platform:         platformName(),
		PlatformVersion:  runtime.GOARCH,
		Provides:         "server",
	}
}

// AppIdentity is the setup wizard's identity for PIN sign-in. It uses
// config.Plex.ClientIdentifier so the owner's plex.tv "authorized devices"
// list shows an "Ether" entry distinct from the server device.
func AppIdentity(cfg config.Config, version string) Identity {
	if version == "" {
		version = "1.0"
	}
	name := cfg.Server.FriendlyName
	if name == "" {
		name = "Ether"
	}
	return Identity{
		Product:          AppProduct,
		Version:          version,
		ClientIdentifier: cfg.Plex.ClientIdentifier,
		DeviceName:       name + " setup",
		Device:           platformName(),
		Platform:         "Web",
		PlatformVersion:  runtime.GOOS,
	}
}

func platformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOSX"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return runtime.GOOS
	}
}

// Options configures a Client.
type Options struct {
	VersionSource func() string
	BaseURL       string // DefaultBaseURL when empty
	AuthAppURL    string // DefaultAuthAppURL when empty
	HTTPClient    *http.Client
	Identity      Identity
	Logger        *slog.Logger
	// RetryBackoff is the initial delay between retries of idempotent
	// requests (default 500ms).
	RetryBackoff time.Duration
}

// Client talks to plex.tv with one identity. It is safe for concurrent use.
type Client struct {
	base          *url.URL
	authApp       string
	hc            *http.Client
	id            Identity
	log           *slog.Logger
	backoff       time.Duration
	versionSource func() string
}

// New returns a client.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimRight(o.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("plextv: base url: %w", err)
	}
	if o.AuthAppURL == "" {
		o.AuthAppURL = DefaultAuthAppURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapDirect, 30*time.Second)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = 500 * time.Millisecond
	}
	return &Client{base: base, authApp: o.AuthAppURL, hc: o.HTTPClient, id: o.Identity,
		log: o.Logger.With("component", "plextv"), backoff: o.RetryBackoff, versionSource: o.VersionSource}, nil
}

// WithIdentity returns a copy of c that sends id.
func (c *Client) WithIdentity(id Identity) *Client {
	cp := *c
	cp.id = id
	return &cp
}

// Identity returns the identity c sends.
func (c *Client) Identity() Identity { return c.id }

// BaseURL returns the plex.tv base URL.
func (c *Client) BaseURL() string { return c.base.String() }

func (c *Client) serversHost() string {
	if c.base.String() == DefaultBaseURL {
		return DefaultServersURL
	}
	return ""
}

// APIError is a non-2xx plex.tv response.
type APIError struct {
	Method     string
	Path       string
	Status     int
	Code       int // plex.tv error code, e.g. 1001
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != 0 {
		return fmt.Sprintf("plex.tv %s %s: %d (code %d): %s", e.Method, e.Path, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("plex.tv %s %s: %d: %s", e.Method, e.Path, e.Status, msg)
}

// Is maps HTTP statuses to sentinel errors: 401/403 -> core.ErrUnauthorized,
// 404 -> core.ErrNotFound, 429 -> ErrRateLimited.
func (e *APIError) Is(target error) bool {
	switch target {
	case core.ErrUnauthorized:
		return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	case core.ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrRateLimited:
		return e.Status == http.StatusTooManyRequests
	}
	return false
}

type request struct {
	method     string
	path       string
	host       string // optional service host override (e.g. servers.plex.tv)
	query      url.Values
	token      string
	tokenQuery string // also send the token as this query parameter (legacy endpoints)
	// queryTokenOnly sends the token only as tokenQuery, without the
	// X-Plex-Token header (PMS's servers.xml POST).
	queryTokenOnly bool
	body           []byte
	contentType    string
	accept         string // default application/json
	okStatus       []int  // accepted statuses besides 2xx
}

type response struct {
	status int
	header http.Header
	body   []byte
}

const maxBody = 8 << 20

// do sends r, retrying idempotent requests on transport errors and 5xx.
func (c *Client) do(ctx context.Context, r request) (*response, error) {
	attempts := 1
	if r.method == http.MethodGet || r.method == http.MethodPut {
		attempts = 3
	}
	delay := c.backoff
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
			delay *= 2
		}
		resp, err := c.once(ctx, r)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Status < 500 {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, r request) (*response, error) {
	u := *c.base
	if r.host != "" {
		host, err := url.Parse(r.host)
		if err != nil || host.Scheme == "" || host.Host == "" {
			return nil, fmt.Errorf("plex.tv: invalid service host")
		}
		u.Scheme, u.Host = host.Scheme, host.Host
	}
	u.Path = strings.TrimRight(u.Path, "/") + r.path
	q := url.Values{}
	for k, v := range r.query {
		q[k] = v
	}
	if r.tokenQuery != "" && r.token != "" {
		q.Set(r.tokenQuery, r.token)
	}
	u.RawQuery = q.Encode()
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
	if err != nil {
		return nil, err
	}
	headerToken := r.token
	if r.queryTokenOnly && r.tokenQuery != "" {
		headerToken = ""
	}
	c.setHeaders(req.Header, headerToken)
	accept := r.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex.tv %s %s: %w", r.method, r.path, redactURLError(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("plex.tv %s %s: read: %w", r.method, r.path, err)
	}
	c.log.Debug("plex.tv request", "method", r.method, "path", r.path, "status", resp.StatusCode, "took", time.Since(start).Round(time.Millisecond))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	for _, s := range r.okStatus {
		if resp.StatusCode == s {
			ok = true
		}
	}
	if !ok {
		return nil, parseAPIError(r.method, r.path, resp, b)
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// redactURLError drops the query string from a transport error's URL.
// net/http's *url.Error quotes the full request URL, and several plex.tv
// endpoints carry the server token (auth_token, X-Plex-Token) or a Home PIN
// in the query, so the raw error would leak them into logs, the admin
// status and API error messages.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := ue.URL
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery, u.Fragment, u.User = "", "", nil
		safe = u.String()
	} else if i := strings.IndexAny(safe, "?#"); i >= 0 {
		safe = safe[:i]
	}
	return &url.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}

func (c *Client) setHeaders(h http.Header, token string) {
	set := func(k, v string) {
		if v != "" {
			h.Set(k, v)
		}
	}
	set("X-Plex-Product", c.id.Product)
	version := c.id.Version
	if c.id.Provides == "server" && c.versionSource != nil {
		version = c.versionSource()
	}
	set("X-Plex-Version", version)
	set("X-Plex-Client-Identifier", c.id.ClientIdentifier)
	set("X-Plex-Device-Name", c.id.DeviceName)
	set("X-Plex-Device", c.id.Device)
	set("X-Plex-Platform", c.id.Platform)
	set("X-Plex-Platform-Version", c.id.PlatformVersion)
	set("X-Plex-Provides", c.id.Provides)
	set("X-Plex-Token", token)
	h.Set("User-Agent", c.id.Product+"/"+version)
}

func parseAPIError(method, path string, resp *http.Response, body []byte) *APIError {
	e := &APIError{Method: method, Path: path, Status: resp.StatusCode}
	e.RetryAfter, _ = retryafter.Parse(resp.Header.Get("Retry-After"), time.Now(), 0)
	trim := bytes.TrimSpace(body)
	switch {
	case bytes.HasPrefix(trim, []byte("{")):
		var v struct {
			Errors []struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"errors"`
			Error string `json:"error"`
		}
		if json.Unmarshal(trim, &v) == nil {
			if len(v.Errors) > 0 {
				e.Code, e.Message = v.Errors[0].Code, v.Errors[0].Message
			} else {
				e.Message = v.Error
			}
		}
	case bytes.HasPrefix(trim, []byte("<")):
		n, err := parseXMLNode(trim)
		if err == nil {
			if el := n.find("error"); el != nil {
				e.Message = strings.TrimSpace(el.Text)
				if m := el.attr("message"); m != "" {
					e.Message = m
				}
				e.Code, _ = strconv.Atoi(el.attr("code"))
			}
		}
	default:
		if len(trim) > 0 && len(trim) < 300 {
			e.Message = string(trim)
		}
	}
	return e
}

// xmlNode is a generic XML element, used for plex.tv's older XML endpoints
// whose attribute names vary between snake_case and camelCase.
type xmlNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xmlNode  `xml:",any"`
	Text    string     `xml:",chardata"`
}

func parseXMLNode(b []byte) (*xmlNode, error) {
	var n xmlNode
	if err := xml.Unmarshal(b, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func normName(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(s))
}

// attr returns the first attribute (or child element text) matching any of
// names, compared case-insensitively and ignoring '_' and '-'.
func (n *xmlNode) attr(names ...string) string {
	for _, name := range names {
		want := normName(name)
		for _, a := range n.Attrs {
			if normName(a.Name.Local) == want {
				return a.Value
			}
		}
		for i := range n.Nodes {
			c := &n.Nodes[i]
			if normName(c.XMLName.Local) == want && len(c.Nodes) == 0 {
				return strings.TrimSpace(c.Text)
			}
		}
	}
	return ""
}

func (n *xmlNode) boolAttr(names ...string) bool {
	v := strings.ToLower(n.attr(names...))
	return v == "1" || v == "true" || v == "yes"
}

func (n *xmlNode) intAttr(names ...string) int64 {
	v, _ := strconv.ParseInt(n.attr(names...), 10, 64)
	return v
}

// find returns the first element (depth-first, including n) named name.
func (n *xmlNode) find(name string) *xmlNode {
	want := normName(name)
	if normName(n.XMLName.Local) == want {
		return n
	}
	for i := range n.Nodes {
		if f := n.Nodes[i].find(name); f != nil {
			return f
		}
	}
	return nil
}

// findAll returns every element named name below (and including) n.
func (n *xmlNode) findAll(name string) []*xmlNode {
	want := normName(name)
	var out []*xmlNode
	var walk func(x *xmlNode)
	walk = func(x *xmlNode) {
		if normName(x.XMLName.Local) == want {
			out = append(out, x)
			return
		}
		for i := range x.Nodes {
			walk(&x.Nodes[i])
		}
	}
	walk(n)
	return out
}

// flexTime decodes RFC 3339 strings and unix-second numbers.
type flexTime struct{ time.Time }

func (t *flexTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		t.Time = time.Unix(n, 0).UTC()
		return nil
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil // tolerate unexpected formats rather than fail the whole decode
	}
	t.Time = v
	return nil
}

func (t flexTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte(`null`), nil
	}
	return json.Marshal(t.Time)
}
