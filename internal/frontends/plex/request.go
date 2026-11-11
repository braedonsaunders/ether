package plex

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/clientnet"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Request is the parsed Plex request context: every X-Plex-* header (or its
// query-string equivalent), paging, the output format and the remote address.
type Request struct {
	Token           string
	ClientID        string // X-Plex-Client-Identifier
	Product         string // X-Plex-Product
	Version         string // X-Plex-Version
	Platform        string // X-Plex-Platform
	PlatformVersion string // X-Plex-Platform-Version
	Device          string // X-Plex-Device
	DeviceName      string // X-Plex-Device-Name
	Model           string // X-Plex-Model
	Vendor          string // X-Plex-Device-Vendor
	Language        string // X-Plex-Language

	Features     string // X-Plex-Features
	ProfileName  string // X-Plex-Client-Profile-Name
	ProfileExtra string // X-Plex-Client-Profile-Extra

	ClientCapabilities string // X-Plex-Client-Capabilities (legacy)
	SessionID          string // X-Plex-Session-Identifier

	// Paging (X-Plex-Container-Start/Size); HasPage is false when the client
	// did not ask for a window.
	Start         int
	Size          int
	HasPage       bool
	pagingInvalid bool

	Format model.Format
	// excludeMedia: the response leaves Media out (excludeElements=Media),
	// so listing rows skip building it.
	excludeMedia bool
	RemoteIP     netip.Addr
	Remote       bool // not on a private/loopback network

	User *frontend.User

	// Set only on a single playable detail request. A nil completion means
	// the refill was not admitted (or the search cache was already warm).
	detailRefillRequested bool
	detailRefill          <-chan struct{}
	detailWaitUntil       time.Time // shared Android TV detail probe/version wait budget

	// Capability is set when authentication used a stream grant or other
	// session-scoped credential, not a user token. Only transcode
	// session/segment handlers may serve a capability-scoped request.
	Capability bool

	versionItemKey frontend.RatingKey // single detail or selected queue item
	versionFlowKey frontend.RatingKey // selected pre-play/queue item only
	versionErr     error
	supportAttempt string // diagnostics-only request correlation; never sent to clients

	// diag is the identity the client actually sent, captured at parse time and
	// never rewritten by synthetic-device adoption. Support events attribute to
	// this wire identity so a capture started for a real device still records a
	// clientless native player's read as its viewer's (unattributed) traffic,
	// rather than dropping it under a synthetic device the capture never saw.
	diag *diagIdentity
}

type diagIdentity struct {
	ClientID string
	Product  string
	Platform string
}

// wireIdentity returns the identity the client sent, falling back to the live
// fields for a Request that was built outside ParseRequest (tests, zero value).
func (pr *Request) wireIdentity() diagIdentity {
	if pr.diag != nil {
		return *pr.diag
	}
	return diagIdentity{ClientID: pr.ClientID, Product: pr.Product, Platform: pr.Platform}
}

type ctxKey struct{}

// FromContext returns the request context attached by the middleware.
func FromContext(ctx context.Context) *Request {
	if r, ok := ctx.Value(ctxKey{}).(*Request); ok {
		return r
	}
	return &Request{}
}

func withRequest(ctx context.Context, pr *Request) context.Context {
	return context.WithValue(frontend.WithCatalogViewer(frontend.WithLiveApp(ctx, "plex"), pr.User), ctxKey{}, pr)
}

// plexParam reads an X-Plex-* value from the header or the query string.
func plexParam(r *http.Request, name string) string {
	if v := r.Header.Get(name); v != "" {
		return strings.TrimSpace(v)
	}
	q := r.URL.Query()
	if v := q.Get(name); v != "" {
		return strings.TrimSpace(v)
	}
	// Some clients lower-case query parameter names.
	for k, vs := range q {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

// pagingParam preserves presence and query whitespace. An empty header still
// overrides a query value; unlike client names, paging values are strict numbers.
func pagingParam(r *http.Request, name string) (string, bool) {
	if values, ok := r.Header[http.CanonicalHeaderKey(name)]; ok {
		if len(values) > 0 {
			return values[0], true
		}
		return "", true
	}
	q := r.URL.Query()
	if values, ok := q[name]; ok {
		if len(values) > 0 {
			return values[0], true
		}
		return "", true
	}
	return "", false
}

// ParseRequest builds the request context from an HTTP request. Forwarded
// addresses are considered only for a loopback peer (a local reverse proxy).
func ParseRequest(r *http.Request, trusted ...netip.Prefix) *Request {
	pr := &Request{
		Token:           plexParam(r, "X-Plex-Token"),
		ClientID:        plexParam(r, "X-Plex-Client-Identifier"),
		Product:         plexParam(r, "X-Plex-Product"),
		Version:         plexParam(r, "X-Plex-Version"),
		Platform:        plexParam(r, "X-Plex-Platform"),
		PlatformVersion: plexParam(r, "X-Plex-Platform-Version"),
		Device:          plexParam(r, "X-Plex-Device"),
		DeviceName:      plexParam(r, "X-Plex-Device-Name"),
		Model:           plexParam(r, "X-Plex-Model"),
		Vendor:          plexParam(r, "X-Plex-Device-Vendor"),
		Language:        requestLanguage(r),

		Features:     plexParam(r, "X-Plex-Features"),
		ProfileExtra: plexParam(r, "X-Plex-Client-Profile-Extra"),
		ProfileName:  profileParam(r, "X-Plex-Client-Profile-Name"),

		ClientCapabilities: plexParam(r, "X-Plex-Client-Capabilities"),
		SessionID:          plexParam(r, "X-Plex-Session-Identifier"),
	}
	if pr.ClientID == "" {
		pr.ClientID = plexParam(r, "X-Plex-Client-Id")
	}
	pr.diag = &diagIdentity{ClientID: pr.ClientID, Product: pr.Product, Platform: pr.Platform}
	if pr.SessionID == "" {
		pr.SessionID = plexParam(r, "X-Plex-Session-Id")
	}
	// PMS validates size whenever supplied, but start only alongside size.
	// A window requires both values and neither may be negative.
	startS, hasStart := pagingParam(r, "X-Plex-Container-Start")
	sizeS, hasSize := pagingParam(r, "X-Plex-Container-Size")
	if hasSize {
		size, sizeErr := strconv.ParseInt(sizeS, 10, 32)
		pr.pagingInvalid = sizeErr != nil
		if hasStart {
			start, startErr := strconv.ParseInt(startS, 10, 32)
			pr.pagingInvalid = pr.pagingInvalid || startErr != nil
			if !pr.pagingInvalid && start >= 0 && size >= 0 {
				pr.Start, pr.Size, pr.HasPage = int(start), int(size), true
			}
		}
	}
	pr.Platform = profileParam(r, "X-Plex-Platform")
	pr.Device = profileParam(r, "X-Plex-Device")
	pr.excludeMedia = excludesListMedia(r)
	pr.Format = model.XML
	if wantsJSON(r) {
		pr.Format = model.JSON
	}
	var local bool
	pr.RemoteIP, local = clientnet.ClientNet(r, trusted...)
	pr.Remote = !local
	return pr
}

// wantsJSON follows PMS's media-range grammar rather than HTTP's generic
// Accept parser. A qualifying range still participates at weight zero when
// its q value is invalid; absence of a qualifying range defaults to XML.
func wantsJSON(r *http.Request) bool {
	bestQ := -1.0
	json := false
	for _, a := range r.Header.Values("Accept") {
		for _, part := range strings.Split(a, ",") {
			mt, params, hasParams := strings.Cut(strings.TrimSpace(part), ";")
			mt = strings.TrimSpace(mt)
			if mt != "application/json" && mt != "application/xml" {
				continue
			}
			q := 1.0
			if hasParams {
				// The first parameter must start with exactly q=, with no
				// whitespace after the semicolon and no case folding.
				if !strings.HasPrefix(params, "q=") {
					continue
				}
				q = acceptQ(params)
			}
			if q > bestQ {
				bestQ, json = q, mt == "application/json"
			}
		}
	}
	return json
}

// acceptQ reads a single q parameter. Extra parameters and invalid or negative
// values retain the media range with weight zero.
func acceptQ(params string) float64 {
	if !strings.HasPrefix(params, "q=") || strings.Contains(params, ";") {
		return 0
	}
	q, err := strconv.ParseFloat(params[2:], 64)
	if err != nil || !(q >= 0) {
		return 0
	}
	return q
}

// window applies the client's paging request to a list of n items and
// returns [lo, hi).
func (pr *Request) window(n int) (lo, hi int) {
	if !pr.HasPage {
		return 0, n
	}
	lo = min(pr.Start, n)
	// Compare against the remaining length: lo+Size overflows for a huge
	// X-Plex-Container-Size and would yield hi < lo.
	if pr.Size < 0 || pr.Size >= n-lo {
		return lo, n
	}
	return lo, lo + pr.Size
}

// Browse offsets do not identify a new listing. Keep section, sort and filters,
// while excluding credentials and transport-only pagination parameters.
func browseScope(r *http.Request, pr *Request) string {
	q := r.URL.Query()
	for key := range q {
		switch strings.ToLower(key) {
		case "x-plex-container-start", "x-plex-container-size", "containerstart", "containersize", "start", "offset", "size", "count", "limit":
			delete(q, key)
		default:
			if traceSecretQuery(key) || strings.HasPrefix(strings.ToLower(key), "x-plex-") {
				delete(q, key)
			}
		}
	}
	return pr.ClientID + "|" + r.URL.Path + "|" + q.Encode()
}
