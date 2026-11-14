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
	"fmt"
	"github.com/braedonsaunders/ether/internal/reqmemo"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
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
	Gen     uint64          `json:"gen,omitempty"`
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
	// A page grant writes nothing per title, so one call signs a whole
	// listing page (an export's 250), up to the input bound.
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
	source, ok := grantHash(req.Credential)
	if !ok {
		return nil, core.ErrUnauthorized
	}
	// A listing signs its rows 50 at a time; within one read request the
	// credential is resolved once (reqmemo's short window), not per batch.
	viewer, err := reqmemo.Do(ctx, "image-page-grant:"+string(s.surface)+":"+source, func() (*core.Viewer, error) {
		return s.grants.Resolve(ctx, string(s.surface), req.Credential)
	})
	if err != nil {
		return nil, err
	}
	deviceJSON, err := json.Marshal(req.Device)
	if err != nil || len(deviceJSON) > 4096 {
		return nil, core.ErrUnauthorized
	}
	master := s.pageKey()
	if master == nil {
		return nil, core.ErrNotAvailable
	}
	expires := s.pageExpires()
	// Page grants support only title artwork, so authorize with visibility
	// rows just as IssueBatch does, without loading credited portraits.
	ctx = frontend.WithArtworkRows(ctx)
	experience := s.experience()
	if page, ok := experience.(frontend.ItemArtworkPage); ok {
		keys := make([]core.RatingKey, 0, len(req.Titles))
		seen := map[core.RatingKey]bool{}
		for _, title := range req.Titles {
			if title.Key == 0 || seen[title.Key] {
				continue
			}
			seen[title.Key] = true
			keys = append(keys, title.Key)
		}
		// Only the wanted kinds are sealed below, so the listing need not
		// prepare the others (UltraBlur colours for a poster-only page).
		listed, err := page.ItemArtworkPage(frontend.WithArtworkKinds(ctx, wanted), viewer, req.Device, keys)
		if err != nil {
			return nil, err
		}
		ctx = s.withCheckedTags(ctx, master, listed, wanted)
		out := make([]frontend.ImageBatchItem, 0, len(keys))
		for _, key := range keys {
			images, ok := listed[key]
			if !ok {
				continue
			}
			item, err := s.sealPageImages(ctx, master, source, deviceJSON, viewer.ID, expires, key, images, true, wanted)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	}
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
		item, err := s.sealPageImages(ctx, master, source, deviceJSON, viewer.ID, expires, title.Key, images, complete, wanted)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// RenewPage gives tags SignPage sealed earlier a full lifetime for
// req.Credential, as signing the page again would, without listing its
// artwork again. A tag names the image it was sealed for; Serve still
// rechecks the credential, revocation and the viewer's artwork policy, so a
// renewal can only keep serving what that viewer may already see.
func (s *ImageGrantService) RenewPage(ctx context.Context, req frontend.ImagePageRenewal) error {
	source, ok := grantHash(req.Credential)
	if !ok {
		return core.ErrUnauthorized
	}
	viewer, err := reqmemo.Do(ctx, "image-page-grant:"+string(s.surface)+":"+source, func() (*core.Viewer, error) {
		return s.grants.Resolve(ctx, string(s.surface), req.Credential)
	})
	if err != nil {
		return err
	}
	if viewer == nil {
		return core.ErrUnauthorized
	}
	deviceJSON, err := json.Marshal(req.Device)
	if err != nil || len(deviceJSON) > 4096 {
		return core.ErrUnauthorized
	}
	expires, gen := s.pageExpires(), s.Generation()
	s.shortMu.Lock()
	defer s.shortMu.Unlock()
	if s.short == nil {
		s.short = map[string]pageClaims{}
	}
	for _, item := range req.Items {
		if item.Item == 0 {
			continue
		}
		for _, g := range item.Images {
			img := g.Image
			if !isShortImageTag(g.Secret) || img.Kind == "" || img.Path == "" || len(img.Path) > 1024 || len(img.Kind) > 128 || len(img.Tag) > 256 || len(img.Subject) > 256 {
				continue
			}
			s.short[g.Secret] = pageClaims{
				Item: int64(item.Item), Kind: img.Kind, Path: img.Path, Tag: img.Tag, Subject: img.Subject,
				Device: deviceJSON, Source: source, Viewer: viewer.ID, Expires: expires, Gen: gen,
			}
		}
	}
	return nil
}

func (s *ImageGrantService) sealPageImages(ctx context.Context, master []byte, source string, deviceJSON []byte, viewer int64, expires int64, key core.RatingKey, images []frontend.Image, complete bool, wanted map[string]bool) (frontend.ImageBatchItem, error) {
	item := frontend.ImageBatchItem{Item: key, Authoritative: complete}
	if !complete {
		return item, nil
	}
	chosen := map[string]bool{}
	for _, image := range images {
		if !wanted[image.Kind] || chosen[image.Kind] || image.Path == "" {
			continue
		}
		if len(image.Path) > 1024 || len(image.Kind) > 128 || len(image.Tag) > 256 || len(image.Subject) > 256 {
			return frontend.ImageBatchItem{}, core.ErrNotAvailable
		}
		chosen[image.Kind] = true
		img := pageImage{Kind: image.Kind, Path: image.Path, Tag: image.Tag, Subject: image.Subject}
		secret := s.publicImageTag(ctx, master, int64(key), img)
		s.rememberShort(secret, pageClaims{
			Item: int64(key), Kind: img.Kind, Path: img.Path, Tag: img.Tag, Subject: img.Subject,
			Device: deviceJSON, Source: source, Viewer: viewer, Expires: expires, Gen: s.Generation(),
		})
		item.Images = append(item.Images, frontend.GrantedImage{Image: image, Secret: secret})
	}
	return item, nil
}

func (s *ImageGrantService) pageImages(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) ([]frontend.Image, bool, error) {
	s.pageMu.Lock()
	genFn := s.imageGen
	s.pageMu.Unlock()
	gen := uint64(0)
	if genFn != nil {
		gen = genFn()
	}
	cacheKey := pageArtKey{viewer: viewer.ID, item: key, language: device.Language, rating: viewer.MaxContentRating, sections: sectionsKey(viewer.AllowedLibraries)}
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

func sectionsKey(sections []core.LibraryID) string {
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

// pageExpires keeps one image tag stable inside a minute so clients can cache
// it, and moves the tag when that window or a revocation generation changes.
func (s *ImageGrantService) pageExpires() int64 {
	ttl := s.ttl.Milliseconds()
	if ttl < 1000 {
		ttl = 1000
	}
	now := s.now().UnixMilli()
	step := int64(time.Minute / time.Millisecond)
	if step > ttl {
		step = ttl
	}
	return now - (now % step) + ttl
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

func isShortImageTag(secret string) bool {
	if len(secret) != 32 {
		return false
	}
	for _, c := range secret {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func contentImageTag(key []byte, item int64, img pageImage, extra uint64) string {
	mac := hmac.New(sha256.New, key)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(item))
	_, _ = mac.Write(buf[:])
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(img.Kind))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(img.Path))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(img.Tag))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(img.Subject))
	if extra != 0 {
		binary.BigEndian.PutUint64(buf[:], extra)
		_, _ = mac.Write(buf[:])
	}
	sum := mac.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

func (s *ImageGrantService) publicImageTag(ctx context.Context, key []byte, item int64, img pageImage) string {
	tag := contentImageTag(key, item, img, 0)
	if s.tagDenied(ctx, tag) {
		extra := s.Generation()
		if extra == 0 {
			extra = 1
		}
		tag = contentImageTag(key, item, img, extra)
	}
	return tag
}

type checkedTagsKey struct{}

// withCheckedTags reads, in one query, which of a page's image tags were
// revoked, so sealing the page does not ask once per image.
func (s *ImageGrantService) withCheckedTags(ctx context.Context, master []byte, listed map[core.RatingKey][]frontend.Image, wanted map[string]bool) context.Context {
	checked := map[string]bool{}
	var digests []any
	for key, images := range listed {
		chosen := map[string]bool{}
		for _, image := range images {
			if !wanted[image.Kind] || chosen[image.Kind] || image.Path == "" {
				continue
			}
			chosen[image.Kind] = true
			digest := pageDigest(contentImageTag(master, int64(key), pageImage{Kind: image.Kind, Path: image.Path, Tag: image.Tag, Subject: image.Subject}, 0))
			if _, ok := checked[digest]; !ok {
				checked[digest] = false
				digests = append(digests, digest)
			}
		}
	}
	for start := 0; start < len(digests); start += 400 {
		part := digests[start:min(start+400, len(digests))]
		rows, err := s.db.QueryContext(ctx, `SELECT hash FROM frontend_image_grants WHERE surface=? AND revoked<>0 AND hash IN (?`+strings.Repeat(",?", len(part)-1)+`)`, append([]any{string(s.surface)}, part...)...)
		if err != nil {
			return ctx
		}
		for rows.Next() {
			var digest string
			if rows.Scan(&digest) == nil {
				checked[digest] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ctx
		}
	}
	return context.WithValue(ctx, checkedTagsKey{}, checked)
}

func (s *ImageGrantService) tagDenied(ctx context.Context, tag string) bool {
	digest := pageDigest(tag)
	if s.isDenied(digest) {
		return true
	}
	if checked, ok := ctx.Value(checkedTagsKey{}).(map[string]bool); ok {
		if denied, ok := checked[digest]; ok {
			if denied {
				s.deny(digest, 0)
			}
			return denied
		}
	}
	var revoked int
	err := s.db.QueryRowContext(ctx, `SELECT revoked FROM frontend_image_grants WHERE hash=? AND surface=?`, digest, string(s.surface)).Scan(&revoked)
	if err == nil && revoked != 0 {
		s.deny(digest, 0)
		return true
	}
	return false
}

func (s *ImageGrantService) rememberShort(tag string, claims pageClaims) {
	s.shortMu.Lock()
	if s.short == nil {
		s.short = map[string]pageClaims{}
	}
	s.short[tag] = claims
	s.shortMu.Unlock()
}

func (s *ImageGrantService) loadShort(tag string) (pageClaims, bool) {
	s.shortMu.Lock()
	claims, ok := s.short[tag]
	s.shortMu.Unlock()
	return claims, ok
}

func (s *ImageGrantService) forgetShort(tag string) {
	s.shortMu.Lock()
	delete(s.short, tag)
	s.shortMu.Unlock()
}

func (s *ImageGrantService) serveShortTag(w http.ResponseWriter, r *http.Request, secret string) {
	if s.tagDenied(r.Context(), secret) {
		http.NotFound(w, r)
		return
	}
	// Viewer grants retain their policy and revocation checks even without a
	// token on the image request. Public content tags are an additional path.
	if claims, ok := s.loadShort(secret); ok && claims.Expires > s.now().UnixMilli() {
		if !shortClaimMatches(claims, r) {
			http.NotFound(w, r)
			return
		}
		s.serveClaims(w, r, secret, claims, s.now().UnixMilli())
		return
	}
	// Tokenless cached URLs are content identities, not expiring viewer grants.
	if s.requestOptions.PublicContentTags && s.requestToken(r) == "" {
		key, ok := imageItemKey(r.URL.Path)
		public, supported := s.experience().(frontend.PublicArtwork)
		if ok && supported {
			images, _, visible, err := public.PublicArtwork(r.Context(), core.Device{}, key)
			if err == nil && visible {
				master := s.pageKey()
				for _, image := range images {
					img := pageImage{Kind: image.Kind, Path: image.Path, Tag: image.Tag, Subject: image.Subject}
					if shortClaimMatches(pageClaims{Kind: img.Kind, Path: img.Path}, r) && (contentImageTag(master, int64(key), img, 0) == secret || contentImageTag(master, int64(key), img, s.Generation()) == secret) {
						s.ServePublic(w, r, core.Device{}, key, image)
						return
					}
				}
			}
		}
		http.NotFound(w, r)
		return
	}
	now := s.now().UnixMilli()
	claims, ok := s.loadShort(secret)
	if ok && claims.Expires <= now {
		ok = false
	}
	if !ok {
		claims, ok = s.resolveShortTag(r, secret)
	}
	if !ok || !shortClaimMatches(claims, r) {
		http.NotFound(w, r)
		return
	}
	s.serveClaims(w, r, secret, claims, now)
}

func shortClaimMatches(claims pageClaims, r *http.Request) bool {
	if claims.Kind == "" || claims.Path == "" {
		return false
	}
	want := urlImageKinds(r)
	if len(want) == 0 {
		return true
	}
	for _, kind := range want {
		if strings.EqualFold(kind, claims.Kind) {
			return true
		}
	}
	return false
}

func (s *ImageGrantService) resolveShortTag(r *http.Request, secret string) (pageClaims, bool) {
	if r == nil || r.URL == nil {
		return pageClaims{}, false
	}
	token := s.requestToken(r)
	key, ok := imageItemKey(r.URL.Path)
	if token == "" || !ok {
		return pageClaims{}, false
	}
	viewer, err := s.grants.Resolve(r.Context(), string(s.surface), token)
	if err != nil || viewer == nil {
		return pageClaims{}, false
	}
	source, ok := grantHash(token)
	if !ok {
		return pageClaims{}, false
	}
	images, complete, err := s.pageImages(r.Context(), viewer, core.Device{}, key)
	if err != nil || !complete {
		return pageClaims{}, false
	}
	master := s.pageKey()
	for _, image := range images {
		img := pageImage{Kind: image.Kind, Path: image.Path, Tag: image.Tag, Subject: image.Subject}
		if contentImageTag(master, int64(key), img, 0) != secret && contentImageTag(master, int64(key), img, s.Generation()) != secret {
			continue
		}
		deviceJSON, err := json.Marshal(core.Device{})
		if err != nil {
			return pageClaims{}, false
		}
		return pageClaims{
			Item: int64(key), Kind: img.Kind, Path: img.Path, Tag: img.Tag, Subject: img.Subject,
			Device: deviceJSON, Source: source, Viewer: viewer.ID, Expires: s.pageExpires(),
		}, true
	}
	return pageClaims{}, false
}

func imageItemKey(path string) (core.RatingKey, bool) {
	const marker = "/Items/"
	i := strings.Index(path, marker)
	if i < 0 {
		return 0, false
	}
	rest := path[i+len(marker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return 0, false
	}
	key, err := ids.ParseRatingKeyGUID(rest[:slash])
	if err != nil {
		return 0, false
	}
	return key, true
}

func (s *ImageGrantService) servePageGrant(w http.ResponseWriter, r *http.Request, secret string) {
	if s.isDenied(pageDigest(secret)) {
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
	s.serveClaims(w, r, secret, claims, now)
}

func (s *ImageGrantService) serveClaims(w http.ResponseWriter, r *http.Request, secret string, claims pageClaims, now int64) {
	var live int
	err := s.db.QueryRowContext(r.Context(), `SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=? AND g.viewer_id=? AND g.surface=? AND g.revoked=0 AND g.expires>? AND u.active=1`, claims.Source, claims.Viewer, string(s.surface), now).Scan(&live)
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
		img = frontend.Image{Kind: claims.Kind, Path: claims.Path, Tag: claims.Tag, Subject: claims.Subject}
		if img.Kind == "" || img.Path == "" {
			http.NotFound(w, r)
			return
		}
	}
	r = artworkRowsRequest(r, img)
	if err = s.authorized(r.Context(), viewer, device, core.RatingKey(claims.Item), claims.Channel, img); err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeGrantedImage(w, r, viewer, device, core.RatingKey(claims.Item), claims.Channel, img, pageDigest(secret), claims.Source, string(claims.Device))
}

// revokeShortGrant and revokePageGrant deny the grant in memory at once
// (this node stops serving it even if persisting fails) and then persist the
// revocation, so it survives a restart and reaches the other nodes. A failed
// persist is returned: the owner must know the revocation is node-local.
func (s *ImageGrantService) revokeShortGrant(ctx context.Context, viewerID int64, source, secret string) error {
	claims, ok := s.loadShort(secret)
	if !ok || claims.Viewer != viewerID || subtle.ConstantTimeCompare([]byte(claims.Source), []byte(source)) != 1 {
		return core.ErrUnauthorized
	}
	s.forgetShort(secret)
	return s.persistRevocation(ctx, viewerID, source, pageDigest(secret), claims)
}

func (s *ImageGrantService) revokePageGrant(ctx context.Context, viewerID int64, source, secret string) error {
	claims, err := openPageGrant(s.pageKey(), string(s.surface), secret, s.now().UnixMilli())
	if err != nil || claims.Viewer != viewerID || subtle.ConstantTimeCompare([]byte(claims.Source), []byte(source)) != 1 {
		return core.ErrUnauthorized
	}
	return s.persistRevocation(ctx, viewerID, source, pageDigest(secret), claims)
}

func (s *ImageGrantService) persistRevocation(ctx context.Context, viewerID int64, source, digest string, claims pageClaims) error {
	s.deny(digest, claims.Expires)
	s.gen.Add(1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: persist image grant revocation: %w", err)
	}
	defer tx.Rollback()
	row := imageBatchRow{hash: digest, item: core.RatingKey(claims.Item), image: frontend.Image{Kind: claims.Kind, Path: claims.Path, Tag: claims.Tag, Subject: claims.Subject}}
	if err = s.insertAdmitted(ctx, tx, viewerID, source, deviceID(claims.Device), string(claims.Device), claims.Channel, []imageBatchRow{row}); err != nil {
		return fmt.Errorf("identity: persist image grant revocation: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE frontend_image_grants SET revoked=1 WHERE hash=? AND source_hash=? AND viewer_id=? AND surface=?`, digest, source, viewerID, string(s.surface)); err != nil {
		return fmt.Errorf("identity: persist image grant revocation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("identity: persist image grant revocation: %w", err)
	}
	return nil
}

// deny records a revoked grant digest in memory until untilMs (Unix
// milliseconds), after which the grant has expired anyway or the persisted
// revoked row answers. Expired entries are swept at most once per TTL.
func (s *ImageGrantService) deny(digest string, untilMs int64) {
	now := s.now().UnixMilli()
	if floor := now + s.ttl.Milliseconds(); untilMs < floor {
		untilMs = floor
	}
	s.denied.Store(digest, untilMs)
	if last := s.deniedSwept.Load(); now-last >= s.ttl.Milliseconds() && s.deniedSwept.CompareAndSwap(last, now) {
		s.denied.Range(func(k, v any) bool {
			if until, _ := v.(int64); until <= now {
				s.denied.CompareAndDelete(k, v)
			}
			return true
		})
	}
}

// isDenied reports whether digest is revoked in memory.
func (s *ImageGrantService) isDenied(digest string) bool {
	v, ok := s.denied.Load(digest)
	if !ok {
		return false
	}
	if until, _ := v.(int64); until <= s.now().UnixMilli() {
		s.denied.CompareAndDelete(digest, v)
		return false
	}
	return true
}

func deviceID(raw json.RawMessage) string {
	var device core.Device
	if json.Unmarshal(raw, &device) != nil {
		return ""
	}
	return device.ID
}

func (s *ImageGrantService) requestToken(r *http.Request) string {
	if s.requestOptions.Token == nil {
		return ""
	}
	return s.requestOptions.Token(r)
}
