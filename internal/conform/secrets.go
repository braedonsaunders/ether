package conform

import (
	"bytes"
	"encoding/json"
	"strings"
)

// RedactSecrets removes known run-specific credentials and identifiers from a
// response before it can reach disk or reports, without altering JSON keys.
func RedactSecrets(r Response, secrets ...string) Response {
	r.Headers = r.Headers.Clone()
	replace := func(s string) string {
		for _, secret := range secrets {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[REDACTED]")
			}
		}
		return s
	}
	for k, values := range r.Headers {
		for i, v := range values {
			values[i] = replace(v)
		}
		r.Headers[k] = values
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		var walk func(any) any
		walk = func(v any) any {
			switch x := v.(type) {
			case string:
				return replace(x)
			case map[string]any:
				for k, val := range x {
					x[k] = walk(val)
				}
			case []any:
				for i, val := range x {
					x[i] = walk(val)
				}
			}
			return v
		}
		if b, err := json.Marshal(walk(v)); err == nil {
			r.Body = b
		}
		return r
	}
	r.Body = []byte(replace(string(r.Body)))
	return r
}
