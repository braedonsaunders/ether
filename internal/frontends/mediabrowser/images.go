package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strings"
)

// gridImages issues bounded, policy-checked grants without loading item
// details or resolving playback versions. An unavailable batch is omitted.
func (i *instance) gridImages(r *http.Request, keys []frontend.RatingKey) map[frontend.RatingKey]map[string]any {
	return i.gridImageRows(r, nil, keys)
}

func (i *instance) gridImagesFor(r *http.Request, titles []frontend.Title) map[frontend.RatingKey]map[string]any {
	return i.gridImageRows(r, titles, nil)
}

func (i *instance) gridImageRows(r *http.Request, titles []frontend.Title, keys []frontend.RatingKey) map[frontend.RatingKey]map[string]any {
	out := make(map[frontend.RatingKey]map[string]any)
	token := authhdr.MediaBrowserToken(r)
	if len(titles) > 0 {
		if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
			for start := 0; start < len(titles); start += 50 {
				end := min(start+50, len(titles))
				rows, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{Credential: token, Device: device(r), Titles: titles[start:end], Kinds: []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner}})
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
		rows, err := batch.IssueBatch(r.Context(), frontend.ImageBatchRequest{Credential: token, Device: device(r), Items: keys[start:end], Kinds: []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner}})
		if err != nil {
			continue
		}
		i.collectGridImages(out, rows)
	}
	return out
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
	for _, img := range detail.Images {
		kind := ""
		switch strings.ToLower(img.Kind) {
		case "poster", "primary":
			kind = "Primary"
		case "logo":
			kind = "Logo"
		case "thumb", "still":
			kind = "Thumb"
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
			i.addImageMetadata(metadata, kind, grant, img)
		}
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
	if len(tags) != 1 || tags[0] == "" {
		http.NotFound(w, r)
		return
	}
	i.host.ImageGrants.Serve(w, r, tags[0])
}
