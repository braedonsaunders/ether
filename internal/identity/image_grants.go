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
type ImageGrantService struct {
	db         *sql.DB
	surface    frontend.Protocol
	grants     *Grants
	experience func() frontend.Experience
	images     ImageSource
	artwork    frontend.Artwork
	now        func() time.Time
	ttl        time.Duration
	admission  imageAdmission
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
	denied             sync.Map
	gen                atomic.Uint64
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
func NewArtworkImageGrantService(db *sql.DB, surface frontend.Protocol, grants *Grants, experience func() frontend.Experience, artwork frontend.Artwork, ttl time.Duration, now func() time.Time) (*ImageGrantService, error) {
	if artwork == nil {
		return nil, errors.New("identity: missing artwork source")
	}
	s, err := NewImageGrantService(db, surface, grants, experience, unavailableImageSource{}, ttl, now)
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

func NewImageGrantService(db *sql.DB, surface frontend.Protocol, grants *Grants, experience func() frontend.Experience, images ImageSource, ttl time.Duration, now func() time.Time) (*ImageGrantService, error) {
	if db == nil || surface == "" || surface == "plex" || grants == nil || experience == nil || images == nil || ttl < time.Second {
		return nil, errors.New("identity: invalid image grant dependencies")
	}
	if now == nil {
		now = time.Now
	}
	return &ImageGrantService{db: db, surface: surface, grants: grants, experience: experience, images: images, now: now, ttl: ttl, admission: defaultImageAdmission()}, nil
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
		images, complete, err := s.artwork.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Device: device, Channel: channel})
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
		err = s.revokeAdmitted(ctx, viewer.ID, source, hash)
	}
	if err == nil {
		s.gen.Add(1)
	}
	return err
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
	if err = s.authorized(r.Context(), viewer, device, core.RatingKey(item), channel, img); err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeGrantedImage(w, r, viewer, device, core.RatingKey(item), channel, img, hash, sourceHash, deviceJSON)
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
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width < 0 || width > 4000 {
		width = 500
	}
	if width == 0 && r.URL.Query().Get("h") == "" {
		width = 500
	}
	height, _ := strconv.Atoi(r.URL.Query().Get("h"))
	format := r.URL.Query().Get("format")
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
	if etag := imageETag(img, width, height, format, blur); etag != "" {
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
		data, contentType, err = s.artwork.Read(r.Context(), frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: item, Channel: channel}, img, frontend.ArtworkTransform{Width: width, Height: height, Format: format, Blur: blur})
	} else {
		data, contentType, err = s.images.Get(r.Context(), img.Path, width)
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
