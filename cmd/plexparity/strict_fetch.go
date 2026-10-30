package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

var pathVariable = regexp.MustCompile(`\{\{([^{}]+)\}\}`)

// strictFetch refuses the generic conformance client's fallback IDs. Missing
// fixture discovery must stop the run rather than silently request item 1.
func strictFetch(ctx context.Context, client *http.Client, base, token string, req protocol.Request, profile string, vars map[string]string, jsonMode bool) (protocol.Response, error) {
	for _, match := range pathVariable.FindAllStringSubmatch(req.Path, -1) {
		if value, ok := vars[match[1]]; !ok || value == "" || pathVariable.MatchString(value) {
			return protocol.Response{}, fmt.Errorf("missing fixture value %q for %s", match[1], req.Path)
		}
	}
	return protocol.Fetch(ctx, client, base, token, req, profile, vars, jsonMode)
}
