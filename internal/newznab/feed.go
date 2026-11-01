package newznab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// FeedSources reports capabilities without making a request. Built-in sites
// remain searchable but are not treated as paginated feeds without that contract.
// An indexer whose resolved search identity is a persona exposes one feed per
// media side, because a persona poll is a tvsearch or movie query, not a
// mixed text search.
func (m *Manager) FeedSources() []core.ReleaseFeed {
	out := []core.ReleaseFeed{}
	for _, c := range m.Clients() {
		cfg := c.Config()
		if !cfg.Enabled {
			continue
		}
		supported := !cfg.UsesBackend()
		reason := ""
		if !supported {
			reason = "This site does not expose a paginated recent-release feed; on-demand search remains available."
		}
		if c.splitFeed() {
			for _, side := range []struct {
				suffix string
				media  identityMedia
				cats   []int
			}{
				{"tv", mediaTV, feedCategoriesFor(cfg, mediaTV)},
				{"movie", mediaMovie, feedCategoriesFor(cfg, mediaMovie)},
			} {
				out = append(out, core.ReleaseFeed{
					ID: c.indexerID() + "#" + side.suffix, Name: c.Name(),
					Generation: c.feedGeneration(side.media), Protocol: cfg.Protocol,
					Categories: side.cats, Supported: supported, Reason: reason,
				})
			}
			continue
		}
		out = append(out, core.ReleaseFeed{
			ID: c.indexerID(), Name: c.Name(), Generation: c.feedGeneration(mediaTV),
			Protocol: cfg.Protocol, Categories: recentCategories(cfg), Supported: supported, Reason: reason,
		})
	}
	return out
}

// feedCategoriesFor is the side's own category list with Newznab defaults.
func feedCategoriesFor(cfg config.Indexer, media identityMedia) []int {
	cats := cfg.MovieCategories
	if media == mediaTV {
		cats = cfg.TVCategories
		if len(cats) == 0 {
			return []int{5000}
		}
		return append([]int{}, cats...)
	}
	if len(cats) == 0 {
		return []int{2000}
	}
	return append([]int{}, cats...)
}

// splitFeed reports whether polled feeds must be per media side: when either
// resolved search identity is a persona.
func (c *Client) splitFeed() bool {
	tv, _ := c.identityFor(purposeSearch, mediaTV)
	movie, _ := c.identityFor(purposeSearch, mediaMovie)
	return tv.profile != profileCurrent || movie.profile != profileCurrent
}

// feedGeneration fingerprints the indexer and the identity that polls it, so
// an identity change restarts the media-side cursor instead of resuming it
// under another client's query shape.
func (c *Client) feedGeneration(media identityMedia) string {
	ident, _ := c.identityFor(purposeSearch, media)
	raw, _ := json.Marshal(struct {
		Indexer config.Indexer
		Name    string
		UA      string
		Headers map[string]string
	}{c.Config(), ident.name, ident.userAgent, ident.headers})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// splitFeedID separates a media-side suffix from an indexer id.
func splitFeedID(id string) (base, side string) {
	if base, side, ok := strings.Cut(id, "#"); ok {
		return base, side
	}
	return id, ""
}

// Recent fetches one page, charging the same account-wide limits as Search.
// It never invokes title searches, capability discovery, or implicit retries.
func (m *Manager) Recent(ctx context.Context, id string, offset, limit int) ([]core.Release, error) {
	base, side := splitFeedID(id)
	for _, c := range m.Clients() {
		if c.indexerID() != base {
			continue
		}
		cfg := c.Config()
		if !cfg.Enabled || cfg.UsesBackend() {
			return nil, fmt.Errorf("recent-release feed is not enabled")
		}
		switch side {
		case "tv":
			if !c.splitFeed() {
				return nil, fmt.Errorf("release-feed source no longer configured")
			}
			return c.recent(ctx, mediaTV, offset, limit)
		case "movie":
			if !c.splitFeed() {
				return nil, fmt.Errorf("release-feed source no longer configured")
			}
			return c.recent(ctx, mediaMovie, offset, limit)
		case "":
			// A split indexer's mixed feed no longer exists; its old checkpoint
			// must not merge two independent offsets.
			if c.splitFeed() {
				return nil, fmt.Errorf("release-feed source no longer configured")
			}
			return c.recent(ctx, mediaUnknown, offset, limit)
		default:
			return nil, fmt.Errorf("release-feed source no longer configured")
		}
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
