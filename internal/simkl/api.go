package simkl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

func metadataClient() *http.Client { return egress.Client(egress.CapMetadata, 30*time.Second) }

// HTTPError exposes status and Retry-After without copying upstream bodies
// into logs. Callers must honor Retry-After before retrying a rate limit.
type HTTPError struct {
	Status     int
	Code       string
	RetryAfter string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("simkl: HTTP %d", e.Status) }

// request sends one bounded API request. Pacing and persisted retry policy
// belong to the coordinating service, shared across nodes.
func (c AuthClient) request(ctx context.Context, method, path, token string, query url.Values, body any, result any) error {
	if c.ClientID == "" || token == "" {
		return fmt.Errorf("simkl: credentials unavailable")
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("client_id", c.ClientID)
	query.Set("app-name", "ether")
	query.Set("app-version", "1.0")
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://api.simkl.com"
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path+"?"+query.Encode(), payload)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Ether/1.0")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = metadataClient()
	}
	safe := *hc
	safe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safe.Do(req)
	if err != nil {
		return fmt.Errorf("simkl: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&body)
		code := ""
		if body.Error == "max_items" {
			code = body.Error
		}
		return &HTTPError{Status: resp.StatusCode, Code: code, RetryAfter: resp.Header.Get("Retry-After")}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return fmt.Errorf("simkl: response too large")
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}
