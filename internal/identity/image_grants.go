package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("frontend_image_grants", 1, `CREATE TABLE frontend_image_grants (hash TEXT PRIMARY KEY, source_hash TEXT NOT NULL REFERENCES identity_grants(hash) ON DELETE CASCADE, viewer_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, surface TEXT NOT NULL, item INTEGER NOT NULL, device TEXT NOT NULL, image_kind TEXT NOT NULL, image_path TEXT NOT NULL, image_tag TEXT NOT NULL, expires INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0); CREATE INDEX frontend_image_grants_source ON frontend_image_grants(source_hash);`)
	store.RegisterAfter("frontend_image_grants", "auth", "identity_grants")
	store.Register("frontend_image_grants", 2, `ALTER TABLE frontend_image_grants ADD COLUMN channel TEXT NOT NULL DEFAULT '';`)
	store.Register("frontend_image_grants", 3, `ALTER TABLE frontend_image_grants ADD COLUMN image_subject TEXT NOT NULL DEFAULT '';`)
	store.Register("frontend_image_grants", 4, `CREATE INDEX frontend_image_grants_scope_expiry ON frontend_image_grants(viewer_id,surface,expires,hash);`)
	store.Register("frontend_image_grants", 5, `CREATE INDEX frontend_image_grants_expiry ON frontend_image_grants(expires,hash);`)
	// The expression and cleanup query use the established 64-shard layout.
	store.Register("frontend_image_grants", 6, `CREATE INDEX frontend_image_grants_shard_expiry ON frontend_image_grants((viewer_id % 64),expires,hash);`)
}

type ImageSource interface {
	Get(context.Context, string, int) ([]byte, string, error)
}

// ImageGrantOptions supplies adapter-owned credential parsing and public URL policy.
type ImageGrantOptions struct {
	Token             func(*http.Request) string
	PublicContentTags bool
}

type ImageGrantService struct {
	requestOptions ImageGrantOptions
	db             *sql.DB
	surface        frontend.Protocol
	grants         *Grants
	experience     func() frontend.Experience
	images         ImageSource
	artwork        frontend.Artwork
	now            func() time.Time
	ttl            time.Duration
	admission      imageAdmission
	// beforeImageCleanup is test-only. It runs after the shard lock and the
	// source recheck, and before expired-row deletion.
	beforeImageCleanup func(*sql.Tx)
	pageMu             sync.Mutex
	pageKeyBytes       []byte
	imageGen           func() uint64
	artMu              sync.Mutex
	artGen             uint64
	art                map[pageArtKey][]frontend.Image
	shortMu            sync.Mutex
	short              map[string]pageClaims
	// denied maps revoked grant digests to when the entry may be dropped
	// (Unix ms); see deny.
	denied      sync.Map
	deniedSwept atomic.Int64
	gen         atomic.Uint64
	// cleaned is when each viewer's expired image rows were last pruned
	// on the issue path (imageCleanupDue).
	cleanMu sync.Mutex
	cleaned map[int64]time.Time
}

// Generation changes when an image grant is revoked, so a cached page stops
// repeating that grant.
func (s *ImageGrantService) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

// NewArtworkImageGrantService serves descriptors through the authorized
// artwork source. The source is consulted again for every fetch, after the
// session, viewer and Experience policy checks performed by Serve.
func NewArtworkImageGrantService(db *sql.DB, surface frontend.Protocol, grants *Grants, experience func() frontend.Experience, artwork frontend.Artwork, ttl time.Duration, now func() time.Time, options ...ImageGrantOptions) (*ImageGrantService, error) {
	if artwork == nil {
		return nil, errors.New("identity: missing artwork source")
	}
	s, err := NewImageGrantService(db, surface, grants, experience, unavailableImageSource{}, ttl, now, options...)
	if err != nil {
		return nil, err
	}
	s.artwork = artwork
	return s, nil
}

type unavailableImageSource struct{}

func (unavailableImageSource) Get(context.Context, string, int) ([]byte, string, error) {
	return nil, "", core.ErrNotAvailable
}

func NewImageGrantService(db *sql.DB, surface frontend.Protocol, grants *Grants, experience func() frontend.Experience, images ImageSource, ttl time.Duration, now func() time.Time, options ...ImageGrantOptions) (*ImageGrantService, error) {
	if db == nil || surface == "" || grants == nil || experience == nil || images == nil || ttl < time.Second {
		return nil, errors.New("identity: invalid image grant dependencies")
	}
	if now == nil {
		now = time.Now
	}
	var requestOptions ImageGrantOptions
	if len(options) > 0 {
		requestOptions = options[0]
	}
	return &ImageGrantService{requestOptions: requestOptions, db: db, surface: surface, grants: grants, experience: experience, images: images, now: now, ttl: ttl, admission: defaultImageAdmission()}, nil
}
func validImageSecret(secret string) (string, bool) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(secret)
	if e != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != secret {
		return "", false
	}
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:]), true
}
func (s *ImageGrantService) authorized(ctx context.Context, viewer *core.Viewer, device core.Device, item core.RatingKey, channel string, image frontend.Image) error {
	if strings.HasPrefix(image.Path, frontend.LibraryCollagePrefix) {
		experience := s.experience()
		if experience == nil {
			return core.ErrNotAvailable
		}
		source, ok := experience.(frontend.LibraryCollageSource)
		if !ok {
			return core.ErrNotAvailable
		}
		got, err := source.LibraryCollage(ctx, viewer, device, image.Subject)
		if err != nil {
			return err
		}
		if frontend.SameImage(got, image) {
			return nil
		}
		return core.ErrUnauthorized
	}
	if channel != "" {
		if s.artwork == nil {
			return core.ErrNotAvailable
		}
		images, complete, err := s.artwork.List(core.WithLiveApp(ctx, string(s.surface)), channelImageRequest(viewer, device, channel, image))
		if err != nil {
			return err
		}
		if !complete {
			return core.ErrNotAvailable
		}
		for _, candidate := range images {
			if frontend.SameImage(candidate, image) {
				return nil
			}
		}
		return core.ErrUnauthorized
	}
	experience := s.experience()
	if experience == nil {
		return core.ErrNotAvailable
	}
	if s.surface == "silo" && image.Kind == frontend.ImagePerson {
		people, ok := experience.(frontend.PeopleImageReader)
		if !ok {
			return core.ErrNotAvailable
		}
		return people.AuthorizePersonImage(ctx, viewer, device, item, image, nil)
	}

	var images []frontend.Image
	var complete bool
	var err error
	if scoped, ok := experience.(frontend.ItemArtwork); ok {
		images, complete, err = scoped.ItemArtwork(ctx, viewer, device, item)
	} else {
		var detail frontend.ItemDetail
		detail, err = experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: item})
		images, complete = detail.Images, detail.ImagesAuthoritative
	}
	if err != nil {
		return err
	}
	if !complete {
		return core.ErrNotAvailable
	}
	for _, candidate := range images {
		if frontend.SameImage(candidate, image) {
			return nil
		}
	}
	return core.ErrUnauthorized
}
func (s *ImageGrantService) Issue(ctx context.Context, req frontend.ImageGrantRequest) (string, error) {
	viewer, err := s.grants.Resolve(ctx, string(s.surface), req.Credential)
	if err != nil {
		return "", err
	}
	source, ok := grantHash(req.Credential)
	if !ok {
		return "", core.ErrUnauthorized
	}
	if (req.Item == 0) == (req.Channel == "") || len(req.Channel) > 512 || req.Image.Path == "" || req.Image.Kind == "" || len(req.Image.Path) > 1024 || len(req.Image.Kind) > 128 || len(req.Image.Tag) > 256 || len(req.Image.Subject) > 256 {
		return "", core.ErrUnauthorized
	}
	if req.Image.Kind != frontend.ImagePerson {
		ctx = frontend.WithArtworkRows(ctx)
	}
	if err := s.authorized(ctx, viewer, req.Device, req.Item, req.Channel, req.Image); err != nil {
		return "", err
	}
	device, err := json.Marshal(req.Device)
	if err != nil || len(device) > 4096 {
		return "", core.ErrUnauthorized
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(random[:])
	hash, _ := validImageSecret(secret)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	row := imageBatchRow{hash: hash, item: req.Item, image: req.Image}
	if err = s.insertAdmitted(ctx, tx, viewer.ID, source, req.Device.ID, string(device), req.Channel, []imageBatchRow{row}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return secret, nil
}
func (s *ImageGrantService) Revoke(ctx context.Context, credential, secret string) error {
	viewer, err := s.grants.Resolve(ctx, string(s.surface), credential)
	if err != nil {
		return err
	}
	source, ok := grantHash(credential)
	if !ok {
		return core.ErrUnauthorized
	}
	if isShortImageTag(secret) {
		err = s.revokeShortGrant(ctx, viewer.ID, source, secret)
	} else if strings.HasPrefix(secret, pageGrantPrefix) {
		err = s.revokePageGrant(ctx, viewer.ID, source, secret)
	} else {
		var hash string
		hash, ok = validImageSecret(secret)
		if !ok {
			return core.ErrUnauthorized
		}
		if err = s.revokeAdmitted(ctx, viewer.ID, source, hash); err == nil {
			s.gen.Add(1)
		}
	}
	return err
}

// ServePublic serves one title image to a request carrying no
// credential at all (frontend.PublicImageServer, #405). There is no
// grant to validate; instead the image must currently be public — the
// title visible to every active viewer (frontend.PublicArtwork) — and
// img must be one of that title's listed public images, re-checked here
// exactly as Serve re-authorizes a grant before writing bytes. Person
// and channel images are never public.
func (s *ImageGrantService) ServePublic(w http.ResponseWriter, r *http.Request, device core.Device, item core.RatingKey, img frontend.Image) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	switch img.Kind {
	case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
	default:
		http.NotFound(w, r)
		return
	}
	public, ok := s.experience().(frontend.PublicArtwork)
	if !ok {
		http.NotFound(w, r)
		return
	}
	r = artworkRowsRequest(r, img)
	images, reader, visible, err := public.PublicArtwork(r.Context(), device, item)
	if err != nil || !visible || reader == nil {
		http.NotFound(w, r)
		return
	}
	for _, candidate := range images {
		if frontend.SameImage(candidate, img) {
			s.writeGrantedImage(w, r, reader, device, item, "", img, "", "", "")
			return
		}
	}
	http.NotFound(w, r)
}

func (s *ImageGrantService) Serve(w http.ResponseWriter, r *http.Request, secret string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", 405)
		return
	}
	if isShortImageTag(secret) {
		s.serveShortTag(w, r, secret)
		return
	}
	if strings.HasPrefix(secret, pageGrantPrefix) {
		s.servePageGrant(w, r, secret)
		return
	}
	hash, ok := validImageSecret(secret)
	if !ok {
		http.NotFound(w, r)
		return
	}
	now := s.now().UnixMilli()
	var viewerID, item int64
	var deviceJSON, channel, sourceHash string
	var img frontend.Image
	err := s.db.QueryRowContext(r.Context(), `SELECT i.viewer_id,i.item,i.device,i.image_kind,i.image_path,i.image_tag,i.channel,i.image_subject,i.source_hash FROM frontend_image_grants i JOIN capability_headers h ON h.namespace='image' AND h.digest=i.hash AND h.viewer_id=i.viewer_id AND h.surface=i.surface AND h.expires=i.expires JOIN identity_grants g ON g.hash=i.source_hash AND g.viewer_id=i.viewer_id AND g.surface=i.surface JOIN auth_users u ON u.id=i.viewer_id WHERE i.hash=? AND i.surface=? AND i.revoked=0 AND i.expires>? AND g.revoked=0 AND g.expires>? AND u.active=1`, hash, string(s.surface), now, now).Scan(&viewerID, &item, &deviceJSON, &img.Kind, &img.Path, &img.Tag, &channel, &img.Subject, &sourceHash)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.grants.viewers.ActiveUser(r.Context(), viewerID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var device core.Device
	if json.Unmarshal([]byte(deviceJSON), &device) != nil {
		http.NotFound(w, r)
		return
	}
	r = artworkRowsRequest(r, img)
	if err = s.authorized(r.Context(), viewer, device, core.RatingKey(item), channel, img); err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeGrantedImage(w, r, viewer, device, core.RatingKey(item), channel, img, hash, sourceHash, deviceJSON)
}

// artworkRowsRequest lets an image read authorize and list its title from
// visibility rows (frontend.WithArtworkRows), shared by its authorization
// and its read. A credited person's portrait is listed from the title's
// credits, so its title is read in full.
func artworkRowsRequest(r *http.Request, img frontend.Image) *http.Request {
	if img.Kind == frontend.ImagePerson {
		return r
	}
	return r.WithContext(frontend.WithArtworkRows(r.Context()))
}

func imageETag(img frontend.Image, width, height int, format string, blur bool) string {
	identity := img.Tag
	if identity == "" {
		identity = img.Path
	}
	if identity == "" {
		return ""
	}
	blurText := "0"
	if blur {
		blurText = "1"
	}
	sum := sha256.Sum256([]byte(identity + "\x00" + strconv.Itoa(width) + "\x00" + strconv.Itoa(height) + "\x00" + format + "\x00" + blurText))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func (s *ImageGrantService) writeGrantedImage(w http.ResponseWriter, r *http.Request, viewer *core.Viewer, device core.Device, item core.RatingKey, channel string, img frontend.Image, hash, sourceHash, deviceJSON string) {
	width := grantedImageWidth(r, s.surface)
	mediaBrowser := s.surface == "jellyfin" || s.surface == "emby"
	quality := 0
	if mediaBrowser {
		quality, _ = strconv.Atoi(r.URL.Query().Get("quality"))
		if quality < 0 || quality > 100 {
			quality = 0
		}
	}
	height, _ := strconv.Atoi(r.URL.Query().Get("h"))
	if mediaBrowser && height == 0 {
		for _, name := range []string{"fillHeight", "maxHeight"} {
			if n, _ := strconv.Atoi(r.URL.Query().Get(name)); n > 0 {
				height = n
				break
			}
		}
	}
	if mediaBrowser && width == 0 && height == 0 && img.Kind != frontend.ImageBackdrop && img.Kind != frontend.ImageThumb && img.Kind != frontend.ImageLogo {
		width = 500
	}
	format := r.URL.Query().Get("format")
	if mediaBrowser {
		// The shared renderer encodes JPEG and PNG. Other Jellyfin formats
		// (Webp, Gif, Bmp, Svg), and unknown values, use the source format.
		switch strings.ToLower(format) {
		case "jpg", "jpeg":
			format = "jpeg"
		case "png":
			format = "png"
		default:
			format = ""
		}
	}
	blur := r.URL.Query().Get("blur") == "1"
	if s.artwork != nil {
		if height < 0 || height > 4000 {
			http.Error(w, "invalid image height", http.StatusBadRequest)
			return
		}
		if format != "" && format != "jpeg" && format != "png" {
			http.Error(w, "invalid image format", http.StatusBadRequest)
			return
		}
		if blurQuery := r.URL.Query().Get("blur"); blurQuery != "" && blurQuery != "0" && blurQuery != "1" {
			http.Error(w, "invalid image blur", http.StatusBadRequest)
			return
		}
	}
	if etag := imageETag(img, width, height, format+strconv.Itoa(quality), blur); etag != "" {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, max-age=86400")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	var data []byte
	var contentType string
	var err error
	if s.artwork != nil {
		data, contentType, err = s.artwork.Read(core.WithLiveApp(r.Context(), string(s.surface)), grantedArtworkRequest(viewer, device, item, channel, img), img, frontend.ArtworkTransform{Width: width, Height: height, Format: format, Blur: blur, Quality: quality, Fit: mediaBrowser || s.surface == "vela"})
	} else {
		data, contentType, err = s.images.Get(r.Context(), img.Path, width)
	}
	if err == nil && s.surface == "vela" && !strings.HasPrefix(contentType, "image/svg") {
		data, contentType, err = fitGrantedImage(data, contentType, width, height)
	}
	if err != nil {
		http.Error(w, "image unavailable", 502)
		return
	}
	// A Silo portrait fetch may cross I/O after its first authorization. Check
	// current credited membership and fresh policy again before publishing bytes.
	if s.surface == "silo" && img.Kind == frontend.ImagePerson {
		if err := s.authorizePersonGrant(r.Context(), viewer, device, item, channel, img, hash, sourceHash, deviceJSON); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("ETag") == "" {
		h.Set("Cache-Control", "private, no-store")
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

var _ frontend.ImageGrants = (*ImageGrantService)(nil)
var _ frontend.PublicImageServer = (*ImageGrantService)(nil)

func grantedImageWidth(r *http.Request, surface frontend.Protocol) int {
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	// MediaBrowser and Silo name their width constraints maxWidth/fillWidth.
	if (surface == "silo" || surface == "jellyfin" || surface == "emby") && width == 0 {
		for _, name := range []string{"maxWidth", "fillWidth"} {
			if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n > 0 {
				width = min(n, 4000)
				if surface == "silo" {
					width = min(width, 3840)
				}
				break
			}
		}
	}
	if width < 0 || width > 4000 {
		width = 500
	}
	if width == 0 && r.URL.Query().Get("h") == "" && surface != "jellyfin" && surface != "emby" && surface != "vela" {
		width = 500
	}
	return width
}

// Programme descriptors use the existing channel capability and persist the
// exact airing identity in image_subject. Both admission and reads re-enumerate
// that airing under current channel/programme policy, without a library scan.
func channelImageRequest(viewer *core.Viewer, device core.Device, channel string, image frontend.Image) frontend.ArtworkRequest {
	req := frontend.ArtworkRequest{Viewer: viewer, Device: device, Channel: channel}
	if image.Kind == frontend.ImageStill || image.Kind == frontend.ImageBackdrop {
		req.Programme = image.Subject
	}
	return req
}
func grantedArtworkRequest(viewer *core.Viewer, device core.Device, item core.RatingKey, channel string, image frontend.Image) frontend.ArtworkRequest {
	if channel != "" {
		return channelImageRequest(viewer, device, channel, image)
	}
	return frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: item}
}
