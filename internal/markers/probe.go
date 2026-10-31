package markers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Probe checks that TheIntroDB answers, and that apiKey is accepted when one
// is set. An empty key probes the public lookup. 404 still means the service
// accepted the request.
func Probe(ctx context.Context, baseURL, apiKey string) error {
	if baseURL == "" {
		baseURL = "https://api.theintrodb.org/v4"
	}
	// A fixed, well-known movie. The probe only cares that the service
	// accepts the call, not that markers exist.
	endpoint := strings.TrimRight(baseURL, "/") + "/media?tmdb_id=550&duration_ms=13920000&merge_unknown=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Ether/1")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		if apiKey != "" {
			return errors.New("theintrodb rejected the key")
		}
		return fmt.Errorf("theintrodb: HTTP %d", resp.StatusCode)
	default:
		return fmt.Errorf("theintrodb: HTTP %d", resp.StatusCode)
	}
}
