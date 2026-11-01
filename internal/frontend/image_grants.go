package frontend

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
)

var ErrImageBatchLimit = errors.New("frontend: invalid artwork batch")

// ImageGrantRequest asks for one authorized image on one authenticated session.
// Credential is supplied in-process and must never be placed in an image URL.
type ImageGrantRequest struct {
	Credential string
	Device     core.Device
	Item       core.RatingKey
	Channel    string
	Image      Image
}

// ImageGrants issues and validates short-lived opaque image capabilities.
// Serve requires a capability, even when the client cannot send account tokens.
type ImageGrants interface {
	Issue(context.Context, ImageGrantRequest) (string, error)
	Revoke(context.Context, string, string) error
	Serve(http.ResponseWriter, *http.Request, string)
}

// ImageBatchRequest requests primary artwork for a page of items. At most 50
// distinct keys and 256 input keys are accepted; duplicates are removed in
// first-occurrence order. Empty Kinds selects poster. Person and channel
// images are deliberately excluded and must use their scoped single grant.
type ImageBatchRequest struct {
	Credential string
	Device     core.Device
	Items      []core.RatingKey
	Kinds      []string
}
type GrantedImage struct {
	Image  Image
	Secret string
}
type ImageBatchItem struct {
	Item          core.RatingKey
	Authoritative bool
	Images        []GrantedImage
}

// ImageGrantBatch is an optional extension for rows and grids. Hidden items
// are omitted. Results preserve first-occurrence item and descriptor order;
// each requested kind contributes at most its first descriptor. An authorized
// item with no images is distinguished from an unavailable artwork list by
// Authoritative. Every secret is independently revalidated when served.
type ImageGrantBatch interface {
	IssueBatch(context.Context, ImageBatchRequest) ([]ImageBatchItem, error)
}

// ImagePageRequest signs artwork for titles the caller has already authorized
// for this page. The signer does not write a grant row.
type ImagePageRequest struct {
	Credential string
	Device     core.Device
	Titles     []core.Title
	Kinds      []string
}

// ImagePageSigner is the list path for image URLs. Serve still rechecks the
// credential, expiry, revocation and current artwork policy.
type ImagePageSigner interface {
	SignPage(context.Context, ImagePageRequest) ([]ImageBatchItem, error)
}

// ItemArtwork is an optional cache-only authorization path for issuing grid
// artwork without computing playback versions or tracks.
type ItemArtwork interface {
	ItemArtwork(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]Image, bool, error)
}

// PageArtwork lists artwork for titles the caller already loaded.
type PageArtwork interface {
	ListPage(context.Context, *core.Viewer, core.Device, []core.Title) (map[core.RatingKey][]Image, error)
}

// ItemArtworkPage authorizes and lists artwork for one grid in a batch.
// SignPage uses it so a home page does not load each title on its own.
type ItemArtworkPage interface {
	ItemArtworkPage(context.Context, *core.Viewer, core.Device, []core.RatingKey) (map[core.RatingKey][]Image, error)
}

type artworkTitleKey struct{}

// WithArtworkTitle carries a title the caller already loaded and authorized.
// Artwork listing must not read that title again.
func WithArtworkTitle(ctx context.Context, title *core.Title) context.Context {
	if title == nil {
		return ctx
	}
	return context.WithValue(ctx, artworkTitleKey{}, title)
}

// ArtworkTitle returns the title stored by WithArtworkTitle when it is the
// requested key.
func ArtworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, bool) {
	title, _ := ctx.Value(artworkTitleKey{}).(*core.Title)
	if title == nil || title.Key != key {
		return nil, false
	}
	return title, true
}

// LibraryCollagePrefix marks a generated library-view collage. The remainder
// is the hex identity of the view and the child posters it was built from.
const LibraryCollagePrefix = "library-collage/"

// LibraryCollageSource builds that collage for one library view. viewID is
// the view GUID. A change in the child posters produces a different image.
type LibraryCollageSource interface {
	LibraryCollage(context.Context, *Viewer, Device, string) (Image, error)
}
