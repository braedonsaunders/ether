package core

import "context"

// On-demand stream links (Release.StreamOnDemand) make their source fetch
// the release when opened: a Stremio add-on that resolves a torrent on its
// debrid account, or queues and imports an NZB, on every request. Ether
// opens one such link at a time and only for work a viewer or the owner is
// waiting on, so a Play costs the source one release while it plays.

// OpensOnDemand reports whether reading r opens an on-demand stream link.
func (r Release) OpensOnDemand() bool {
	return r.StreamOnDemand && r.StreamURL != "" && r.Protocol == ProtocolHTTP
}

type noOnDemandKey struct{}

// WithoutOnDemandStreams marks ctx as work that must not open on-demand
// stream links although it is not speculative, such as a client's
// file-availability check on a detail page.
func WithoutOnDemandStreams(ctx context.Context) context.Context {
	return context.WithValue(ctx, noOnDemandKey{}, true)
}

// OnDemandStreamsAllowed reports whether ctx may open an on-demand stream
// link: Play, failover and the owner's own checks may. Speculative indexer
// work (warm-ups, prefetch, detail discovery), background checks and work
// marked WithoutOnDemandStreams may not; they answer ErrForegroundOnly.
func OnDemandStreamsAllowed(ctx context.Context) bool {
	if v, _ := ctx.Value(noOnDemandKey{}).(bool); v {
		return false
	}
	return !SpeculativeIndexerWork(ctx) && !IsBackgroundCheck(ctx)
}
