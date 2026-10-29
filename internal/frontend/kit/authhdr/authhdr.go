// Package authhdr parses client authentication headers without exposing tokens in URLs.
package authhdr

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

var ErrMalformed = errors.New("malformed authorization header")

// MediaBrowser parses the case-sensitive key/value parameters of a
// MediaBrowser authorization value. Quoted values may contain commas and
// backslash-escaped quotes. Percent escapes are decoded after parsing.
func MediaBrowser(value string) (map[string]string, error) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "MediaBrowser") {
		return nil, ErrMalformed
	}
	out := make(map[string]string)
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		if rest == "" {
			return out, nil
		}
		idx := strings.IndexByte(rest, '=')
		if idx <= 0 {
			return nil, ErrMalformed
		}
		key := strings.TrimSpace(rest[:idx])
		if key == "" || strings.ContainsAny(key, ", \t\r\n\"") {
			return nil, ErrMalformed
		}
		rest = strings.TrimLeftFunc(rest[idx+1:], unicode.IsSpace)
		var raw string
		if strings.HasPrefix(rest, "\"") {
			rest = rest[1:]
			var b strings.Builder
			closed := false
			for len(rest) > 0 {
				c := rest[0]
				rest = rest[1:]
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' {
					if len(rest) == 0 {
						return nil, ErrMalformed
					}
					c = rest[0]
					rest = rest[1:]
				}
				b.WriteByte(c)
			}
			if !closed {
				return nil, ErrMalformed
			}
			raw = b.String()
		} else {
			idx = strings.IndexByte(rest, ',')
			if idx < 0 {
				raw = strings.TrimSpace(rest)
				rest = ""
			} else {
				raw = strings.TrimSpace(rest[:idx])
				rest = rest[idx:]
			}
		}
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			return nil, ErrMalformed
		}
		if _, exists := out[key]; exists {
			return nil, ErrMalformed
		}
		out[key] = decoded
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		if rest == "" {
			return out, nil
		}
		if rest[0] != ',' {
			return nil, ErrMalformed
		}
		rest = rest[1:]
		if strings.TrimSpace(rest) == "" {
			return nil, ErrMalformed
		}
	}
}

// Bearer extracts an Authorization bearer credential. An empty or malformed
// value is rejected, and the credential is never accepted from a URL.
func Bearer(value string) (string, bool) {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// Plex returns X-Plex headers keyed by their canonical HTTP header names.
func Plex(h http.Header) http.Header {
	result := make(http.Header)
	for k, v := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-plex-") {
			result[http.CanonicalHeaderKey(k)] = append([]string(nil), v...)
		}
	}
	return result
}

// MediaBrowserToken returns legacy header credentials before an ApiKey
// query parameter. It does not interpret a query credential as Plex auth.
func MediaBrowserToken(r *http.Request) string {
	for _, k := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if v := r.Header.Get(k); v != "" {
			return v
		}
	}
	for _, k := range []string{"Authorization", "X-Emby-Authorization"} {
		if v := r.Header.Get(k); v != "" {
			if p, err := MediaBrowser(v); err == nil && p["Token"] != "" {
				return p["Token"]
			}
		}
	}
	var token string
	found := false
	for k, values := range r.URL.Query() {
		if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "ApiKey") {
			if found || len(values) != 1 {
				return ""
			}
			found, token = true, values[0]
		}
	}
	return token
}
