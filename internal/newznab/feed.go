package newznab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// FeedSources reports capabilities without making a request. Built-in sites
// remain searchable but are not treated as paginated feeds without that contract.
func (m *Manager) FeedSources() []core.ReleaseFeed {
	out := []core.ReleaseFeed{}
	for _, c := range m.Clients() {
		cfg := c.Config()
		if !cfg.Enabled {
			continue
		}
		raw, _ := json.Marshal(cfg)
		sum := sha256.Sum256(raw)
		c.capsMu.Lock()
		page := 0
		if c.caps != nil {
			page = c.caps.LimitMax
		}
		c.capsMu.Unlock()
		f := core.ReleaseFeed{ID: c.indexerID(), Name: c.Name(), Generation: hex.EncodeToString(sum[:]), Protocol: cfg.Protocol, Categories: recentCategories(cfg), Supported: !cfg.UsesBackend(), DailyLimit: cfg.DailyLimit, PageMax: page}
		if !f.Supported {
			f.Reason = "This site does not expose a paginated recent-release feed; on-demand search remains available."
		}
		out = append(out, f)
	}
	return out
}

// Recent fetches one page, charging the same account-wide limits as Search.
// It never invokes title searches, capability discovery, or implicit retries.
func (m *Manager) Recent(ctx context.Context, id string, offset, limit int) ([]core.Release, error) {
	for _, c := range m.Clients() {
		if c.indexerID() != id {
			continue
		}
		cfg := c.Config()
		if !cfg.Enabled || cfg.UsesBackend() {
			return nil, fmt.Errorf("recent-release feed is not enabled")
		}
		return c.recent(ctx, offset, limit)
	}
	return nil, fmt.Errorf("release-feed source no longer configured")
}

// CachedReleases reuses a bounded set of already-paid-for search/feed results.
// It performs no requests, capability discovery, cache refresh, or downloads.
func (m *Manager) CachedReleases(ctx context.Context) ([]core.Release, error) {
	if m.db == nil {
		return nil, nil
	}
	clients := map[string]*Client{}
	for _, c := range m.Clients() {
		if c.Config().Enabled {
			clients[c.indexerID()] = c
		}
	}
	rows, err := m.db.QueryContext(ctx, `SELECT indexer_id,results FROM newznab_cache WHERE expires_at>? ORDER BY expires_at DESC LIMIT 100`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Release
	seen := map[string]bool{}
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		c := clients[id]
		if c == nil {
			continue
		}
		// Bulk reuse must establish the same configuration binding as cacheKey.
		// Older cache rows remain usable by exact Search, but cannot be bulk adopted.
		var doc cachedResults
		if json.Unmarshal([]byte(raw), &doc) != nil || doc.Identity != searchIdentity(c.Config()) {
			continue
		}
		rs, ok := c.decodeCached(raw)
		if !ok {
			continue
		}
		for _, r := range rs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
				if len(out) >= 5000 {
					return out, nil
				}
			}
		}
	}
	return out, rows.Err()
}
