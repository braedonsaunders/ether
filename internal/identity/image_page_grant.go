package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const pageGrantPrefix = "e1."

type pageArtKey struct {
	viewer   int64
	item     core.RatingKey
	language string
	rating   string
	sections string
}

type pageImage struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Tag     string `json:"tag,omitempty"`
	Subject string `json:"subject,omitempty"`
}

type pageClaims struct {
	Item    int64           `json:"item"`
	Kind    string          `json:"kind,omitempty"`
	Path    string          `json:"path,omitempty"`
	Tag     string          `json:"tag,omitempty"`
	Subject string          `json:"subject,omitempty"`
	Images  []pageImage     `json:"images,omitempty"`
	Channel string          `json:"channel"`
	Device  json.RawMessage `json:"device"`
	Source  string          `json:"source"`
	Viewer  int64           `json:"viewer"`
	Expires int64           `json:"expires"`
}

// SetImageKey installs the node key for stateless page grants. An empty key
// keeps the process-local key created on first use.
func (s *ImageGrantService) SetImageKey(key []byte) {
	if s == nil || len(key) == 0 {
		return
	}
	sum := sha256.Sum256(key)
	s.pageMu.Lock()
	s.pageKeyBytes = append([]byte(nil), sum[:]...)
	s.pageMu.Unlock()
}

// SetImageGeneration invalidates cached artwork identities when the catalogue
// or config generation changes. A nil function disables that cache.
func (s *ImageGrantService) SetImageGeneration(gen func() uint64) {
	if s == nil {
		return
	}
	s.pageMu.Lock()
	s.imageGen = gen
	s.pageMu.Unlock()
}

func (s *ImageGrantService) pageKey() []byte {
	s.pageMu.Lock()
	defer s.pageMu.Unlock()
	if len(s.pageKeyBytes) != 32 {
		var fresh [32]byte
		if _, err := rand.Read(fresh[:]); err != nil {
			return nil
		}
		s.pageKeyBytes = fresh[:]
	}
	return append([]byte(nil), s.pageKeyBytes...)
}

// SignPage seals artwork the caller already listed, with one key derivation
// for the page and no grant write. Serve rechecks the credential, expiry,
// revocation and current artwork policy before returning bytes.
func (s *ImageGrantService) SignPage(ctx context.Context, req frontend.ImagePageRequest) ([]frontend.ImageBatchItem, error) {
	if len(req.Titles) > maxImageBatchInput || len(req.Kinds) > 6 {
		return nil, frontend.ErrImageBatchLimit
	}
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = []string{frontend.ImagePoster}
	}
	wanted := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
			wanted[kind] = true
		default:
			return nil, frontend.ErrImageBatchLimit
		}
	}
	viewer, err := s.grants.Resolve(ctx, string(s.surface), req.Credential)
	if err != nil {
		return nil, err
	}
	source, ok := grantHash(req.Credential)
	if !ok {
		return nil, core.ErrUnauthorized
	}
	deviceJSON, err := json.Marshal(req.Device)
	if err != nil || len(deviceJSON) > 4096 {
		return nil, core.ErrUnauthorized
	}
	master := s.pageKey()
	if master == nil {
		return nil, core.ErrNotAvailable
	}
	expires := s.now().Add(s.ttl).UnixMilli()
	reqKey := pageRequestKey(master, viewer.ID, expires, string(s.surface))
	out := make([]frontend.ImageBatchItem, 0, len(req.Titles))
	seen := map[core.RatingKey]bool{}
	for _, title := range req.Titles {
		if title.Key == 0 {
			return nil, frontend.ErrImageBatchLimit
		}
		if seen[title.Key] {
			continue
		}
		seen[title.Key] = true
		if len(seen) > maxImageBatchItems {
			return nil, frontend.ErrImageBatchLimit
		}
		images, complete, err := s.pageImages(ctx, viewer, req.Device, title.Key)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			continue
		}
		if errors.Is(err, core.ErrNotAvailable) {
			out = append(out, frontend.ImageBatchItem{Item: title.Key})
			continue
		}
		if err != nil {
			return nil, err
		}
		item := frontend.ImageBatchItem{Item: title.Key, Authoritative: complete}
		if complete {
			chosen := map[string]bool{}
			var sealed []pageImage
			var granted []frontend.Image
			for _, image := range images {
				if !wanted[image.Kind] || chosen[image.Kind] || image.Path == "" {
					continue
				}
				if len(image.Path) > 1024 || len(image.Kind) > 128 || len(image.Tag) > 256 || len(image.Subject) > 256 {
					return nil, core.ErrNotAvailable
				}
				chosen[image.Kind] = true
				sealed = append(sealed, pageImage{Kind: image.Kind, Path: image.Path, Tag: image.Tag, Subject: image.Subject})
				granted = append(granted, image)
			}
			if len(sealed) > 0 {
				secret, err := sealPageGrant(reqKey, pageClaims{
					Item: int64(title.Key), Images: sealed,
					Kind: sealed[0].Kind, Path: sealed[0].Path, Tag: sealed[0].Tag, Subject: sealed[0].Subject,
					Device: deviceJSON, Source: source, Viewer: viewer.ID, Expires: expires,
				})
				if err != nil {
					return nil, err
				}
				for _, image := range granted {
					item.Images = append(item.Images, frontend.GrantedImage{Image: image, Secret: secret})
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *ImageGrantService) pageImages(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
	s.pageMu.Lock()
	genFn := s.imageGen
	s.pageMu.Unlock()
	gen := uint64(0)
	if genFn != nil {
		gen = genFn()
	}
	cacheKey := pageArtKey{viewer: viewer.ID, item: key, language: device.Language, rating: viewer.MaxContentRating, sections: sectionsKey(viewer.AllowedSections)}
	if genFn != nil {
		s.artMu.Lock()
		if s.art != nil && s.artGen == gen {
			if images, ok := s.art[cacheKey]; ok {
				s.artMu.Unlock()
				return images, true, nil
			}
		}
		s.artMu.Unlock()
	}
	experience := s.experience()
	if experience == nil {
		return nil, false, core.ErrNotAvailable
	}
	var images []frontend.Image
	var complete bool
	var err error
	if scoped, ok := experience.(frontend.ItemArtwork); ok {
		images, complete, err = scoped.ItemArtwork(ctx, viewer, device, key)
	} else {
		var detail frontend.ItemDetail
		detail, err = experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
		images, complete = detail.Images, detail.ImagesAuthoritative
	}
	if err != nil || !complete || genFn == nil {
		return images, complete, err
	}
	s.artMu.Lock()
	if s.art == nil || s.artGen != gen {
		s.art = map[pageArtKey][]frontend.Image{}
		s.artGen = gen
	}
	s.art[cacheKey] = images
	s.artMu.Unlock()
	return images, complete, nil
}

func sectionsKey(sections []int) string {
	if len(sections) == 0 {
		return ""
	}
	b := make([]byte, 0, len(sections)*4)
	var buf [4]byte
	for _, section := range sections {
		binary.BigEndian.PutUint32(buf[:], uint32(section))
		b = append(b, buf[:]...)
	}
	return string(b)
}

func pageRequestKey(master []byte, viewer, expires int64, surface string) []byte {
	mac := hmac.New(sha256.New, master)
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(viewer))
	binary.BigEndian.PutUint64(buf[8:], uint64(expires))
	_, _ = mac.Write(buf[:])
	_, _ = mac.Write([]byte(surface))
	return mac.Sum(nil)
}

func sealPageGrant(reqKey []byte, claims pageClaims) (string, error) {
	block, err := aes.NewCipher(reqKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	body := make([]byte, 16+len(sealed))
	binary.BigEndian.PutUint64(body[:8], uint64(claims.Viewer))
	binary.BigEndian.PutUint64(body[8:16], uint64(claims.Expires))
	copy(body[16:], sealed)
	return pageGrantPrefix + base64.RawURLEncoding.EncodeToString(body), nil
}

func openPageGrant(master []byte, surface, secret string, now int64) (pageClaims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(secret, pageGrantPrefix))
	if err != nil || len(raw) < 16+12+16 {
		return pageClaims{}, core.ErrUnauthorized
	}
	viewer := int64(binary.BigEndian.Uint64(raw[:8]))
	expires := int64(binary.BigEndian.Uint64(raw[8:16]))
	if viewer <= 0 || expires <= now {
		return pageClaims{}, core.ErrUnauthorized
	}
	block, err := aes.NewCipher(pageRequestKey(master, viewer, expires, surface))
	if err != nil {
		return pageClaims{}, core.ErrUnauthorized
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(raw) < 16+gcm.NonceSize() {
		return pageClaims{}, core.ErrUnauthorized
	}
	sealed := raw[16:]
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return pageClaims{}, core.ErrUnauthorized
	}
	var claims pageClaims
	if json.Unmarshal(plain, &claims) != nil || claims.Viewer != viewer || claims.Expires != expires || claims.Source == "" || claims.Item == 0 {
		return pageClaims{}, core.ErrUnauthorized
	}
	if len(claims.Images) == 0 {
		if claims.Kind == "" || claims.Path == "" {
			return pageClaims{}, core.ErrUnauthorized
		}
		return claims, nil
	}
	for _, img := range claims.Images {
		if img.Kind == "" || img.Path == "" || len(img.Path) > 1024 || len(img.Kind) > 128 {
			return pageClaims{}, core.ErrUnauthorized
		}
	}
	return claims, nil
}

func (c pageClaims) imageFor(r *http.Request) (frontend.Image, bool) {
	if len(c.Images) == 0 {
		if c.Kind == "" || c.Path == "" {
			return frontend.Image{}, false
		}
		return frontend.Image{Kind: c.Kind, Path: c.Path, Tag: c.Tag, Subject: c.Subject}, true
	}
	want := urlImageKinds(r)
	if len(want) == 0 {
		img := c.Images[0]
		if img.Kind == "" || img.Path == "" {
			return frontend.Image{}, false
		}
		return frontend.Image{Kind: img.Kind, Path: img.Path, Tag: img.Tag, Subject: img.Subject}, true
	}
	for _, kind := range want {
		for _, img := range c.Images {
			if strings.EqualFold(img.Kind, kind) && img.Path != "" {
				return frontend.Image{Kind: img.Kind, Path: img.Path, Tag: img.Tag, Subject: img.Subject}, true
			}
		}
	}
	return frontend.Image{}, false
}

func urlImageKinds(r *http.Request) []string {
	if r == nil || r.URL == nil {
		return nil
	}
	const marker = "/Images/"
	path := r.URL.Path
	i := strings.LastIndex(path, marker)
	if i < 0 {
		return nil
	}
	rest := path[i+len(marker):]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	switch strings.ToLower(rest) {
	case "primary":
		return []string{frontend.ImagePoster, "primary", frontend.ImageStill}
	case "thumb":
		return []string{frontend.ImageThumb, frontend.ImageStill}
	case "backdrop":
		return []string{frontend.ImageBackdrop}
	case "logo":
		return []string{frontend.ImageLogo}
	case "banner":
		return []string{frontend.ImageBanner}
	default:
		return nil
	}
}

func pageDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *ImageGrantService) servePageGrant(w http.ResponseWriter, r *http.Request, secret string) {
	if _, denied := s.denied.Load(pageDigest(secret)); denied {
		http.NotFound(w, r)
		return
	}
	now := s.now().UnixMilli()
	var revoked int
	err := s.db.QueryRowContext(r.Context(), `SELECT revoked FROM frontend_image_grants WHERE hash=? AND surface=?`, pageDigest(secret), string(s.surface)).Scan(&revoked)
	if err == nil && revoked != 0 {
		http.NotFound(w, r)
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	claims, err := openPageGrant(s.pageKey(), string(s.surface), secret, now)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var live int
	err = s.db.QueryRowContext(r.Context(), `SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.viewer_id=? AND g.surface=? AND g.revoked=0 AND g.expires>? AND u.active=1`, claims.Source, claims.Viewer, string(s.surface), now).Scan(&live)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	viewer, err := s.grants.viewers.ActiveUser(r.Context(), claims.Viewer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var device core.Device
	if json.Unmarshal(claims.Device, &device) != nil {
		http.NotFound(w, r)
		return
	}
	img, ok := claims.imageFor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err = s.authorized(r.Context(), viewer, device, core.RatingKey(claims.Item), claims.Channel, img); err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeGrantedImage(w, r, viewer, device, core.RatingKey(claims.Item), claims.Channel, img, pageDigest(secret), claims.Source, string(claims.Device))
}

func (s *ImageGrantService) revokePageGrant(ctx context.Context, viewerID int64, source, secret string) error {
	claims, err := openPageGrant(s.pageKey(), string(s.surface), secret, s.now().UnixMilli())
	if err != nil || claims.Viewer != viewerID || subtle.ConstantTimeCompare([]byte(claims.Source), []byte(source)) != 1 {
		return core.ErrUnauthorized
	}
	digest := pageDigest(secret)
	s.denied.Store(digest, true)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	row := imageBatchRow{hash: digest, item: core.RatingKey(claims.Item), image: frontend.Image{Kind: claims.Kind, Path: claims.Path, Tag: claims.Tag, Subject: claims.Subject}}
	if err = s.insertAdmitted(ctx, tx, viewerID, source, deviceID(claims.Device), string(claims.Device), claims.Channel, []imageBatchRow{row}); err != nil {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE frontend_image_grants SET revoked=1 WHERE hash=? AND source_hash=? AND viewer_id=? AND surface=?`, digest, source, viewerID, string(s.surface)); err != nil {
		return nil
	}
	_ = tx.Commit()
	return nil
}

func deviceID(raw json.RawMessage) string {
	var device core.Device
	if json.Unmarshal(raw, &device) != nil {
		return ""
	}
	return device.ID
}
