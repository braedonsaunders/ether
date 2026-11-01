package collections

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// LookAhead is a server-wide collection that lists upcoming releases.
type LookAhead struct {
	Name    string
	Members []core.RatingKey
}

// LookAheads returns the enabled server-wide collections that admit
// upcoming releases, with their last built members shared with every
// viewer. It reads stored builds only.
func (s *Service) LookAheads(ctx context.Context) ([]LookAhead, error) {
	rows, err := s.loadRows(ctx)
	if err != nil {
		return nil, err
	}
	var out []LookAhead
	for _, r := range rows {
		if !r.enabled || r.owner != 0 || (r.spec.ReleaseMode != "upcoming" && r.spec.ReleaseMode != "mixed") {
			continue
		}
		keys, unshared, err := s.loadMembers(ctx, r.id)
		if err != nil {
			return nil, err
		}
		la := LookAhead{Name: r.spec.Name}
		for _, k := range keys {
			if !unshared[k] {
				la.Members = append(la.Members, k)
			}
		}
		out = append(out, la)
	}
	return out, nil
}
