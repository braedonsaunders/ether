package newznab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/retryafter"
)

// ProwlarrDiscovery is the result of a successful Prowlarr discovery: the
// importable indexers plus what was left out and why, so a preview can
// report skips instead of dropping them silently (same pattern as the
// quality-profile import's Skipped/Warnings).
type ProwlarrDiscovery struct {
	Indexers               []config.Indexer `json:"indexers"`
	SkippedUnknownProtocol int              `json:"skippedUnknownProtocol"`
	Warnings               []string         `json:"warnings,omitempty"`
	// PresentIDs are the scoped IDs of every enabled indexer the source
	// reported, including ones filtered out below (unknown protocol or,
	// when tags are set, a tag the owner did not choose). It is diagnostic
	// only. Tag sync decides what to stop using from Indexers, never from
	// this list, and an empty tag list stays additive.
	PresentIDs []string `json:"presentIds,omitempty"`
}

// ProwlarrTag is one tag from Prowlarr's /api/v1/tag.
type ProwlarrTag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// prowlarrScope fingerprints a Prowlarr base URL; indexer IDs are
// "prowlarr-<scope>-<id>", so the scope ties an imported indexer to the
// instance (and base path) it came from.
func prowlarrScope(base string) string {
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:6])
}

// normalizeProwlarrBase preserves the URL spelling used by existing imported
// IDs. Canonicalizing host case, ports or escaped paths here would change the
// persisted identity and make additive imports create duplicate indexers.
func normalizeProwlarrBase(raw string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("prowlarr: URL must be an HTTP(S) base URL")
	}
	return base, nil
}

// ProwlarrIDPrefix returns the ID prefix of indexers imported from cfg's
// Prowlarr instance ("prowlarr-<scope>-"), or "" when the URL is not a
// usable base URL (same validation as discovery).
func ProwlarrIDPrefix(cfg config.Prowlarr) string {
	base, err := normalizeProwlarrBase(cfg.URL)
	if err != nil {
		return ""
	}
	return "prowlarr-" + prowlarrScope(base.String()) + "-"
}

// MaxRateLimitPause is the shared cap on a service-requested pause,
// exported so HTTP handlers cap Retry-After exactly like this client
// instead of duplicating a literal bound.
const MaxRateLimitPause = maxRateLimitPause

// validatedRetryAfter canonicalises a Retry-After header for propagation:
// only values retryafter.Parse accepts (delay-seconds or a future
// HTTP-date) survive, capped like the Newznab client's pause; empty,
// malformed or past values become "" so callers never forward them.
func validatedRetryAfter(v string) string {
	d, ok := retryafter.Parse(v, time.Now(), maxRateLimitPause)
	if !ok {
		return ""
	}
	return strconv.FormatInt(int64(d/time.Second), 10)
}

type prowlarrIndexer struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	Enable       bool            `json:"enable"`
	Protocol     json.RawMessage `json:"protocol"`
	Priority     int             `json:"priority"`
	Tags         []int           `json:"tags"`
	Capabilities struct {
		Categories []struct {
			ID int `json:"id"`
		} `json:"categories"`
	} `json:"capabilities"`
}

// DiscoverProwlarr imports enabled Usenet (Newznab) and torrent (Torznab)
// indexers; torrent indexers get Protocol "torrent" and stream through a
// debrid provider. Each receives a stable
// source-scoped ID and its own Ether hit/grab budget, cache and reputation.
// Prowlarr's /{id}/api alias is intentionally used so ordinary Newznab URL
// construction and all existing budget enforcement apply unchanged.
func DiscoverProwlarr(ctx context.Context, cfg config.Prowlarr, opts Options) ([]config.Indexer, error) {
	report, err := DiscoverProwlarrReport(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	return report.Indexers, nil
}

// DiscoverProwlarrReport is DiscoverProwlarr plus a count (and warning) for
// enabled indexers skipped because Prowlarr reported a protocol Ether
// cannot use.
//
// It only reads Prowlarr's indexer list (GET /api/v1/indexer). It does not
// search or download, so indexer hit and grab quotas are not spent.
// cfg.Tags empty keeps every enabled Usenet and torrent indexer. With tags
// set, an indexer is kept only when its tags field shares one of those ids.
func DiscoverProwlarrReport(ctx context.Context, cfg config.Prowlarr, opts Options) (ProwlarrDiscovery, error) {
	base, err := normalizeProwlarrBase(cfg.URL)
	if err != nil {
		return ProwlarrDiscovery{}, err
	}
	if cfg.APIKey == "" {
		return ProwlarrDiscovery{}, fmt.Errorf("prowlarr: API key is required")
	}
	cfg.Tags = config.NormalizeProwlarrTags(cfg.Tags)
	data, err := prowlarrGET(ctx, base.String()+"/api/v1/indexer", cfg.APIKey, "discovery", opts)
	if err != nil {
		return ProwlarrDiscovery{}, err
	}
	var items []prowlarrIndexer
	if err = json.Unmarshal(data, &items); err != nil {
		return ProwlarrDiscovery{}, fmt.Errorf("prowlarr: invalid indexer response: %w", err)
	}
	scope := prowlarrScope(base.String())
	var out []config.Indexer
	var present []string
	skippedUnknown := 0
	seen := map[int]bool{}
	for _, item := range items {
		// Prowlarr's public API serializes protocol as "usenet" or "torrent"
		// (older exports can contain its enum values 1 and 2).
		protocol := strings.ToLower(strings.Trim(string(item.Protocol), "\""))
		torrent := protocol == "torrent" || protocol == "2"
		if !item.Enable || item.ID <= 0 || seen[item.ID] {
			continue
		}
		// Mark seen before any filtering so a duplicated source entry —
		// importable or not — is counted/reported exactly once.
		seen[item.ID] = true
		id := "prowlarr-" + scope + "-" + strconv.Itoa(item.ID)
		// Present at the source (enabled), even if unimportable below.
		// Diagnostic only — an empty tag list stays additive, and tag sync
		// uses Indexers rather than this list.
		present = append(present, id)
		if !indexerHasTag(item.Tags, cfg.Tags) {
			continue
		}
		if protocol != "usenet" && protocol != "1" && !torrent {
			// Counted and reported, not silently dropped: the owner should
			// learn that an enabled indexer cannot be imported.
			skippedUnknown++
			continue
		}
		c := config.Indexer{ID: id, Name: "Prowlarr / " + item.Name + " [" + scope + ":" + strconv.Itoa(item.ID) + "]", URL: base.String() + "/" + strconv.Itoa(item.ID), APIKey: cfg.APIKey, DailyLimit: cfg.DailyLimit, DailyGrabs: cfg.DailyGrabs, Priority: cfg.Priority + item.Priority, Enabled: true}
		if torrent {
			c.Protocol = "torrent"
		}
		for _, cat := range item.Capabilities.Categories {
			if cat.ID >= 2000 && cat.ID < 3000 {
				c.MovieCategories = append(c.MovieCategories, cat.ID)
			}
			if cat.ID >= 5000 && cat.ID < 6000 {
				c.TVCategories = append(c.TVCategories, cat.ID)
			}
		}
		out = append(out, c)
	}
	report := ProwlarrDiscovery{Indexers: out, SkippedUnknownProtocol: skippedUnknown, PresentIDs: present}
	if skippedUnknown > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("skipped %d Prowlarr indexer(s) with an unsupported protocol (only usenet/torrent can be imported)", skippedUnknown))
		if opts.Logger != nil {
			opts.Logger.Warn("prowlarr discovery skipped indexers with unsupported protocol", "skipped", skippedUnknown)
		}
	}
	return report, nil
}

// indexerHasTag reports whether an indexer's tag ids include one of want.
// An empty want list matches every indexer (no tag filter).
func indexerHasTag(have, want []int) bool {
	if len(want) == 0 {
		return true
	}
	for _, id := range have {
		for _, w := range want {
			if id == w {
				return true
			}
		}
	}
	return false
}

// ListProwlarrTags reads Prowlarr's tag catalogue (GET /api/v1/tag) so the
// owner can pick tags. It does not search or download.
func ListProwlarrTags(ctx context.Context, cfg config.Prowlarr, opts Options) ([]ProwlarrTag, error) {
	base, err := normalizeProwlarrBase(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("prowlarr: API key is required")
	}
	data, err := prowlarrGET(ctx, base.String()+"/api/v1/tag", cfg.APIKey, "tag list", opts)
	if err != nil {
		return nil, err
	}
	var items []ProwlarrTag
	if err = json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("prowlarr: invalid tag response: %w", err)
	}
	seen := map[int]bool{}
	out := make([]ProwlarrTag, 0, len(items))
	for _, item := range items {
		item.Label = strings.TrimSpace(item.Label)
		if item.ID <= 0 || item.Label == "" || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		out = append(out, item)
	}
	slices.SortFunc(out, func(a, b ProwlarrTag) int {
		if c := strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)); c != 0 {
			return c
		}
		return a.ID - b.ID
	})
	return out, nil
}

// prowlarrGET reads one Prowlarr management URL. what names the call in
// errors ("discovery", "tag list"). A transport failure stays generic so
// the URL is not echoed. A 429 is a *RateLimitError with a validated pause.
func prowlarrGET(ctx context.Context, rawURL, apiKey, what string, opts Options) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	cl := opts.HTTPClient
	if cl == nil {
		cl = egress.Client(egress.CapIndexers, defaultHTTPTimeout)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prowlarr: %s failed", what)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		// Propagate Retry-After exactly like the Newznab client does for a
		// 429 (see Client.get/recordRateLimit): callers can inspect the
		// *RateLimitError instead of guessing a pause.
		return nil, &RateLimitError{RetryAfter: validatedRetryAfter(resp.Header.Get("Retry-After")), Err: fmt.Errorf("prowlarr: %s HTTP %d", what, resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prowlarr: %s HTTP %d", what, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("prowlarr: response exceeds 4 MiB")
	}
	return data, nil
}
