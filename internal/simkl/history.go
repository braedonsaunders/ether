package simkl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var ErrIdentityNotFound = errors.New("simkl: history identity not found")

// HistoryPayload uses only identities represented unambiguously by Ether's
// native movie and episode keys. Local files and show/season keys are skipped.
// https://api.simkl.org/api-reference/simkl/add-to-history
func HistoryPayload(key core.RatingKey, at time.Time) (map[string]any, error) {
	if !key.Valid() || key.TMDB() <= 0 || at.IsZero() {
		return nil, errors.New("simkl: unsupported history identity or timestamp")
	}
	ids := map[string]string{"tmdb": strconv.FormatInt(key.TMDB(), 10)}
	watched := at.UTC().Format(time.RFC3339)
	switch key.Kind() {
	case core.KindMovie:
		return map[string]any{"movies": []any{map[string]any{"ids": ids, "watched_at": watched}}}, nil
	case core.KindEpisode:
		if key.Episode() <= 0 {
			return nil, errors.New("simkl: unsupported episode number")
		}
		episode := map[string]any{"number": key.Episode(), "watched_at": watched}
		season := map[string]any{"number": key.Season(), "episodes": []any{episode}}
		return map[string]any{"shows": []any{map[string]any{"ids": ids, "seasons": []any{season}}}}, nil
	default:
		return nil, errors.New("simkl: unsupported history identity")
	}
}

// PushHistory sends one completion; episode numbering must first be verified
// against Simkl's TV mapping by the caller. Anime often uses a different
// numbering scheme and is not safe to infer from an Ether key alone.
func (c AuthClient) PushHistory(ctx context.Context, token string, key core.RatingKey, at time.Time, tvNumberingVerified bool) error {
	if key.Kind() == core.KindEpisode && !tvNumberingVerified {
		return errors.New("simkl: episode numbering is not verified")
	}
	payload, err := HistoryPayload(key, at)
	if err != nil {
		return err
	}
	var result struct {
		NotFound *struct {
			Movies   []json.RawMessage `json:"movies"`
			Shows    []json.RawMessage `json:"shows"`
			Episodes []json.RawMessage `json:"episodes"`
		} `json:"not_found"`
	}
	if err = c.request(ctx, http.MethodPost, "/sync/history", token, url.Values{"skip_auto_watching": {"yes"}}, payload, &result); err != nil {
		return err
	}
	if result.NotFound == nil {
		return errors.New("simkl: invalid history response")
	}
	if len(result.NotFound.Movies)+len(result.NotFound.Shows)+len(result.NotFound.Episodes) > 0 {
		return ErrIdentityNotFound
	}
	return nil
}

// IsOrdinaryTV uses Simkl's documented redirect classification without
// following it. Anime is intentionally not assigned TMDB episode numbers.
// https://api.simkl.org/api-reference/simkl/redirect
func (c AuthClient) IsOrdinaryTV(ctx context.Context, tmdb int64) (bool, error) {
	if tmdb <= 0 || c.ClientID == "" {
		return false, errors.New("simkl: missing show identity")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.simkl.com"
	}
	q := url.Values{"to": {"simkl"}, "tmdb": {strconv.FormatInt(tmdb, 10)}, "type": {"tv"}, "client_id": {c.ClientID}, "app-name": {"ether"}, "app-version": {"1.0"}}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/redirect?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "Ether/1.0")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	safe := *hc
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		return false, &HTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || location.Scheme != "https" || location.Hostname() != "simkl.com" {
		return false, errors.New("simkl: invalid redirect identity")
	}
	parts := strings.Split(strings.Trim(location.Path, "/"), "/")
	if len(parts) < 2 {
		return false, errors.New("simkl: invalid redirect identity")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return false, errors.New("simkl: invalid redirect identity")
	}
	if parts[0] == "anime" {
		return false, nil
	}
	if parts[0] != "tv" {
		return false, errors.New("simkl: unexpected redirect type")
	}
	return true, nil
}
