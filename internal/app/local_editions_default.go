//go:build !plexparity && !scorecard

package app

import "github.com/braedonsaunders/ether/internal/core"

func localEditionVariants(string, *core.Title) ([]core.ReleaseVariant, error) {
	return nil, nil
}

func labMediaVariants(string, *core.Title) ([]core.ReleaseVariant, bool) {
	return nil, false
}
