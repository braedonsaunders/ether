package frontend

import "context"

// DescriptiveSearcher is deterministic viewer-scoped collection search. The
// implementation must not wait for a model or provider, or count the library.
type DescriptiveSearcher interface {
	DescriptiveSearch(context.Context, *Viewer, Device, string, ItemQuery) (SearchPage, error)
	DescriptiveSearchAvailable() bool
}
