package conform

import (
	"context"
	"net/http"
)

// Protocol supplies the wire-specific parts of a conformance run. Implementations
// must not retain credentials or use shared mutable state between runs.
type Protocol interface {
	Clients() map[string]Client
	Corpus(profile string) []Request
	Formats() []bool // false is XML, true is JSON
	Discover(context.Context, *http.Client, string, string, string, *DiscoveredIDs) DiscoveredIDs
	Fetch(context.Context, *http.Client, string, string, Request, string, map[string]string, bool) (Response, error)
	FixtureName(profile string, request Request, jsonMode bool) string
	Redact(response Response) Response
	Compare(reference, target Response) Report
}
