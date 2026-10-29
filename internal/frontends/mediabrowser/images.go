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
	out := make(map[frontend.RatingKey]map[string]any)
	batch, ok := i.host.ImageGrants.(frontend.ImageGrantBatch)
	if !ok {
		return out
	}
	token := authhdr.MediaBrowserToken(r)
	for start := 0; start < len(keys); start += 50 {
		end := min(start+50, len(keys))
		rows, err := batch.IssueBatch(r.Context(), frontend.ImageBatchRequest{Credential: token, Device: device(r), Items: keys[start:end], Kinds: []string{frontend.ImagePoster, frontend.ImageStill, frontend.ImageBackdrop}})
		if err != nil {
			continue
		}
		for _, row := range rows {
			if !row.Authoritative {
				continue
			}
			tags := map[string]string{}
			backdrops := []string{}
			for _, img := range row.Images {
				if img.Secret == "" {
					continue
				}
				switch img.Image.Kind {
				case frontend.ImagePoster:
					tags["Primary"] = img.Secret
				case frontend.ImageStill:
					if tags["Primary"] == "" {
						tags["Primary"] = img.Secret
					}
				case frontend.ImageBackdrop:
					backdrops = append(backdrops, img.Secret)
				}
			}
			result := map[string]any{}
			if len(tags) > 0 {
				result["ImageTags"] = tags
			}
			if len(backdrops) > 0 {
				result["BackdropImageTags"] = backdrops
			}
			out[row.Item] = result
		}
	}
	return out
}

func (i *instance) imageTags(r *http.Request, detail frontend.ItemDetail, token string) (map[string]any, []string) {
	tags := map[string]any{}
	backdrops := []string{}
	if i.host.ImageGrants == nil || !detail.ImagesAuthoritative {
		return tags, backdrops
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
		}
	}
	return tags, backdrops
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
