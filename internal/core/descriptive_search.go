package core

import "context"

// DescriptiveResult is an AI-generated collection search. Pending requests
// have no invented results; clients that can poll retain their pending state.
type DescriptiveResult struct {
	Spec  *CollectionSpec
	State string
	Title string
	Items []Title
}

// DescriptiveSearcher reuses the collection builder and never waits for AI
// unless wait is explicit (an asynchronous protocol job, not a search request).
type DescriptiveSearcher interface {
	DescriptiveSearchAvailable() bool
	DescribeSearch(context.Context, *User, string, bool) (DescriptiveResult, error)
}

// DescriptiveCatalog evaluates a generated spec locally, without gathering a
// library-sized in-memory collection or fetching external lists.
type DescriptiveCatalog interface {
	DescribeCandidates(context.Context, CollectionSpec, int) ([]Title, error)
}
