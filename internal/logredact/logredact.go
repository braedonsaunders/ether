// Package logredact removes credentials from log output. Handler wraps the
// final slog handler, so every record is scrubbed whichever call site wrote
// it: a URL with ?X-Plex-Token=…, an error that quotes a request, a header
// dump or an attribute named "password". Call-site redaction stays useful
// (it keeps the shape of a value readable); this is the backstop.
//
// It recognises credentials by name (query parameters, JSON keys, header
// names and attribute keys, compared case-insensitively ignoring '-' and
// '_' and after URL-decoding, so %58-Plex-Token is X-Plex-Token), by
// position (user:password@ in URLs, Bearer/Basic credentials) and by the
// prefixes of Ether's own credentials. Anything else passes through
// unchanged.
package logredact

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Mask replaces a removed credential.
const Mask = "[redacted]"

// sensitive holds normalised credential names (see normalise).
var sensitive = map[string]bool{
	"token": true, "xplextoken": true, "plextoken": true, "accesstoken": true, "refreshtoken": true,
	"authtoken": true, "idtoken": true, "sessiontoken": true, "xetheradmintoken": true, "etheradmintoken": true,
	"xetherrecoverytoken": true, "recoverytoken": true,
	"password": true, "passwd": true, "pwd": true, "passkey": true,
	"secret": true, "clientsecret": true, "apikey": true, "xapikey": true, "apitoken": true,
	"authorization": true, "proxyauthorization": true, "cookie": true, "setcookie": true,
	"rsskey": true,
}

// normalise lowercases name and drops everything but letters and digits.
func normalise(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Sensitive reports whether a parameter, header, JSON key or attribute key
// names a credential. Percent-encoded names are decoded first.
func Sensitive(name string) bool {
	for range 3 { // %2558 is %58 is X
		dec, err := url.QueryUnescape(name)
		if err != nil || dec == name {
			break
		}
		name = dec
	}
	return sensitive[normalise(name)]
}

var (
	// name=value after a query, form or log-text boundary. A value stops at
	// '?' so that url=https://host/?apikey=… is still seen as apikey=….
	param = regexp.MustCompile(`(^|[?&;\s,(\[{])([^=&#\s"'<>?;,(){}\[\]]+)=("[^"?]*"|[^&#\s"'<>;,)\]}?]*)`)
	// "name": "value" in JSON.
	jsonField = regexp.MustCompile(`("([^"\\]{1,64})"\s*:\s*)"((?:[^"\\]|\\.)*)"`)
	// Header lines and Go's map[Name:[value]] dumps: cookies to the end of
	// the line, token headers and Authorization (in its canonical spelling,
	// so prose such as "authorization: failed" is left alone) one value.
	cookie      = regexp.MustCompile(`(?i)\b(cookie|set-cookie)(\s*:\s*\[?|:\[)([^\]\r\n]+)`)
	tokenHeader = regexp.MustCompile(`(?i)\b(x-plex-token|x-ether-admin-token|x-ether-recovery-token|x-api-key)(\s*:\s*\[?|:\[)([^\s\],;"]+)`)
	authHeader  = regexp.MustCompile(`\b(Authorization|Proxy-Authorization)(: ?\[?|:\[)((?:[A-Za-z]+ )?[^\s\],;"]+)`)
	// scheme://user:password@host
	userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]*):([^@/\s]+)@`)
	// Bearer/Basic credentials wherever they appear.
	scheme = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{6,}`)
	// Ether's own credentials: console sessions and cast delegations.
	own = regexp.MustCompile(`\bether-(admin|cast|recovery)-[A-Za-z0-9]+`)
)

// String returns s with credentials replaced by Mask.
func String(s string) string {
	if s == "" || !strings.ContainsAny(s, "=:@") && !strings.Contains(s, "ether-") && !strings.Contains(strings.ToLower(s), "earer") && !strings.Contains(strings.ToLower(s), "basic ") {
		return s
	}
	s = maskValue(cookie, s)
	s = maskValue(tokenHeader, s)
	s = maskValue(authHeader, s)
	s = scheme.ReplaceAllString(s, "${1} "+Mask)
	s = userinfo.ReplaceAllString(s, "${1}:"+Mask+"@")
	s = own.ReplaceAllString(s, "ether-${1}-"+Mask)
	s = param.ReplaceAllStringFunc(s, func(m string) string {
		sub := param.FindStringSubmatch(m)
		if !Sensitive(sub[2]) || sub[3] == "" || masked(sub[3]) {
			return m
		}
		return sub[1] + sub[2] + "=" + Mask
	})
	s = jsonField.ReplaceAllStringFunc(s, func(m string) string {
		sub := jsonField.FindStringSubmatch(m)
		if !Sensitive(sub[2]) || sub[3] == "" {
			return m
		}
		return sub[1] + `"` + Mask + `"`
	})
	return s
}

// maskValue replaces the third group of re's matches (name, separator,
// value) with Mask.
func maskValue(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if masked(sub[3]) {
			return m
		}
		return sub[1] + sub[2] + Mask
	})
}

// masked reports whether v is already (the start of) a Mask.
func masked(v string) bool {
	v = strings.Trim(v, `"'`)
	return v == Mask || v == strings.TrimSuffix(Mask, "]") || v == strings.Trim(Mask, "[]")
}

// Handler returns a slog.Handler that scrubs every record before next sees
// it: the message, attribute values (inside groups too) and attributes
// added with WithAttrs. Attributes whose key names a credential lose their
// value entirely.
func Handler(next slog.Handler) slog.Handler { return &handler{next: next} }

type handler struct {
	next        slog.Handler
	secretGroup bool
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, String(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(as))
	for i, a := range as {
		clean[i] = h.attr(a)
	}
	return &handler{next: h.next.WithAttrs(clean), secretGroup: h.secretGroup}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{next: h.next.WithGroup(name), secretGroup: h.secretGroup || Sensitive(name)}
}

func (h *handler) attr(a slog.Attr) slog.Attr {
	if h.secretGroup && credentialValue(a.Value.Resolve()) {
		return slog.String(a.Key, Mask)
	}
	return Attr(a)
}

// Attr scrubs one attribute.
func Attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if Sensitive(a.Key) && credentialValue(v) {
		return slog.String(a.Key, Mask)
	}
	if v.Kind() == slog.KindGroup {
		as := v.Group()
		clean := make([]slog.Attr, len(as))
		for i, g := range as {
			clean[i] = Attr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	}
	if Sensitive(a.Key) && credentialValue(v) {
		return slog.String(a.Key, Mask)
	}
	switch v.Kind() {
	case slog.KindString:
		if s := String(v.String()); s != v.String() {
			return slog.String(a.Key, s)
		}
	case slog.KindAny:
		var text string
		switch x := v.Any().(type) {
		case error:
			text = x.Error()
		case *url.URL:
			text = x.String()
		case url.URL:
			text = x.String()
		case fmt.Stringer:
			text = x.String()
		case []byte:
			text = string(x)
		default:
			// Preserve structured fields, including full-width integers, while
			// recursively scrubbing credential keys and string values. Cyclic or
			// otherwise non-JSON values cannot be safely inspected: omit them.
			b, err := json.Marshal(v.Any())
			if err != nil {
				return slog.String(a.Key, Mask)
			}
			var decoded any
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.UseNumber()
			if err := dec.Decode(&decoded); err != nil {
				return slog.String(a.Key, Mask)
			}
			if clean, changed := structured(decoded); changed {
				return slog.Any(a.Key, clean)
			}
			return slog.Attr{Key: a.Key, Value: v}
		}
		if s := String(text); s != text {
			return slog.String(a.Key, s)
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// credentialValue reports whether a value under a credential's name could
// hold the credential: text, not a count or a flag ("token=false").
func credentialValue(v slog.Value) bool {
	switch v.Kind() {
	case slog.KindBool, slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindDuration, slog.KindTime:
		return false
	case slog.KindString:
		return v.String() != ""
	}
	return true
}

func structured(value any) (any, bool) {
	changed := false
	switch x := value.(type) {
	case map[string]any:
		for key, item := range x {
			if Sensitive(key) {
				switch v := item.(type) {
				case nil, bool, json.Number:
				case string:
					if v != "" && v != Mask {
						x[key] = Mask
						changed = true
					}
				default:
					x[key] = Mask
					changed = true
				}
				continue
			}
			clean, c := structured(item)
			x[key] = clean
			changed = changed || c
		}
	case []any:
		for i, item := range x {
			clean, c := structured(item)
			x[i] = clean
			changed = changed || c
		}
	case string:
		clean := String(x)
		return clean, clean != x
	}
	return value, changed
}
