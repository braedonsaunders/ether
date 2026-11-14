package frontend

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"sync"
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
// images are deliberately excluded. Portraits use PersonImageGrantBatch or a
// scoped single grant; channel images use a scoped single grant.
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

// PublicArtwork lists title artwork an unauthenticated image request may
// be served: artwork of a title visible to every active viewer under
// current policy (sections, content ratings, the owner's content policy
// and inherited show rules). ok is false when the title is missing,
// hidden from any viewer, or not a plain title (collections, playlists
// and extras are never public). reader is a real active viewer the bytes
// may be read as — every viewer passed, so any of them may read — and is
// never a requester identity: there is none. Implementations cache the
// intersection per policy epoch, so a restriction change takes effect
// without waiting for a TTL.
type PublicArtwork interface {
	PublicArtwork(ctx context.Context, device core.Device, item core.RatingKey) (images []Image, reader *core.Viewer, ok bool, err error)
}

// PublicImageServer is the optional image-grant extension that serves
// one PublicArtwork-selected image to a request with no credential. It
// re-checks public visibility and that the image belongs to the item
// before writing bytes, mirroring Serve's re-authorization.
type PublicImageServer interface {
	ServePublic(http.ResponseWriter, *http.Request, core.Device, core.RatingKey, Image)
}

// ImagePageRequest signs artwork for titles the caller has already authorized
// for this page. The signer does not write a grant row.
type ImagePageRequest struct {
	Ranked     map[core.RatingKey]int
	RankedWide bool
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

// ImagePageRenewal re-arms images SignPage sealed earlier, for Credential.
// Items are SignPage results, unchanged.
type ImagePageRenewal struct {
	Credential string
	Device     core.Device
	Items      []ImageBatchItem
}

// ImagePageRenewer keeps a cached listing's image URLs valid past the page
// grant lifetime: each renewal starts a full lifetime for Credential. It
// authorizes nothing by itself; Serve still rechecks the credential,
// expiry, revocation and current artwork policy.
type ImagePageRenewer interface {
	RenewPage(context.Context, ImagePageRenewal) error
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
// Present keys have authoritative listings (including empty listings); missing
// keys must not be granted. SignPage and IssueBatch use it without falling back
// to scalar authorization for missing keys or errors.
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
// requested key, else the one this request read as a row (ArtworkRows).
func ArtworkTitle(ctx context.Context, key core.RatingKey) (*core.Title, bool) {
	title, _ := ctx.Value(artworkTitleKey{}).(*core.Title)
	if title != nil && title.Key == key {
		return title, true
	}
	if rows := ArtworkRowsFrom(ctx); rows != nil {
		if t, ok := rows.Get(key); ok {
			return &t, true
		}
	}
	return nil, false
}

// ArtworkRows holds the titles an artwork request read as visibility rows:
// the row, with the facts the viewer's policy reads, and no credits. Only a
// request that never lists credited people's portraits (a grant of a
// title's own images: poster, art, logo...) reads titles this way; its
// authorization and its image read then share them (ArtworkTitle).
type ArtworkRows struct {
	mu     sync.Mutex
	titles map[core.RatingKey]core.Title
}

// Put records a title the request judged.
func (r *ArtworkRows) Put(t core.Title) {
	if r == nil || t.Key == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.titles == nil {
		r.titles = map[core.RatingKey]core.Title{}
	}
	r.titles[t.Key] = t
}

// Get returns a title the request recorded.
func (r *ArtworkRows) Get(key core.RatingKey) (core.Title, bool) {
	if r == nil {
		return core.Title{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.titles[key]
	return t, ok
}

type artworkRowsKey struct{}

// WithArtworkRows marks ctx as an artwork request that lists no credited
// people's portraits (ArtworkRows).
func WithArtworkRows(ctx context.Context) context.Context {
	if ArtworkRowsFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, artworkRowsKey{}, &ArtworkRows{})
}

// ArtworkRowsFrom returns WithArtworkRows' holder, or nil.
func ArtworkRowsFrom(ctx context.Context) *ArtworkRows {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(artworkRowsKey{}).(*ArtworkRows)
	return r
}

// LibraryCollagePrefix marks a generated library-view collage. The remainder
// is the hex identity of the view and the child posters it was built from.
const LibraryCollagePrefix = "library-collage/"

// LibraryCollageSource builds that collage for one library view. viewID is
// the view GUID. A change in the child posters produces a different image.
type LibraryCollageSource interface {
	LibraryCollage(context.Context, *Viewer, Device, string) (Image, error)
}
