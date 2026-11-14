package stremio

import "github.com/braedonsaunders/ether/internal/core"

// Stream is one stream object an add-on answers (backend.go's stream, the
// one decoder of it). Installed add-ons and Ether source plugins
// (internal/plugins/adapters) convert their answers with the same code as
// the Stremio indexer.
type Stream = stream

// StreamHints is a stream's behaviorHints.
type StreamHints = behaviorHints

// ConvertOptions carry what Convert needs beyond the streams.
type ConvertOptions struct {
	// Origin is the add-on's own scheme://host[:port], the one private
	// address its stream URLs may reach.
	Origin string
	// Instance identifies the configured add-on in HTTP and NZB GUIDs.
	Instance string
}

// Converted is the outcome of Convert. Source[i] is the index of the
// stream Releases[i] came from.
type Converted struct {
	Releases         []core.Release
	Source           []int
	SkippedNoHash    int
	SkippedAmbiguous int
}

// Convert turns an add-on's streams into releases exactly as the Stremio
// indexer does (convertWith).
func Convert(streams []Stream, req core.SearchRequest, look Lookup, opts ConvertOptions) Converted {
	var c Converted
	c.Releases, c.SkippedNoHash, c.SkippedAmbiguous = convertWith(streams, req, look.Packs, convertOptions{
		origin: opts.Origin, instance: opts.Instance, entryEpisode: look.EntryEpisode, specialEpisode: look.SpecialEpisode, sources: &c.Source,
	})
	return c
}
