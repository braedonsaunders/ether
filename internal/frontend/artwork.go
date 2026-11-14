package frontend

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
)

// ArtworkImageFile describes one candidate remote artwork variant.
type ArtworkImageFile = core.ArtworkImageFile

// ArtworkImages groups candidate artwork variants for a title by kind.
type ArtworkImages = core.ArtworkImages

// ArtworkRequest identifies the viewer and presentation context for artwork.
// Implementations must apply the same item visibility policy as Experience.
type ArtworkRequest struct {
	Viewer *core.Viewer
	Device core.Device
	Item   core.RatingKey
	// Channel is a stable LiveTV channel ID. Exactly one of Item and Channel
	// identifies the subject.
	Channel string
	// Programme selects one airing on Channel. Its ID is carried by Image.Subject
	// in a channel-bound programme image capability; it never grants a title.
	Programme string
}

// ArtworkTransform describes the requested output. Zero dimensions preserve
// the source's normal dimensions; Format is empty, "jpeg", or "png".
type ArtworkTransform struct {
	Width, Height int
	Quality       int  // JPEG quality when encoding; zero uses the default.
	Fit           bool // Fit within dimensions, preserving aspect ratio without upscaling.
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
	RemoteCache() ArtworkImageCache
	PaletteCache() ArtworkPaletteCache
	PosterRenderer() TitlePosterRenderer
	ImageURLFunc() ImageURLFunc
}

// ValidArtworkPath exposes the shared, pure artwork filename contract.
func ValidArtworkPath(path string) bool { return core.ValidArtworkPath(path) }

// NeedsGeneratedPoster exposes shared title fallback eligibility without I/O.
func NeedsGeneratedPoster(title *Title) bool { return core.NeedsGeneratedPoster(title) }

// ArtworkUploader accepts bounded owner artwork through protocol adapters.
type ArtworkUploader interface {
	Upload(context.Context, ArtworkRequest, string, []byte, string) error
}

type ArtworkUploadHandler interface {
	UploadRequest(http.ResponseWriter, *http.Request, ArtworkRequest, string, bool) error
}

type OwnerArtworkReader interface {
	OwnerArtwork(context.Context, ArtworkRequest, string) ([]byte, string, error)
}

// OwnerArtworkVersioner exposes a cheap cache identity for metadata image URLs.
type OwnerArtworkVersioner interface {
	OwnerArtworkVersion(*Title, string) string
}
