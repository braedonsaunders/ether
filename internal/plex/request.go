package plex

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Request is the parsed Plex request context: every X-Plex-* header (or its
// query-string equivalent), paging, the output format and the remote address.
type Request struct {
	Token              string
	ClientID           string // X-Plex-Client-Identifier
	Product            string // X-Plex-Product
	Version            string // X-Plex-Version
	Platform           string // X-Plex-Platform
	PlatformVersion    string // X-Plex-Platform-Version
	Device             string // X-Plex-Device
	DeviceName         string // X-Plex-Device-Name
	Model              string // X-Plex-Model
	Vendor             string // X-Plex-Device-Vendor
	Language           string // X-Plex-Language
	Marketplace        string // X-Plex-Marketplace
	Provides           string // X-Plex-Provides
	Features           string // X-Plex-Features
	ProfileExtra       string // X-Plex-Client-Profile-Extra
	ProfileName        string // X-Plex-Client-Profile-Name
	ClientCapabilities string // X-Plex-Client-Capabilities (legacy)
	SessionID          string // X-Plex-Session-Identifier
	PMSAPIVersion      string // X-Plex-Pms-Api-Version

	// Paging (X-Plex-Container-Start/Size); HasPage is false when the client
	// did not ask for a window.
	Start   int
	Size    int
	HasPage bool

	Format   model.Format
	RemoteIP netip.Addr
	Remote   bool // not on a private/loopback network

	User *core.User

	// Capability is set when authentication used a stream grant or other
	// session-scoped credential, not a user token. Only transcode
	// session/segment handlers may serve a capability-scoped request.
	Capability bool
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
	return context.WithValue(ctx, ctxKey{}, pr)
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

// ParseRequest builds the request context from an HTTP request. Forwarded
// addresses are considered only for a loopback peer (a local reverse proxy).
func ParseRequest(r *http.Request, trusted ...netip.Prefix) *Request {
	pr := &Request{
		Token:              plexParam(r, "X-Plex-Token"),
		ClientID:           plexParam(r, "X-Plex-Client-Identifier"),
		Product:            plexParam(r, "X-Plex-Product"),
		Version:            plexParam(r, "X-Plex-Version"),
		Platform:           plexParam(r, "X-Plex-Platform"),
		PlatformVersion:    plexParam(r, "X-Plex-Platform-Version"),
		Device:             plexParam(r, "X-Plex-Device"),
		DeviceName:         plexParam(r, "X-Plex-Device-Name"),
		Model:              plexParam(r, "X-Plex-Model"),
		Vendor:             plexParam(r, "X-Plex-Device-Vendor"),
		Language:           plexParam(r, "X-Plex-Language"),
		Marketplace:        plexParam(r, "X-Plex-Marketplace"),
		Provides:           plexParam(r, "X-Plex-Provides"),
		Features:           plexParam(r, "X-Plex-Features"),
		ProfileExtra:       plexParam(r, "X-Plex-Client-Profile-Extra"),
		ProfileName:        plexParam(r, "X-Plex-Client-Profile-Name"),
		ClientCapabilities: plexParam(r, "X-Plex-Client-Capabilities"),
		SessionID:          plexParam(r, "X-Plex-Session-Identifier"),
		PMSAPIVersion:      plexParam(r, "X-Plex-Pms-Api-Version"),
	}
	if pr.ClientID == "" {
		pr.ClientID = plexParam(r, "X-Plex-Client-Id")
	}
	if pr.SessionID == "" {
		pr.SessionID = plexParam(r, "X-Plex-Session-Id")
	}
	// Paging.
	startS := plexParam(r, "X-Plex-Container-Start")
	sizeS := plexParam(r, "X-Plex-Container-Size")
	if sizeS != "" || startS != "" {
		start, _ := strconv.Atoi(startS)
		size := -1
		if sizeS != "" {
			if n, err := strconv.Atoi(sizeS); err == nil {
				size = n
			}
		}
		if start < 0 {
			start = 0
		}
		pr.Start, pr.Size, pr.HasPage = start, size, true
	}
	pr.Format = model.XML
	if wantsJSON(r) {
		pr.Format = model.JSON
	}
	var local bool
	pr.RemoteIP, local = identity.ClientNet(r, trusted...)
	pr.Remote = !local
	return pr
}

// wantsJSON negotiates the response format from Accept like PMS: XML is the
// default, JSON only when application/json is acceptable and preferred over
// every explicitly listed XML type (higher q, or equal q and listed first).
// Clients such as plexapi send "application/xml, application/json;q=0.9" and
// must get XML; Plex Web sends "application/json, text/plain, */*" and gets JSON.
func wantsJSON(r *http.Request) bool {
	jsonQ, jsonPos := -1.0, 0
	xmlQ, xmlPos := -1.0, 0
	pos := 0
	for _, a := range r.Header.Values("Accept") {
		for _, part := range strings.Split(a, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			pos++
			mt, params, _ := strings.Cut(part, ";")
			mt = strings.ToLower(strings.TrimSpace(mt))
			q := acceptQ(params)
			switch mt {
			case "application/json":
				if q > jsonQ {
					jsonQ, jsonPos = q, pos
				}
			case "application/xml", "text/xml":
				if q > xmlQ {
					xmlQ, xmlPos = q, pos
				}
			}
		}
	}
	if jsonQ <= 0 {
		return false
	}
	if xmlQ <= 0 {
		return true
	}
	if jsonQ != xmlQ {
		return jsonQ > xmlQ
	}
	return jsonPos < xmlPos
}

// acceptQ returns the q parameter of an Accept media range (default 1).
func acceptQ(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || q < 0 {
			return 0
		}
		if q > 1 {
			return 1
		}
		return q
	}
	return 1
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
