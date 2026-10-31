package debrid

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Positive hints are validated by the normal per-id readiness request.
// Separate from the short-lived account snapshot; normal per-id reads validate hints.
// Mirrors torbox_reuse.go so a cold foreground Play can go straight to
// the atomic cached-only add (src=hash is only added when cached)
// instead of listing the whole account first.
func (p *debridLink) findKnown(hash string) string {
	p.knownMu.Lock()
	defer p.knownMu.Unlock()
	return p.known[strings.ToLower(hash)]
}

func (p *debridLink) rememberKnown(hash, id string) {
	p.knownMu.Lock()
	defer p.knownMu.Unlock()
	if p.known == nil {
		p.known = make(map[string]string)
	}
	hash = strings.ToLower(hash)
	if _, ok := p.known[hash]; !ok && len(p.known) >= 4096 {
		for k := range p.known {
			delete(p.known, k)
			break
		}
	}
	p.known[hash] = id
}

func (p *debridLink) forgetKnown(id string) {
	p.knownMu.Lock()
	defer p.knownMu.Unlock()
	for hash, v := range p.known {
		if v == id {
			delete(p.known, hash)
		}
	}
}

// findFresh is only the recovery path for a duplicate add. Matching by
// hash preserves episode/series identity and avoids duplicate account
// entries without an unconditional account-wide request on Play.
func (p *debridLink) findFresh(ctx context.Context, hash string) (string, error) {
	// D: a positive fresh/in-flight background snapshot already identifies
	// this duplicate. A miss still needs the provider's fresh recovery read.
	if id, err := p.list.reuse(ctx, p.opts.Now(), hash); id != "" || err != nil {
		return id, err
	}
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/seedbox/list", query: url.Values{"perPage": {"100"}}})
	if err != nil {
		return "", err
	}
	var items []dlTorrent
	if err := json.Unmarshal(data, &items); err != nil {
		return "", errors.New("debrid-link: unexpected torrent list")
	}
	for _, it := range items {
		if strings.EqualFold(it.HashString, hash) && it.ID != "" {
			return it.ID, nil
		}
	}
	return "", nil
}
