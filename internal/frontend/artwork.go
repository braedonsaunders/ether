package frontend

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// ArtworkRequest identifies the viewer and presentation context for artwork.
// Implementations must apply the same item visibility policy as Experience.
type ArtworkRequest struct {
	Viewer *core.Viewer
	Device core.Device
	Item   core.RatingKey
	// Channel is a stable LiveTV channel ID. Exactly one of Item and Channel
	// identifies the subject.
	Channel string
}

// ArtworkTransform describes the requested output. Zero dimensions preserve
// the source's normal dimensions; Format is empty, "jpeg", or "png".
type ArtworkTransform struct {
	Width, Height int
	Format        string
	Blur          bool
}

// Artwork is the host-side source of authorized descriptors and their bytes.
// An implementation reports complete when its currently selected artwork is
// authoritative, including on a cold cache. Alternatives may arrive later.
// It must recheck visibility and descriptor membership on Read;
// the descriptor is a reference, not an authorization credential.
type Artwork interface {
	List(context.Context, ArtworkRequest) (images []Image, complete bool, err error)
	Read(context.Context, ArtworkRequest, Image, ArtworkTransform) (data []byte, contentType string, err error)
}
