package diagnostics

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/logredact"
)

// secretForms lists literal encodings of a nonempty secret. Percent-encoded
// forms are not enumerated: redactPercentEncoded decodes bounded runs, so
// mixed hex case and fully encoded tokens are covered without an exponential
// case list. There is no minimum length.
func secretForms(secret string) []string {
	if secret == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var forms []string
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		forms = append(forms, s)
	}
	add(secret)
	add(url.QueryEscape(secret))
	add(url.PathEscape(secret))
	add(url.QueryEscape(url.QueryEscape(secret)))
	raw := []byte(secret)
	add(base64.StdEncoding.EncodeToString(raw))
	add(base64.RawStdEncoding.EncodeToString(raw))
	add(base64.URLEncoding.EncodeToString(raw))
	add(base64.RawURLEncoding.EncodeToString(raw))
	quoted := strconv.Quote(secret)
	if len(quoted) >= 2 {
		add(quoted[1 : len(quoted)-1])
	}
	return forms
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func unhex(b byte) byte {
	switch {
	case b >= '0' && b <= '9':
		return b - '0'
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10
	default:
		return 0
	}
}

type secretUse struct {
	raw    string
	forms  []string
	single bool
}

func prepareSecrets(values []string) (uses []secretUse, freeTextUnsafe bool) {
	seen := map[string]struct{}{}
	for _, raw := range values {
		if raw == "" {
			continue
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		if len(raw) == 1 {
			freeTextUnsafe = true
		}
		uses = append(uses, secretUse{raw: raw, forms: secretForms(raw), single: len(raw) == 1})
	}
	return uses, freeTextUnsafe
}

// redactString removes known secrets from s. Secrets of length >= 4 are
// removed wherever they occur. Shorter secrets are removed when they stand
// alone as a token, and any longer encoding of them is removed wherever it
// occurs. logredact still runs first.
func redactString(s string, uses []secretUse) string {
	if s == "" {
		return s
	}
	s = logredact.String(s)
	type form struct {
		text  string
		whole bool
	}
	var forms []form
	for _, u := range uses {
		for _, f := range u.forms {
			whole := len(u.raw) < 4 && len(f) < 4
			forms = append(forms, form{text: f, whole: whole})
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i].text) > len(forms[j].text) })
	for _, f := range forms {
		if f.text == "" {
			continue
		}
		if f.whole {
			s = replaceBounded(s, f.text, redactMask)
			continue
		}
		s = strings.ReplaceAll(s, f.text, redactMask)
	}
	return redactPercentEncoded(s, uses)
}

// redactPercentEncoded replaces a bounded run of %HH sequences when a bounded
// number of decodes reveals a known secret. Hex case is canonicalized by
// decoding (%cA and %Ca are the same byte). Fully encoded and double-encoded
// tokens are handled here; alphanumeric secrets are not left untouched just
// because QueryEscape keeps them as-is. Replacement is on the Go string, so a
// JSON document that contains the token remains valid JSON.
func redactPercentEncoded(s string, uses []secretUse) string {
	if len(uses) == 0 || !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if !percentByte(s, i) {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j-i < maxPercentRun && percentByte(s, j) {
			j += 3
		}
		span := s[i:j]
		if percentRunHoldsSecret(span, uses) {
			b.WriteString(redactMask)
		} else {
			b.WriteString(span)
		}
		i = j
	}
	return b.String()
}

const (
	maxPercentRun    = 4096
	maxPercentPasses = 3
)

func percentByte(s string, i int) bool {
	return i+2 < len(s) && s[i] == '%' && isHex(s[i+1]) && isHex(s[i+2])
}

func percentRunHoldsSecret(span string, uses []secretUse) bool {
	cur := span
	for pass := 0; pass < maxPercentPasses; pass++ {
		decoded, ok := decodePercent(cur)
		if !ok {
			return false
		}
		if secretIn(decoded, uses) {
			return true
		}
		cur = decoded
	}
	return false
}

func decodePercent(s string) (string, bool) {
	if len(s) > maxPercentRun || !strings.Contains(s, "%") {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s) / 2)
	changed := false
	for i := 0; i < len(s); {
		if percentByte(s, i) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 3
			changed = true
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

func secretIn(decoded string, uses []secretUse) bool {
	for _, u := range uses {
		if u.raw == "" {
			continue
		}
		if len(u.raw) >= 2 {
			if strings.Contains(decoded, u.raw) {
				return true
			}
			continue
		}
		if decoded == u.raw || delimitedContains(decoded, u.raw) {
			return true
		}
	}
	return false
}

// replaceBounded replaces needle only when it is not in the middle of an
// ASCII letter or digit. The needle itself may contain punctuation.
func replaceBounded(s, needle, mask string) string {
	if needle == "" || !strings.Contains(s, needle) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], needle)
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		j += i
		end := j + len(needle)
		leftOK := j == 0 || !isAlnum(s[j-1])
		rightOK := end >= len(s) || !isAlnum(s[end])
		if leftOK && rightOK {
			b.WriteString(s[i:j])
			b.WriteString(mask)
			i = end
			continue
		}
		// Advance one byte so overlapping needles are still found.
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// leaks reports whether b still contains a secret in the form the redactor
// promised to remove. It does not modify b. JSON text is also decoded, so a
// secret hidden by encoding/json (\u003c and friends) still counts.
func leaks(b []byte, uses []secretUse) bool {
	if len(uses) == 0 || len(b) == 0 {
		return false
	}
	if len(b) > maxScanBytes {
		b = b[:maxScanBytes]
	}
	text := string(b)
	if literalLeak(text, uses) || encodedLeak(text, uses) {
		return true
	}
	return jsonValueLeak(b, uses)
}

// jsonValueLeak decodes a JSON document or JSONL body and checks the
// recovered strings. Escapes are not a way to keep a configured secret.
func jsonValueLeak(b []byte, uses []secretUse) bool {
	if json.Valid(b) && walkJSONLeak(b, uses) {
		return true
	}
	if !bytes.Contains(b, []byte("\n")) {
		return false
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		if walkJSONLeak(line, uses) {
			return true
		}
	}
	return false
}

func walkJSONLeak(b []byte, uses []secretUse) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	return valueLeak(v, uses)
}

func valueLeak(v any, uses []secretUse) bool {
	switch x := v.(type) {
	case string:
		return literalLeak(x, uses) || encodedLeak(x, uses)
	case []any:
		for _, e := range x {
			if valueLeak(e, uses) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if literalLeak(k, uses) || encodedLeak(k, uses) || valueLeak(e, uses) {
				return true
			}
		}
	}
	return false
}

// encodedLeak walks percent-encoded runs. It does not decode the whole
// buffer as one span, so a long URL does not hide a later token and does
// not force the collector to be omitted.
func encodedLeak(text string, uses []secretUse) bool {
	i := 0
	for i < len(text) {
		if !percentByte(text, i) {
			i++
			continue
		}
		j := i
		for j-i < maxPercentRun && percentByte(text, j) {
			j += 3
		}
		if percentRunHoldsSecret(text[i:j], uses) {
			return true
		}
		i = j
	}
	return false
}

const maxScanBytes = 2 << 20

func literalLeak(text string, uses []secretUse) bool {
	for _, u := range uses {
		for _, f := range u.forms {
			if f == "" {
				continue
			}
			if len(u.raw) >= 4 || len(f) >= 4 {
				if strings.Contains(text, f) {
					return true
				}
				continue
			}
			if delimitedContains(text, f) {
				return true
			}
		}
	}
	return false
}

func delimitedContains(s, needle string) bool {
	if needle == "" {
		return false
	}
	start := 0
	for {
		j := strings.Index(s[start:], needle)
		if j < 0 {
			return false
		}
		j += start
		end := j + len(needle)
		leftOK := j == 0 || !isAlnum(s[j-1])
		rightOK := end >= len(s) || !isAlnum(s[end])
		if leftOK && rightOK {
			return true
		}
		start = j + 1
		if start >= len(s) {
			return false
		}
	}
}

// maxFieldScan is the most of an untrusted field clipFree will read or copy.
const maxFieldScan = 16 << 10

// clipFree redacts within a bounded copy, then shortens. A secret that
// crosses the cutoff is removed instead of leaving its prefix. The original
// string's tail is not scanned and is not retained.
func clipFree(s string, max int, uses []secretUse) string {
	if s == "" || max <= 0 {
		return ""
	}
	cut := len(s) > max
	n := len(s)
	if n > maxFieldScan {
		n = maxFieldScan
		cut = true
	}
	buf := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		c := s[i]
		if c == '\n' || c == '\t' || c >= 0x20 {
			buf = append(buf, c)
		}
	}
	redacted := redactString(string(buf), uses)
	if cut {
		redacted = dropCutSecrets(redacted, uses)
	}
	if len(redacted) > max {
		redacted = trimUTF8(redacted, max)
		redacted = dropCutSecrets(redacted, uses)
	}
	return redacted
}

func trimUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// dropCutSecrets removes a prefix of a known secret left by a cutoff.
// The full secret is longer than what remains, so full-value matching would
// otherwise miss it.
func dropCutSecrets(s string, uses []secretUse) string {
	for _, u := range uses {
		sec := u.raw
		if len(sec) < 8 || s == "" || strings.Contains(s, sec) {
			continue
		}
		n := len(sec) - 1
		if n > len(s) {
			n = len(s)
		}
		for ; n >= 8; n-- {
			p := sec[:n]
			if i := strings.Index(s, p); i >= 0 {
				s = s[:i] + redactMask + s[i+len(p):]
				break
			}
		}
	}
	return s
}

func stripQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

func boundNote(s string, uses []secretUse) string {
	s = clipFree(s, maxNoteRunes*4, uses)
	r := []rune(s)
	if len(r) <= maxNoteRunes {
		return s
	}
	s = string(r[:maxNoteRunes])
	return dropCutSecrets(s, uses)
}

func boundCategory(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "playback", "picture", "subtitles", "library", "other":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "other"
	}
}
