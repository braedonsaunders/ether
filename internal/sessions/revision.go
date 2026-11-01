package sessions

import "github.com/braedonsaunders/ether/internal/core"

func sourceRevision(s *core.PlaySession) string { return core.SourceRevision(s) }
