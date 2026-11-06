package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// TrickplayCacheKey binds preview work to the selected item, release and
// opened source member. An expired local revision is not safe to serve.
func TrickplayCacheKey(s *PlaySession) string {
	if s == nil || s.Resolution == nil {
		return ""
	}
	release := s.Resolution.Release.ID
	if release == "" {
		return ""
	}
	// A logical-only adapter still has item/version scope even without an
	// opened member revision. Opened sources must provide their verified revision.
	revision := "logical"
	if s.Source != nil {
		revision = SourceRevision(s)
		if remote, ok := s.Source.(interface{ SourceRevision() string }); ok {
			revision = remote.SourceRevision()
		}
	}
	if revision == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(int64(s.Key), 10) + "\x00" + release + "\x00" + revision))
	return "preview-" + hex.EncodeToString(sum[:16])
}
