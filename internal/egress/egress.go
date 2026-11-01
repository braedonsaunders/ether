// Package egress routes Ether's outbound dials. One optional, node-local
// proxy is off by default. While it is off, clients follow the process
// environment the way they did before the setting existed. While it is on,
// source traffic uses that proxy and does not fall back to a direct route,
// and Plex, updates, UPnP and remote access stay direct.
package egress

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// Capability is one class of outbound call.
type Capability int

const (
	// CapDirect is Plex, the update feed, UPnP, reachability and anything
	// else that must stay on the normal route while a source proxy is on.
	CapDirect Capability = iota
	// CapIndexers is Newznab, Torznab, built-in sites and their definitions.
	CapIndexers
	// CapStremio is addon APIs, NZB downloads and direct addon HTTP streams.
	CapStremio
	// CapDebrid is debrid APIs and debrid CDN streams.
	CapDebrid
	// CapUsenet is NNTP.
	CapUsenet
	// CapMetadata is catalogues, artwork, ratings, subtitles, Trakt, Simkl
	// and the language model. It stays direct unless the owner opts in.
	CapMetadata
	// CapProbe is the public address source traffic leaves from.
	CapProbe
)

// Policy is the live routing decision. Proxy is nil when there is nothing
// to dial. The URL may carry a username and password; callers must not log it.
type Policy struct {
	Enabled  bool
	Proxy    *url.URL
	Indexers bool
	Stremio  bool
	Debrid   bool
	Usenet   bool
	Metadata bool
	Bypass   []string
}

var current atomic.Pointer[Policy]

func init() { current.Store(&Policy{}) }

// ErrNotConfigured is returned when a routed call has no proxy to dial.
// The request fails. It is not retried on the normal route.
var ErrNotConfigured = errors.New("outbound proxy is not configured")

// Set replaces the process-wide policy. Tests should restore the previous
// policy with t.Cleanup.
func Set(p Policy) {
	if p.Proxy != nil {
		p.Proxy = cloneURL(p.Proxy)
	}
	if p.Bypass != nil {
		p.Bypass = append([]string(nil), p.Bypass...)
	}
	current.Store(&p)
}

// Current returns a copy of the live policy. The proxy URL is shared and
// must not be mutated.
func Current() Policy {
	p := current.Load()
	if p == nil {
		return Policy{}
	}
	return *p
}

// Use installs the owner setting. An enabled proxy with an unusable address
// stays enabled and fails closed. When the setting is on, it overrides
// HTTP_PROXY and HTTPS_PROXY for clients that use this package. ALL_PROXY
// is never read.
func Use(o config.Outbound, log *slog.Logger) {
	Set(PolicyFrom(o))
	noteEnv(o.Enabled, log)
}

// PolicyFrom builds a policy. Credentials from Username and Password are
// attached to the URL. An invalid URL yields an enabled policy with no
// proxy, so routed dials fail instead of going direct.
func PolicyFrom(o config.Outbound) Policy {
	p := Policy{
		Enabled:  o.Enabled,
		Indexers: o.Indexers,
		Stremio:  o.Stremio,
		Debrid:   o.Debrid,
		Usenet:   o.Usenet,
		Metadata: o.Metadata,
		Bypass:   append([]string(nil), o.Bypass...),
	}
	if !o.Enabled {
		return p
	}
	u, err := o.ProxyURL()
	if err != nil {
		return p
	}
	if o.Username != "" || o.Password != "" {
		u.User = url.UserPassword(o.Username, o.Password)
	}
	p.Proxy = u
	return p
}

// ProxyFunc is an http.Transport proxy selector for cap. It reads the live
// policy on every request.
func ProxyFunc(cap Capability) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		return Current().selectProxy(req, cap)
	}
}

// Transport is a default-like transport whose proxy selector follows cap.
func Transport(cap Capability) *http.Transport {
	tr := baseTransport()
	tr.Proxy = ProxyFunc(cap)
	return tr
}

// Client is an HTTP client for cap. A zero timeout means no client timeout.
// The proxy selector reads the live policy on every request.
func Client(cap Capability, timeout time.Duration) *http.Client {
	tr := baseTransport()
	tr.Proxy = ProxyFunc(cap)
	return &http.Client{Timeout: timeout, Transport: tr}
}

// SnapshotClient is an HTTP client bound to p, for a check that must not
// follow a policy saved later in the same process.
func (p Policy) SnapshotClient(cap Capability, timeout time.Duration) *http.Client {
	snap := p
	tr := baseTransport()
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		return snap.selectProxy(req, cap)
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

func baseTransport() *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		return dt.Clone()
	}
	return &http.Transport{ForceAttemptHTTP2: true}
}

func (p Policy) selectProxy(req *http.Request, cap Capability) (*url.URL, error) {
	if !p.Enabled {
		if req == nil {
			return nil, nil
		}
		return http.ProxyFromEnvironment(req)
	}
	if !p.routes(cap) {
		return nil, nil
	}
	if req != nil && bypass(requestHost(req), p.Bypass) {
		return nil, nil
	}
	if p.Proxy == nil || p.Proxy.Hostname() == "" {
		return nil, ErrNotConfigured
	}
	return p.Proxy, nil
}

func (p Policy) routes(cap Capability) bool {
	switch cap {
	case CapIndexers:
		return p.Indexers
	case CapStremio:
		return p.Stremio
	case CapDebrid:
		return p.Debrid
	case CapUsenet:
		return p.Usenet
	case CapMetadata:
		return p.Metadata
	case CapProbe:
		return p.Indexers || p.Stremio || p.Debrid || p.Usenet
	default:
		return false
	}
}

func requestHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Hostname()
}

func bypass(host string, extra []string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return host != ""
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
	}
	for _, b := range extra {
		b = strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(b)), "."), ".")
		if b == "" {
			continue
		}
		if host == b || strings.HasSuffix(host, "."+b) {
			return true
		}
	}
	return false
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	c := *u
	if u.User != nil {
		password, _ := u.User.Password()
		c.User = url.UserPassword(u.User.Username(), password)
	}
	return &c
}

var envOnce sync.Once

func noteEnv(enabled bool, log *slog.Logger) {
	if !enabled || log == nil {
		return
	}
	if os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" &&
		os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" &&
		os.Getenv("ALL_PROXY") == "" && os.Getenv("all_proxy") == "" {
		return
	}
	envOnce.Do(func() {
		log.Info("outbound proxy setting overrides HTTP_PROXY and HTTPS_PROXY; ALL_PROXY is ignored")
	})
}
