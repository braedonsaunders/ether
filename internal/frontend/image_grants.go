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
