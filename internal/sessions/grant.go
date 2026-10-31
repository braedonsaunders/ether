package sessions

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// accountQueryKeys are credentials that must never be copied into a generated
// media URL. Playback-scoped stream grants are not in this set.
var accountQueryKeys = map[string]struct{}{
	"x-plex-token":         {},
	"api_key":              {},
	"apikey":               {},
	"x-emby-token":         {},
	"x-mediabrowser-token": {},
	"access_token":         {},
}

// StripAccountCredentials removes account credentials from a media URL.
// Relative paths and values without those keys are unchanged.
func StripAccountCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for key := range q {
		if _, ok := accountQueryKeys[strings.ToLower(key)]; ok {
			q.Del(key)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// StreamGrantParam is the scoped media capability used by existing Plex
// clients. It is independent of account credentials.
const StreamGrantParam = "X-Ether-Stream-Token"

// GrantURL appends a broker-generated stream capability to a media URL.
// Capabilities passed here must be URL-safe.
func GrantURL(raw, token string) string {
	raw = StripAccountCredentials(raw)
	if token == "" {
		return raw
	}
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
