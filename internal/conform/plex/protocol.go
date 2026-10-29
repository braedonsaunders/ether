package plex

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/conform"
)

// Protocol implements the Plex conformance dialect.
type Protocol struct{}

var _ conform.Protocol = Protocol{}

func (Protocol) Clients() map[string]conform.Client { return Clients() }
func (Protocol) Corpus(string) []conform.Request    { return Corpus() }
func (Protocol) Formats() []bool                    { return []bool{false, true} }
func (Protocol) Discover(ctx context.Context, c *http.Client, base, token, profile string, align *conform.DiscoveredIDs) conform.DiscoveredIDs {
	return Discover(ctx, c, base, token, profile, align)
}
func (Protocol) Fetch(ctx context.Context, c *http.Client, base, token string, r conform.Request, profile string, vars map[string]string, jsonMode bool) (conform.Response, error) {
	return Fetch(ctx, c, base, token, r, profile, vars, jsonMode)
}
func (Protocol) FixtureName(profile string, r conform.Request, jsonMode bool) string {
	return conform.FixtureName(profile, r.Path, jsonMode)
}
func (Protocol) Compare(a, b conform.Response) conform.Report { return conform.Compare(a, b) }

func (Protocol) Redact(r conform.Response) conform.Response { return r }
