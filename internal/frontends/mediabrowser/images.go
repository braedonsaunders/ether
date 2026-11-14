package mediabrowser

import (
	"context"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strconv"
	"strings"
)

type previewRowArtworkKey struct{}

func withPreviewRowArtwork(r *http.Request, enabled bool) *http.Request {
	if !enabled {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), previewRowArtworkKey{}, true))
}

func previewRowArtwork(r *http.Request, item map[string]any) {
	if enabled, _ := r.Context().Value(previewRowArtworkKey{}).(bool); !enabled {
		return
	}
	backdrops, _ := item["BackdropImageTags"].([]string)
	if len(backdrops) == 0 || backdrops[0] == "" {
		return
	}
	tags := map[string]string{}
	switch old := item["ImageTags"].(type) {
	case map[string]string:
		for k, v := range old {
			tags[k] = v
		}
	case map[string]any:
		for k, v := range old {
			if value, ok := v.(string); ok {
				tags[k] = value
			}
		}
	}
	tags["Primary"] = backdrops[0]
	item["ImageTags"] = tags
	item["PrimaryImageAspectRatio"] = float64(16) / 9
	if old, ok := item["ImageBlurHashes"].(map[string]any); ok {
		hashes := map[string]any{}
		for kind, values := range old {
			if kind != "Primary" {
				hashes[kind] = values
			}
		}
		if byTag, ok := old["Backdrop"].(map[string]string); ok && byTag[backdrops[0]] != "" {
			hashes["Primary"] = map[string]string{backdrops[0]: byTag[backdrops[0]]}
		}
		item["ImageBlurHashes"] = hashes
	}
}

func (i *instance) gridImagesFor(r *http.Request, titles []frontend.Title) map[frontend.RatingKey]map[string]any {
	return i.gridImageRows(r, titles, nil, nil)
}

// gridListImages signs the artwork a library list carries. Like Jellyfin, a
// list carries every image type's tag unless the app narrows them with
// EnableImageTypes (or turns images off): apps build wide rows and the
// home-screen spotlight from BackdropImageTags and ImageTags.Thumb/Logo,
// and request nothing for a type whose tag is missing. Page grants are
// stateless, so the extra kinds cost response bytes, not writes.
func (i *instance) gridListImages(r *http.Request, titles []frontend.Title) map[frontend.RatingKey]map[string]any {
	kinds := listImageKinds(r)
	if kinds != nil && len(kinds) == 0 {
		return map[frontend.RatingKey]map[string]any{}
	}
	return i.gridImageRows(r, titles, nil, kinds)
}

// listImageKinds maps EnableImages/EnableImageTypes to image kinds: nil is
// every kind, an empty slice is none.
func listImageKinds(r *http.Request) []string {
	if v := route.Query(r.URL.Query(), "EnableImages"); len(v) == 1 && strings.EqualFold(v[0], "false") {
		return []string{}
	}
	types := route.Query(r.URL.Query(), "EnableImageTypes")
	if len(types) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var kinds []string
	add := func(k ...string) {
		for _, kind := range k {
			if !seen[kind] {
				seen[kind] = true
				kinds = append(kinds, kind)
			}
		}
	}
	for _, value := range types {
		for _, t := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "primary":
				add(frontend.ImagePoster, frontend.ImageStill)
			case "backdrop":
				add(frontend.ImageBackdrop)
			case "thumb":
				add(frontend.ImageThumb)
			case "logo":
				add(frontend.ImageLogo)
			case "banner":
				add(frontend.ImageBanner)
			}
		}
	}
	if len(kinds) == 0 {
		// Only types Ether has no artwork for: keep the poster.
		return []string{frontend.ImagePoster, frontend.ImageStill}
	}
	return kinds
}

func (i *instance) gridImageRows(r *http.Request, titles []frontend.Title, keys []frontend.RatingKey, kinds []string) map[frontend.RatingKey]map[string]any {
	out := make(map[frontend.RatingKey]map[string]any)
	token := authhdr.MediaBrowserToken(r)
	if len(titles) > 0 {
		if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
			for start := 0; start < len(titles); start += 50 {
				end := min(start+50, len(titles))
				rows, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{Credential: token, Device: device(r), Titles: titles[start:end], Kinds: gridKinds(kinds)})
				if err != nil {
					continue
				}
				i.collectGridImages(out, rows)
			}
			return out
		}
		if len(keys) == 0 {
			keys = make([]frontend.RatingKey, len(titles))
			for n, title := range titles {
				keys[n] = title.Key
			}
		}
	}
	batch, ok := i.host.ImageGrants.(frontend.ImageGrantBatch)
	if !ok {
		return out
	}
	for start := 0; start < len(keys); start += 50 {
		end := min(start+50, len(keys))
		rows, err := batch.IssueBatch(r.Context(), frontend.ImageBatchRequest{Credential: token, Device: device(r), Items: keys[start:end], Kinds: gridKinds(kinds)})
		if err != nil {
			continue
		}
		i.collectGridImages(out, rows)
	}
	return out
}

func gridKinds(kinds []string) []string {
	if len(kinds) > 0 {
		return kinds
	}
	return []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner}
}

func (i *instance) collectGridImages(out map[frontend.RatingKey]map[string]any, rows []frontend.ImageBatchItem) {
	for _, row := range rows {
		if !row.Authoritative {
			continue
		}
		hasPoster := false
		for _, image := range row.Images {
			if image.Image.Kind == frontend.ImagePoster && image.Secret != "" {
				hasPoster = true
				break
			}
		}
		tags := map[string]string{}
		backdrops := []string{}
		result := map[string]any{}
		primarySelected := false
		for _, img := range row.Images {
			if img.Secret == "" {
				continue
			}
			switch img.Image.Kind {
			case frontend.ImagePoster:
				tags["Primary"] = img.Secret
				primarySelected = true
				i.addImageMetadata(result, "Primary", img.Secret, img.Image)
			case frontend.ImageStill:
				if !primarySelected && !hasPoster {
					tags["Primary"] = img.Secret
					primarySelected = true
					i.addImageMetadata(result, "Primary", img.Secret, img.Image)
				}
			case frontend.ImageBackdrop:
				backdrops = append(backdrops, img.Secret)
				i.addImageMetadata(result, "Backdrop", img.Secret, img.Image)
			case frontend.ImageLogo:
				tags["Logo"] = img.Secret
				i.addImageMetadata(result, "Logo", img.Secret, img.Image)
			case frontend.ImageThumb:
				tags["Thumb"] = img.Secret
				i.addImageMetadata(result, "Thumb", img.Secret, img.Image)
			case frontend.ImageBanner:
				tags["Banner"] = img.Secret
				i.addImageMetadata(result, "Banner", img.Secret, img.Image)
			}
		}
		if len(tags) > 0 {
			result["ImageTags"] = tags
		}
		if len(backdrops) > 0 {
			result["BackdropImageTags"] = backdrops
		}
		out[row.Item] = result
	}
}

func (i *instance) imageTags(r *http.Request, detail frontend.ItemDetail, token string) (map[string]any, []string, map[string]any) {
	tags := map[string]any{}
	backdrops := []string{}
	metadata := map[string]any{}
	if i.host.ImageGrants == nil || !detail.ImagesAuthoritative {
		return tags, backdrops, metadata
	}
	if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
		rows, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{
			Credential: token, Device: device(r), Titles: []frontend.Title{detail.Title},
			Kinds: []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner},
		})
		if err == nil && len(rows) > 0 && rows[0].Authoritative && rows[0].Item == detail.Title.Key {
			images := make([]frontend.Image, len(rows[0].Images))
			secrets := make([]string, len(rows[0].Images))
			for n, granted := range rows[0].Images {
				images[n] = granted.Image
				secrets[n] = granted.Secret
			}
			tags, backdrops, metadata = i.tagGrantedImages(images, secrets)
			return tags, backdrops, metadata
		}
	}
	hasPoster := false
	for _, img := range detail.Images {
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			hasPoster = true
		}
	}
	primarySelected := false
	for _, img := range detail.Images {
		kind := ""
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			kind = "Primary"
		case "logo":
			kind = "Logo"
		case "thumb":
			kind = "Thumb"
		case "still":
			// An episode still is the primary image when the title has no poster.
			if hasPoster || primarySelected {
				kind = "Thumb"
			} else {
				kind = "Primary"
			}
		case "banner":
			kind = "Banner"
		case "backdrop":
			kind = "Backdrop"
		}
		if kind == "" {
			continue
		}
		if _, exists := tags[kind]; exists && kind != "Backdrop" {
			continue
		}
		grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: detail.Title.Key, Image: img})
		if err == nil && grant != "" {
			if kind == "Backdrop" {
				backdrops = append(backdrops, grant)
			} else {
				tags[kind] = grant
			}
			if kind == "Primary" {
				primarySelected = true
			}
			i.addImageMetadata(metadata, kind, grant, img)
		}
	}
	return tags, backdrops, metadata
}

func (i *instance) tagGrantedImages(images []frontend.Image, secrets []string) (map[string]any, []string, map[string]any) {
	tags := map[string]any{}
	backdrops := []string{}
	metadata := map[string]any{}
	hasPoster := false
	for _, img := range images {
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			hasPoster = true
		}
	}
	primarySelected := false
	for n, img := range images {
		kind := ""
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			kind = "Primary"
		case "logo":
			kind = "Logo"
		case "thumb":
			kind = "Thumb"
		case "still":
			if hasPoster || primarySelected {
				kind = "Thumb"
			} else {
				kind = "Primary"
			}
		case "banner":
			kind = "Banner"
		case "backdrop":
			kind = "Backdrop"
		}
		if kind == "" || n >= len(secrets) || secrets[n] == "" {
			continue
		}
		if _, exists := tags[kind]; exists && kind != "Backdrop" {
			continue
		}
		if kind == "Backdrop" {
			backdrops = append(backdrops, secrets[n])
		} else {
			tags[kind] = secrets[n]
		}
		if kind == "Primary" {
			primarySelected = true
		}
		i.addImageMetadata(metadata, kind, secrets[n], img)
	}
	return tags, backdrops, metadata
}

// addImageMetadata attaches facts only after the matching grant was issued.
func (i *instance) addImageMetadata(dto map[string]any, kind, tag string, img frontend.Image) {
	if tag == "" {
		return
	}
	if kind == "Primary" && img.Width > 0 && img.Height > 0 {
		dto["PrimaryImageAspectRatio"] = float64(img.Width) / float64(img.Height)
	}
	if i.protocol != "jellyfin" || img.BlurHash == "" {
		return
	}
	hashes, _ := dto["ImageBlurHashes"].(map[string]any)
	if hashes == nil {
		hashes = map[string]any{}
		dto["ImageBlurHashes"] = hashes
	}
	byTag, _ := hashes[kind].(map[string]string)
	if byTag == nil {
		byTag = map[string]string{}
		hashes[kind] = byTag
	}
	byTag[tag] = img.BlurHash
}
func (i *instance) image(w http.ResponseWriter, r *http.Request) {
	if i.host.ImageGrants == nil {
		http.NotFound(w, r)
		return
	}
	tags := route.Query(r.URL.Query(), "tag")
	if len(tags) == 1 && tags[0] != "" {
		i.host.ImageGrants.Serve(w, r, tags[0])
		return
	}
	if len(tags) > 1 {
		http.NotFound(w, r)
		return
	}
	// Jellyfin treats tag as an optional cache-buster, and clients such as
	// Moonfin omit it (#405). Serve those requests in serveTaglessImage.
	i.serveTaglessImage(w, r)
}

// serveTaglessImage answers an image request with no tag. A tag is a
// short-lived capability minted for one viewer after that viewer was
// shown the item. Two tag-less shapes exist in the wild:
//
//   - The request carries the client's own live credential (header or
//     api_key). Resolve the item and image exactly as that viewer, then
//     mint the same scoped grant in-process and serve through it. Grant
//     Issue and Serve re-authorize against current policy, so this path
//     only returns artwork the credential holder could already have
//     obtained a tag for.
//   - The request is fully anonymous. Some clients (Moonfin on Smart
//     TVs) build image URLs with no tag and load them with a plain
//     <img>, so no credential can ever arrive; Jellyfin itself serves
//     these. Ether serves them only via servePublicImage: title artwork
//     for titles every active viewer may see. Anything hidden from
//     anyone — and every unresolvable id — stays the same 404, so the
//     anonymous path is not an oracle for restricted titles.
//
// A tag that is present but invalid never reaches here: it goes to
// Serve and 404s there, so a forged tag cannot fall through to either
// tag-less path.
func (i *instance) serveTaglessImage(w http.ResponseWriter, r *http.Request) {
	wireID, imageType := r.PathValue("itemId"), r.PathValue("imageType")
	index := 0
	if raw := r.PathValue("index"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			http.NotFound(w, r)
			return
		}
		index = n
	}
	v, token, err := i.authenticate(r)
	if err != nil || v == nil {
		i.servePublicImage(w, r, wireID, imageType, index)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{viewer: v, token: token}))
	if v, token, err = i.viewer(r); err != nil || v == nil {
		http.NotFound(w, r)
		return
	}
	primaryOnly := strings.EqualFold(imageType, "Primary") && index == 0
	if _, err := ids.ParseGUID(ids.Channel, wireID); err == nil {
		if !primaryOnly || i.host.LiveTV == nil {
			http.NotFound(w, r)
			return
		}
		ch, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, wireID)
		if err != nil || ch.ID == "" {
			http.NotFound(w, r)
			return
		}
		for _, img := range ch.Images {
			if img.Kind != frontend.ImageChannel {
				continue
			}
			grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Channel: ch.ID, Image: img})
			if err == nil && grant != "" {
				i.host.ImageGrants.Serve(w, r, grant)
				return
			}
			break
		}
		http.NotFound(w, r)
		return
	}
	if viewNum, err := ids.ParseGUID(ids.View, wireID); err == nil {
		if !primaryOnly {
			http.NotFound(w, r)
			return
		}
		secret, _, ok := i.libraryCollageGrant(r, v, viewNum, wireID)
		if !ok || secret == "" {
			http.NotFound(w, r)
			return
		}
		i.host.ImageGrants.Serve(w, r, secret)
		return
	}
	key, err := ids.ParseRatingKeyGUID(wireID)
	if err != nil || i.host.Experience == nil {
		http.NotFound(w, r)
		return
	}
	detail, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
	if err != nil || !detail.ImagesAuthoritative {
		http.NotFound(w, r)
		return
	}
	img, ok := taglessImage(detail.Images, imageType, index)
	if !ok {
		http.NotFound(w, r)
		return
	}
	item := detail.Title.Key
	if item == 0 {
		item = key
	}
	grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: item, Image: img})
	if err != nil || grant == "" {
		http.NotFound(w, r)
		return
	}
	i.host.ImageGrants.Serve(w, r, grant)
}

// servePublicImage serves title artwork to a fully anonymous tag-less
// request. Only plain title items are eligible (person, channel, user
// and library-view images stay grant-only), the title must be visible
// to every active viewer (frontend.PublicArtwork), and the bytes are
// served through the grant service's public path, which re-checks all
// of that before writing (frontend.PublicImageServer).
func (i *instance) servePublicImage(w http.ResponseWriter, r *http.Request, wireID, imageType string, index int) {
	public, experienceOK := i.host.Experience.(frontend.PublicArtwork)
	server, grantsOK := i.host.ImageGrants.(frontend.PublicImageServer)
	if !experienceOK || !grantsOK {
		http.NotFound(w, r)
		return
	}
	key, err := ids.ParseRatingKeyGUID(wireID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	images, _, visible, err := public.PublicArtwork(i.identityContext(r), device(r), key)
	if err != nil || !visible {
		http.NotFound(w, r)
		return
	}
	img, ok := taglessImage(images, imageType, index)
	if !ok {
		http.NotFound(w, r)
		return
	}
	server.ServePublic(w, r, device(r), key, img)
}

// taglessImage picks the index-th descriptor for a Jellyfin/Emby image
// type, in the same priority imageTags uses when it mints tags: a poster
// is Primary, an episode still stands in when there is no poster, and a
// still is also the Thumb fallback.
func taglessImage(images []frontend.Image, imageType string, index int) (frontend.Image, bool) {
	var kinds []string
	switch strings.ToLower(imageType) {
	case "primary":
		kinds = []string{frontend.ImagePoster, "primary", frontend.ImageStill}
	case "art", "backdrop":
		kinds = []string{frontend.ImageBackdrop}
	case "logo":
		kinds = []string{frontend.ImageLogo}
	case "thumb":
		kinds = []string{frontend.ImageThumb, frontend.ImageStill}
	case "banner":
		kinds = []string{frontend.ImageBanner}
	default:
		return frontend.Image{}, false
	}
	var candidates []frontend.Image
	for _, kind := range kinds {
		for _, img := range images {
			if strings.EqualFold(img.Kind, kind) && img.Path != "" {
				candidates = append(candidates, img)
			}
		}
	}
	if index >= len(candidates) {
		return frontend.Image{}, false
	}
	return candidates[index], true
}

// mediaBrowserError uses the ProblemDetails shape returned by Jellyfin.
func mediaBrowserError(w http.ResponseWriter, r *http.Request, status int) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status})
	}
}

// itemImages advertises only artwork with the same scoped tags as the item DTO.
func (i *instance) itemImages(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		mediaBrowserError(w, r, 401)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("itemId"))
	if err != nil {
		mediaBrowserError(w, r, 404)
		return
	}
	if i.host.Experience == nil {
		mediaBrowserError(w, r, 503)
		return
	}
	detail, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
	if err != nil {
		mediaBrowserError(w, r, 404)
		return
	}
	tags, backdrops, _ := i.imageTags(r, detail, token)
	images := []map[string]any{}
	for _, kind := range []string{"Primary", "Logo", "Thumb", "Banner"} {
		if tag, ok := tags[kind].(string); ok && tag != "" {
			images = append(images, map[string]any{"ImageType": kind, "ImageTag": tag, "Size": 0})
		}
	}
	for index, tag := range backdrops {
		images = append(images, map[string]any{"ImageType": "Backdrop", "ImageIndex": index, "ImageTag": tag, "Size": 0})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(images)
}
