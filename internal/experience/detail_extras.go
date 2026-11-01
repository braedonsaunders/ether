package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/extras"
)

type ExtrasSource interface {
	Enabled() bool
	List(context.Context, *core.Title, string) ([]extras.Extra, error)
}

// DetailExtras selects enabled clips in source order. Numeric dialect filters
// are parsed by the adapter; an empty selection retains the complete list.
func DetailExtras(ctx context.Context, source ExtrasSource, parent *core.Title, language string, wanted map[int]bool) ([]extras.Extra, error) {
	if source == nil || parent == nil || !source.Enabled() {
		return nil, nil
	}
	list, err := source.List(ctx, parent, language)
	if err != nil {
		return nil, err
	}
	if len(wanted) == 0 {
		return list, nil
	}
	out := list[:0:0]
	for _, clip := range list {
		if wanted[clip.ExtraType] {
			out = append(out, clip)
		}
	}
	return out, nil
}
