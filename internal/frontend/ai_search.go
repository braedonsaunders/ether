package frontend

import "context"

// AISearcher refines a description through the shared Collections Describe
// pipeline. Calls may await the model and belong in an explicit asynchronous
// protocol job, never in a browse, title search, or descriptive cache read.
// Implementations cache normalized queries and apply viewer policy and ranking.
type AISearcher interface {
	AISearchAvailable() bool
	AISearch(context.Context, *Viewer, Device, string, ItemQuery) (SearchPage, error)
}
