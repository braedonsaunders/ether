package sessions

import (
	"context"
	"crypto/subtle"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"strings"
)

// StreamGrantParam is the scoped media capability used by existing Plex
// clients. It is independent of account credentials.
const StreamGrantParam = "X-Ether-Stream-Token"

// GrantURL appends a broker-generated stream capability to a media URL.
// Capabilities passed here must be URL-safe.
func GrantURL(raw, token string) string {
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + StreamGrantParam + "=" + token
}

// GrantBinding is the playback-scoped capability and its active viewer.
type GrantBinding struct {
	Token    string
	ViewerID int64
	Started  bool
}

// AuthenticateGrant verifies a stream capability and rechecks the viewer on
// every media request, including cached segments. Callers apply playback policy.
func AuthenticateGrant(ctx context.Context, method, token string, binding GrantBinding, active func(context.Context, int64) *core.Viewer) (*core.Viewer, bool) {
	if method != http.MethodGet && method != http.MethodHead {
		return nil, false
	}
	if !binding.Started || binding.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(binding.Token)) != 1 {
		return nil, false
	}
	if active == nil {
		return nil, false
	}
	viewer := active(ctx, binding.ViewerID)
	if viewer == nil {
		return nil, false
	}
	return viewer, true
}
