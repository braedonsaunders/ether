//go:build !plexparity && !scorecard

package app

import "github.com/braedonsaunders/ether/internal/core"

func localEditionVariants(string, *core.Title) ([]core.ReleaseVariant, error) {
	return nil, nil
}

func localEditionVariantsMany(string, []core.Title) (map[core.RatingKey][]core.ReleaseVariant, error) {
	return map[core.RatingKey][]core.ReleaseVariant{}, nil
}

func labMediaVariants(string, *core.Title) ([]core.ReleaseVariant, bool) {
	return nil, false
}
