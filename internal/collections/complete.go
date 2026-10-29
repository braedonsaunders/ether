package collections

import (
	"context"
	"fmt"
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
)

// ListComplete builds every eligible collection or returns an error. Unlike
// List, it never suppresses refresh or franchise-list failures; consumers that
// advertise an exact total must not use a partial result.
func (s *Service) ListComplete(ctx context.Context, user *core.User, typ core.MediaType) ([]core.Collection, error) {
	rows, err := s.loadRows(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].builtin && !rows[j].builtin })
	out := make([]core.Collection, 0)
	for _, row := range rows {
		if !visible(row, user) || !row.spec.Targets.CollectionsTab || !typeMatches(row.spec, typ) {
			continue
		}
		keys, err := s.serve(ctx, row, user, true)
		if err != nil {
			return nil, fmt.Errorf("collections: complete list: %w", err)
		}
		if len(keys) > 0 {
			out = append(out, s.collectionFor(ctx, row, user, typ, keys))
		}
	}
	out = append(out, s.listEphemeral(user, typ)...)
	hideFranchises := s.catCfg != nil && s.catCfg().HideFranchiseCollections
	if !hideFranchises && (typ == "" || typ == core.TypeMovie) {
		franchises, err := s.mirror.Franchises(ctx)
		if err != nil {
			return nil, fmt.Errorf("collections: complete franchises: %w", err)
		}
		out = append(out, franchises...)
	}
	return out, nil
}
