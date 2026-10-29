package core

import "context"

// ReleaseVariant is one playable version of a title, offered in the Plex
// "Play Version" picker: a distinct candidate release, by quality tier or
// edition (cut).
type ReleaseVariant struct {
	// ID is stable for the title and release; front ends encode it (or its
	// position) in Media/Part ids.
	ID string `json:"id"`
	// Label is the viewer-facing name, e.g. "4K Dolby Vision Remux" or
	// "1080p WEB-DL (High Compatibility)".
	Label string `json:"label"`
	// Edition is the cut, e.g. "Director's Cut", "Extended", "IMAX"; empty
	// for the theatrical/unspecified cut.
	Edition string `json:"edition,omitempty"`
	// Auto marks the resolver's own pick for this client ("Best for Device").
	Auto    bool    `json:"auto,omitempty"`
	Release Release `json:"release"`
	// Info is the expected media info, estimated from the release name until
	// the release has been probed.
	Info MediaInfo `json:"info"`
}

// VariantResolver lists distinct candidate releases of a title for a client,
// best first, without health-checking every one of them. Implemented by
// internal/resolver.
type VariantResolver interface {
	Variants(ctx context.Context, req ResolveRequest, max int) ([]ReleaseVariant, error)
}

// VariantPlayback is implemented by playback managers that can prepare a
// session on a specific release (the viewer picked a version). The release
// is health-checked like any other; when it is unavailable the manager falls
// back to the normal choice for the client.
type VariantPlayback interface {
	PrepareVariant(ctx context.Context, user *User, key RatingKey, caps ClientCaps, releaseID string) (*PlaySession, error)
}

// ExactVariantPlayback never substitutes a different release, including
// during later failover. The requested release may instead become unavailable.
type ExactVariantPlayback interface {
	PrepareExactVariant(ctx context.Context, user *User, key RatingKey, caps ClientCaps, releaseID string) (*PlaySession, error)
}
