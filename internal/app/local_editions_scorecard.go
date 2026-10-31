//go:build scorecard && !plexparity

package app

import (
	"github.com/braedonsaunders/ether/internal/core"
)

func localEditionVariants(dir string, t *core.Title) ([]core.ReleaseVariant, error) {
	if t == nil {
		return nil, nil
	}
	got, err := scorecardEditionVariants(dir, []core.Title{*t})
	if err != nil {
		return nil, err
	}
	return got[t.Key], nil
}

func localEditionVariantsMany(dir string, titles []core.Title) (map[core.RatingKey][]core.ReleaseVariant, error) {
	return scorecardEditionVariants(dir, titles)
}
