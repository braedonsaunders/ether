package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// sourceRevision uses the stable release/file identity rather than source
// URLs or credentials. Local mutable files have no authoritative revision.
func sourceRevision(s *core.PlaySession) string {
	if s == nil || s.Source == nil {
		return ""
	}
	id := s.Source.ReleaseID()
	if id == "" || strings.HasPrefix(id, "local:") {
		return ""
	}
	name, size := s.Source.Name(), s.Source.Size()
	if name == "" || size <= 0 {
		return ""
	}
	data, _ := json.Marshal(struct {
		Key      core.RatingKey
		ID, Name string
		Size     int64
	}{s.Key, id, name, size})
	sum := sha256.Sum256(data)
	return "src-" + hex.EncodeToString(sum[:16])
}
