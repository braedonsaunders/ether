package newznab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
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
	// reported, including ones filtered out below (unknown protocol).
	// The default import is additive (upsert only) and must not use this
	// list to remove stored indexers; only an explicit owner-requested
	// sync (import with sync=true) may reconcile against it, and even
	// then never when it is empty or the discovery skipped indexers.
	PresentIDs []string `json:"presentIds,omitempty"`
}

// prowlarrScope fingerprints a Prowlarr base URL; indexer IDs are
// "prowlarr-<scope>-<id>", so the scope ties an imported indexer to the
// instance (and base path) it came from.
func prowlarrScope(base string) string {
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:6])
}

// normalizeProwlarrBase parses and validates a Prowlarr base URL exactly
// as discovery requires, then canonicalises it so the same instance always
// yields the same scope: scheme/host lower-cased and a default port
// (80/http, 443/https) dropped. It is the single normalisation used by
// both DiscoverProwlarrReport and ProwlarrIDPrefix, so IDs stay consistent
// however the owner spells the URL.
func normalizeProwlarrBase(raw string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("prowlarr: URL must be an HTTP(S) base URL")
	}
	base.Scheme = strings.ToLower(base.Scheme)
	if base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("prowlarr: URL must be an HTTP(S) base URL")
	}
	host := strings.ToLower(base.Hostname())
	port := base.Port()
	if (base.Scheme == "http" && port == "80") || (base.Scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		base.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		base.Host = "[" + host + "]"
	default:
		base.Host = host
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawPath = ""
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

// ProwlarrLocallyEdited reports whether a stored indexer that carries
// this instance's ID prefix no longer matches the identity discovery
// would have generated for it, i.e. the owner edited it locally after
// import. There is no import history to diff against, so the rule is
// deliberately narrow and purely pattern-based. An entry is locally
// edited when ANY of these holds:
//
//   - the ID suffix after "prowlarr-<scope>-" is not the numeric source
//     indexer ID discovery generates;
//   - the URL is not the generated proxy URL "<normalised base>/<id>";
//   - the name does not carry the generated "[<scope>:<id>]" marker.
//
// Edits to limits, grabs or priority alone are NOT detectable this way
// and do not protect an entry from an explicit sync; only the identity
// fields above do. Callers must only ask about same-scope entries.
func ProwlarrLocallyEdited(cfg config.Prowlarr, ix config.Indexer) bool {
	prefix := ProwlarrIDPrefix(cfg)
	if prefix == "" || !strings.HasPrefix(ix.ID, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(ix.ID, prefix)
	if suffix == "" {
		return true
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return true
		}
	}
	base, err := normalizeProwlarrBase(cfg.URL)
	if err != nil {
		return true
	}
	if ix.URL != base.String()+"/"+suffix {
		return true
	}
	marker := "[" + prowlarrScope(base.String()) + ":" + suffix + "]"
	return !strings.Contains(ix.Name, marker)
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
func DiscoverProwlarrReport(ctx context.Context, cfg config.Prowlarr, opts Options) (ProwlarrDiscovery, error) {
	base, err := normalizeProwlarrBase(cfg.URL)
	if err != nil {
		return ProwlarrDiscovery{}, err
	}
	if cfg.APIKey == "" {
		return ProwlarrDiscovery{}, fmt.Errorf("prowlarr: API key is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/api/v1/indexer", nil)
	if err != nil {
		return ProwlarrDiscovery{}, err
	}
	req.Header.Set("X-Api-Key", cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	cl := opts.HTTPClient
	if cl == nil {
		cl = &http.Client{Timeout: defaultHTTPTimeout}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return ProwlarrDiscovery{}, fmt.Errorf("prowlarr: discovery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		// Propagate Retry-After exactly like the Newznab client does for a
		// 429 (see Client.get/recordRateLimit): callers can inspect the
		// *RateLimitError instead of guessing a pause.
		return ProwlarrDiscovery{}, &RateLimitError{RetryAfter: validatedRetryAfter(resp.Header.Get("Retry-After")), Err: fmt.Errorf("prowlarr: discovery HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return ProwlarrDiscovery{}, fmt.Errorf("prowlarr: discovery HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return ProwlarrDiscovery{}, err
	}
	if len(data) > 4<<20 {
		return ProwlarrDiscovery{}, fmt.Errorf("prowlarr: response exceeds 4 MiB")
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
		// The default import stays additive and never removes a stored
		// indexer absent from this list; only an explicit sync may.
		present = append(present, id)
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
