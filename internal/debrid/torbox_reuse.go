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
func (p *torBox) findKnown(hash string) string {
	p.knownMu.Lock()
	defer p.knownMu.Unlock()
	return p.known[strings.ToLower(hash)]
}

func (p *torBox) rememberKnown(hash, id string) {
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

// findFresh is only the recovery path for a documented DUPLICATE_ITEM.
// Matching by hash preserves episode/series identity and avoids duplicate
// account entries without an unconditional account-wide request on Play.
func (p *torBox) findFresh(ctx context.Context, hash string) (string, error) {
	// D: a positive fresh/in-flight background snapshot already identifies
	// this duplicate. A miss still needs the provider's fresh recovery read.
	if id, err := p.list.reuse(ctx, p.opts.Now(), hash); id != "" || err != nil {
		return id, err
	}
	data, err := p.do(ctx, apiRequest{method: http.MethodGet, path: "/torrents/mylist", query: url.Values{"bypass_cache": {"true"}}})
	if err != nil {
		return "", err
	}
	var items []tbTorrent
	if err := json.Unmarshal(data, &items); err != nil {
		return "", errors.New("torbox: unexpected torrent list")
	}
	for _, it := range items {
		if strings.EqualFold(it.Hash, hash) && it.ID.String() != "" {
			return it.ID.String(), nil
		}
	}
	return "", nil
}
