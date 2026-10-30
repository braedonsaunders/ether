package mediabrowser

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// lazySourceURL returns a scoped, absolute HTTP path. Issuing the grant is an
// estimate: the broker opens a release only when the capability is fetched.
func (i *instance) lazySourceURL(r *http.Request, credential string, plan frontend.PlanRequest) string {
	if i.host.LazyStreams == nil || i.host.SurfaceURL == nil || credential == "" {
		return ""
	}
	base, err := i.host.SurfaceURL(i.identityContext(r), r)
	if err != nil {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	path, err := i.host.LazyStreams.Issue(i.identityContext(r), credential, plan)
	if err != nil || !strings.HasPrefix(path, "/sessions/lazy/") || strings.ContainsAny(path, "?#\\\r\n") {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[3] == "" || parts[4] != "file" || parts[3] == "." || parts[3] == ".." {
		return ""
	}
	for _, c := range parts[3] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return strings.TrimRight(base, "/") + path
}

// lazyHLSSourceURL accepts only the broker's scoped master capability. The
// caller must first establish resolved HLS eligibility; issuance reserves no
// encoder capacity, and media requests must stay on the encoder's node.
func (i *instance) lazyHLSSourceURL(r *http.Request, credential string, plan frontend.PlanRequest) string {
	issuer, ok := i.host.LazyStreams.(frontend.LazyHLSStreams)
	if !ok || i.host.SurfaceURL == nil || credential == "" {
		return ""
	}
	base, err := i.host.SurfaceURL(i.identityContext(r), r)
	if err != nil {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	path, err := issuer.IssueHLS(i.identityContext(r), credential, plan, frontend.StreamRequest{Format: frontend.StreamHLS, Position: plan.Intent.Position})
	if err != nil {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "sessions" || parts[2] != "lazy" || !embySafeGrant(parts[3]) || parts[4] != "hls" || parts[5] != "master.m3u8" {
		return ""
	}
	return strings.TrimRight(base, "/") + path
}

func (i *instance) lazyMedia(w http.ResponseWriter, r *http.Request) {
	if i.host.LazyStreams == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	i.host.LazyStreams.ServeHTTP(w, r)
}

// absoluteMediaURL turns a broker grant path into a reachable surface URL.
// The server base is host-verified; client Host and forwarding headers are not used.
func (i *instance) absoluteMediaURL(r *http.Request, path string) string {
	if i.host.SurfaceURL == nil || !strings.HasPrefix(path, "/sessions/media/") || strings.ContainsAny(path, "?#%\\\r\n") {
		return ""
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return ""
		}
	}
	base, err := i.host.SurfaceURL(i.identityContext(r), r)
	if err != nil {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.TrimRight(base, "/") + path
}
