package newznab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Cached search results must not keep the indexer's API key (or a
// built-in site's secret settings, such as a passkey), which download and
// magnet links embed. cachePut replaces every occurrence in the link
// fields with a token naming the secret; cacheGet puts the current value
// back. The cache key includes the secrets (searchIdentity), so a hit is
// always hydrated with the values the result was fetched with, and
// FetchNZB authenticates exactly as it would with a fresh result.
//
// Rows are stored as {"format":2,"releases":[...]}. Releases before this
// format read a row as a bare array; they treat the object as a cache miss
// instead of fetching a tokenized link. Legacy array rows (which contain
// keys) are still served until PurgeLegacyCache removes them.

const cacheFormat = 2

type cachedResults struct {
	Identity string         `json:"identity,omitempty"`
	Format   int            `json:"format"`
	Releases []core.Release `json:"releases"`
}

// minAddonSecret is the shortest addon path segment treated as a secret:
// shorter ones ("stream", "resolve") are route words, not settings.
const minAddonSecret = 12

// cacheSecret is one stored form of a secret and the token replacing it.
// Tokens are delimited by NUL, which never occurs in a link (results
// containing NUL are not cached).
type cacheSecret struct{ token, text string }

// cacheSecrets lists the forms of this indexer's secrets a link may carry,
// longest first.
func (c *Client) cacheSecrets() []cacheSecret {
	cfg := c.Config()
	var out []cacheSecret
	seen := map[string]bool{}
	add := func(name, v string) {
		if v == "" || strings.ContainsRune(v, 0) {
			return
		}
		for _, f := range []struct{ form, text string }{{"raw", v}, {"query", url.QueryEscape(v)}, {"path", url.PathEscape(v)}} {
			if seen[f.text] {
				continue
			}
			seen[f.text] = true
			out = append(out, cacheSecret{token: "\x00" + name + "|" + f.form + "\x00", text: f.text})
		}
	}
	add("apikey", cfg.APIKey)
	// A Stremio addon's settings, often including its debrid key, live in
	// its install path, and its stream and NZB links repeat them.
	if cfg.IsStremio() {
		if u, err := url.Parse(strings.TrimSpace(cfg.URL)); err == nil {
			n := 0
			for _, seg := range strings.Split(u.EscapedPath(), "/") {
				raw, err := url.PathUnescape(seg)
				if err != nil || raw == "manifest.json" {
					continue
				}
				// "providers=x|realdebrid=KEY": the segment and each value.
				parts := append([]string{raw}, strings.FieldsFunc(raw, func(r rune) bool { return strings.ContainsRune("|=,;&", r) })...)
				for _, part := range parts {
					if len(part) >= minAddonSecret {
						add(fmt.Sprintf("addon:%d", n), part)
						n++
					}
				}
			}
		}
	}
	keys := make([]string, 0, len(cfg.SecretSettings))
	for k := range cfg.SecretSettings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("secret:"+k, cfg.SecretSettings[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].text) > len(out[j].text) })
	return out
}

// redactLink replaces the secrets in v with their tokens; ok is false
// when v cannot be tokenized (it contains NUL).
func redactLink(v string, secrets []cacheSecret) (string, bool) {
	if strings.ContainsRune(v, 0) {
		return "", false
	}
	if v == "" || len(secrets) == 0 {
		return v, true
	}
	var b strings.Builder
	for i := 0; i < len(v); {
		matched := false
		for _, s := range secrets {
			if strings.HasPrefix(v[i:], s.text) {
				b.WriteString(s.token)
				i += len(s.text)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(v[i])
			i++
		}
	}
	return b.String(), true
}

// hydrateLink reverses redactLink; ok is false for an unknown token.
func hydrateLink(v string, secrets []cacheSecret) (string, bool) {
	if !strings.ContainsRune(v, 0) {
		return v, true
	}
	parts := strings.Split(v, "\x00")
	if len(parts)%2 == 0 {
		return "", false
	}
	for i := 1; i < len(parts); i += 2 {
		token, found := "\x00"+parts[i]+"\x00", false
		for _, s := range secrets {
			if s.token == token {
				parts[i], found = s.text, true
				break
			}
		}
		if !found {
			return "", false
		}
	}
	return strings.Join(parts, ""), true
}

func releaseLinks(r *core.Release) []*string {
	return []*string{&r.NZBURL, &r.GUID, &r.MagnetURL, &r.StreamURL}
}

// mapHeaders applies fn to a copy of r's stream header values; ok is false
// when fn fails for one.
func mapHeaders(r *core.Release, fn func(string) (string, bool)) bool {
	if len(r.StreamHeaders) == 0 {
		return true
	}
	out := make(map[string]string, len(r.StreamHeaders))
	for k, v := range r.StreamHeaders {
		w, ok := fn(v)
		if !ok {
			return false
		}
		out[k] = w
	}
	r.StreamHeaders = out
	return true
}

// encodeCached is the stored form of results; ok is false when they
// cannot be stored without a secret.
func (c *Client) encodeCached(results []core.Release) (string, bool) {
	secrets := c.cacheSecrets()
	out := make([]core.Release, len(results))
	for i, r := range results {
		for _, p := range releaseLinks(&r) {
			v, ok := redactLink(*p, secrets)
			if !ok {
				return "", false
			}
			*p = v
		}
		if !mapHeaders(&r, func(v string) (string, bool) { return redactLink(v, secrets) }) {
			return "", false
		}
		out[i] = r
	}
	raw, err := json.Marshal(cachedResults{Format: cacheFormat, Releases: out, Identity: searchIdentity(c.Config())})
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// decodeCached reads a stored row (current or legacy format).
func (c *Client) decodeCached(raw string) ([]core.Release, bool) {
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		var legacy []core.Release
		if json.Unmarshal([]byte(raw), &legacy) != nil {
			return nil, false
		}
		return legacy, true
	}
	var doc cachedResults
	if json.Unmarshal([]byte(raw), &doc) != nil || doc.Format != cacheFormat {
		return nil, false
	}
	secrets := c.cacheSecrets()
	for i := range doc.Releases {
		for _, p := range releaseLinks(&doc.Releases[i]) {
			v, ok := hydrateLink(*p, secrets)
			if !ok {
				return nil, false
			}
			*p = v
		}
		if !mapHeaders(&doc.Releases[i], func(v string) (string, bool) { return hydrateLink(v, secrets) }) {
			return nil, false
		}
	}
	return doc.Releases, true
}

// PurgeLegacyCache deletes search-cache rows written before cached links
// were tokenized (they contain indexer API keys) and returns how many it
// removed. Call it once no node running an older release writes the cache.
func (m *Manager) PurgeLegacyCache(ctx context.Context) (int64, error) {
	if m.db == nil {
		return 0, nil
	}
	res, err := m.db.ExecContext(ctx, `DELETE FROM newznab_cache WHERE results NOT LIKE '{%'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
