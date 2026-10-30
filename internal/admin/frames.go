package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Only owner-configured origins enter this policy; never reflect Origin or
// Referer. Invalid file-edited settings also fail closed at request time.
func frameOrigins(origins []string) ([]string, error) {
	if len(origins) > 16 {
		return nil, errors.New("allow at most 16 exact HTTP(S) origins")
	}
	out := make([]string, 0, len(origins))
	for _, raw := range origins {
		u, err := url.Parse(raw)
		if err != nil || len(raw) > 512 || strings.ContainsAny(raw, "\r\n\t ;'\"\\*%") || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("use exact HTTP(S) origins without credentials, paths, queries, fragments or wildcards")
		}
		host := u.Hostname()
		if net.ParseIP(host) == nil {
			if host == "" || strings.HasPrefix(host, ".") || strings.Contains(host, "..") {
				return nil, errors.New("invalid frame origin host")
			}
			for _, c := range host {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
					return nil, errors.New("invalid frame origin host")
				}
			}
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, errors.New("invalid frame origin port")
			}
		} else if strings.HasSuffix(u.Host, ":") {
			return nil, errors.New("invalid frame origin port")
		}
		out = append(out, u.Scheme+"://"+strings.ToLower(u.Host))
	}
	return out, nil
}

func (h *Handler) frameHeaders(headers http.Header) {
	origins, err := frameOrigins(h.deps.Config.Get().Server.WebFrameOrigins)
	if err != nil || len(origins) == 0 {
		headers.Set("X-Frame-Options", "DENY")
		headers.Set("Content-Security-Policy", "frame-ancestors 'none'; "+consoleScriptPolicy())
		return
	}
	// X-Frame-Options cannot express a cross-origin allowlist. CSP checks all
	// ancestors and allows only these explicitly trusted dashboard origins.
	headers.Del("X-Frame-Options")
	headers.Set("Content-Security-Policy", "frame-ancestors "+strings.Join(origins, " ")+"; "+consoleScriptPolicy())
}

// consoleScriptPolicy limits the console to its own bundled scripts plus the
// inline scripts the build put in index.html (the pre-paint theme script),
// pinned by hash. The hashes come from the embedded file itself, so they
// always match what is served. hls.js may run its transmuxer in a blob:
// worker.
var consoleScriptPolicy = sync.OnceValue(func() string {
	index, _ := fs.ReadFile(distFS, "dist/index.html")
	return scriptPolicy(index)
})

var inlineScript = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)

func scriptPolicy(index []byte) string {
	src := "'self'"
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		if bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
			continue
		}
		sum := sha256.Sum256(m[2])
		src += " 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	return "script-src " + src + "; worker-src 'self' blob:; object-src 'none'; base-uri 'self'"
}
