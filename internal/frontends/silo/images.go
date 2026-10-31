package silo

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type creditPortrait struct{ url, thumbHash string }

// creditPortraits returns title-bound grants for known credited people.
func (i *instance) creditPortraits(r *http.Request, viewer *frontend.Viewer, credential string, d frontend.ItemDetail) map[int64]creditPortrait {
	urls := map[int64]creditPortrait{}
	if i.host.ImageGrants == nil {
		return urls
	}
	images, authoritative := d.Images, d.ImagesAuthoritative
	if provider, ok := i.host.Experience.(frontend.RelatedArtworkProvider); ok {
		if related, err := provider.RelatedArtwork(r.Context(), viewer, device(r), d.Title.Key); err == nil && related.PeopleAuthoritative {
			images, authoritative = related.People, true
		}
	}
	if !authoritative {
		return urls
	}
	for _, img := range images {
		if img.Kind != frontend.ImagePerson || !strings.HasPrefix(img.Subject, "tmdb:") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(img.Subject, "tmdb:"), 10, 64)
		if err != nil || id <= 0 || urls[id].url != "" {
			continue
		}
		grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device(r), Item: d.Title.Key, Image: img})
		if err != nil {
			continue
		}
		prefix := ""
		if r.Context().Value(sharedPrefix{}) != nil {
			prefix = "/silo"
		}
		urls[id] = creditPortrait{url: prefix + "/api/v2/images/" + grant, thumbHash: img.ThumbHash}
	}
	return urls
}

// sectionArtwork signs poster, backdrop, and logo for titles the row already
// authorized. The page signer resolves the artwork identity the image server
// accepts; a stored PosterPath is a different identity and Issue rejects it.
func (i *instance) sectionArtwork(r *http.Request, v *frontend.Viewer, credential string, titles []frontend.Title) map[frontend.RatingKey][]frontend.GrantedImage {
	out := map[frontend.RatingKey][]frontend.GrantedImage{}
	if i.host.ImageGrants == nil || len(titles) == 0 {
		return out
	}
	kinds := []string{frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageStill}
	if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
		const batch = 50
		for start := 0; start < len(titles); start += batch {
			end := min(start+batch, len(titles))
			page, err := signer.SignPage(r.Context(), frontend.ImagePageRequest{Credential: credential, Device: device(r), Titles: titles[start:end], Kinds: kinds})
			if err != nil {
				continue
			}
			for _, item := range page {
				if item.Authoritative && len(item.Images) > 0 {
					out[item.Item] = item.Images
				}
			}
		}
		return out
	}
	for _, title := range titles {
		if granted := i.storedCardGrants(r, v, credential, title); len(granted) > 0 {
			out[title.Key] = granted
		}
	}
	return out
}

func (i *instance) paintCardArtwork(r *http.Request, granted []frontend.GrantedImage, out map[string]any) {
	prefix := ""
	if r.Context().Value(sharedPrefix{}) != nil {
		prefix = "/silo"
	}
	fields := map[string]string{frontend.ImagePoster: "poster_url", frontend.ImageBackdrop: "backdrop_url", frontend.ImageLogo: "logo_url", frontend.ImageStill: "still_url"}
	seen := map[string]bool{}
	for _, grant := range granted {
		field := fields[grant.Image.Kind]
		if field == "" || grant.Secret == "" || seen[field] {
			continue
		}
		seen[field] = true
		out[field] = prefix + "/api/v2/images/" + grant.Secret
	}
}

// storedCardGrants is the single-grant fallback when the host cannot sign a page.
// It issues the artwork list's identity, which is what Serve rechecks.
func (i *instance) storedCardGrants(r *http.Request, v *frontend.Viewer, credential string, title frontend.Title) []frontend.GrantedImage {
	if i.host.ImageGrants == nil {
		return nil
	}
	images := []frontend.Image{
		{Kind: frontend.ImagePoster, Path: title.PosterPath},
		{Kind: frontend.ImageBackdrop, Path: title.BackdropPath},
		{Kind: frontend.ImageLogo, Path: title.LogoPath},
	}
	if art, ok := i.host.Experience.(frontend.ItemArtwork); ok {
		listed, complete, err := art.ItemArtwork(r.Context(), v, device(r), title.Key)
		if err == nil && complete {
			images = listed
		}
	}
	var out []frontend.GrantedImage
	seen := map[string]bool{}
	for _, img := range images {
		if img.Path == "" || seen[img.Kind] || (img.Kind != frontend.ImagePoster && img.Kind != frontend.ImageBackdrop && img.Kind != frontend.ImageLogo && img.Kind != frontend.ImageStill) {
			continue
		}
		seen[img.Kind] = true
		token, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device(r), Item: title.Key, Image: img})
		if err != nil {
			continue
		}
		out = append(out, frontend.GrantedImage{Image: img, Secret: token})
	}
	return out
}

func (i *instance) artwork(r *http.Request, viewer *frontend.Viewer, credential string, key frontend.RatingKey, out map[string]any) {
	if i.host.Experience == nil || i.host.ImageGrants == nil {
		return
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil {
		return
	}
	backdrops := []string{}
	var images []frontend.Image
	if detail.ImagesAuthoritative {
		images = detail.Images
	}
	for _, img := range images {
		field := map[string]string{"poster": "poster_url", "backdrop": "backdrop_url", "logo": "logo_url"}[img.Kind]
		if field == "" || img.Path == "" {
			continue
		}
		token, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device(r), Item: key, Image: img})
		if err != nil {
			continue
		}
		prefix := ""
		if r.Context().Value(sharedPrefix{}) != nil {
			prefix = "/silo"
		}
		path := prefix + "/api/v2/images/" + token
		if _, exists := out[field]; !exists {
			out[field] = path
			if img.ThumbHash != "" {
				if hashField := map[string]string{"poster": "poster_thumbhash", "backdrop": "backdrop_thumbhash"}[img.Kind]; hashField != "" {
					out[hashField] = img.ThumbHash
				}
			}
		}
		if img.Kind == "backdrop" {
			backdrops = append(backdrops, path)
		}
	}
	if r.PathValue("id") != "" && len(backdrops) > 0 {
		out["backdrop_urls"] = backdrops
	}
	if detail.Title.Type != "episode" {
		return
	}
	if detail.Title.PosterPath != "" || detail.Title.BackdropPath != "" || detail.Title.StillPath != "" {
		still := detail.Title.StillPath
		out["poster_is_episode_still"] = still != "" && detail.Title.PosterPath == still
		out["backdrop_is_episode_still"] = still != "" && detail.Title.BackdropPath == still
	}
	provider, ok := i.host.Experience.(frontend.RelatedArtworkProvider)
	if !ok {
		return
	}
	related, err := provider.RelatedArtwork(r.Context(), viewer, device(r), key)
	if err != nil {
		return
	}
	for _, relation := range []struct {
		owner  *frontend.ArtworkOwner
		fields map[string]string
	}{
		{related.Season, map[string]string{"poster": "season_poster"}},
		{related.Series, map[string]string{"poster": "series_poster", "backdrop": "series_backdrop"}},
	} {
		if relation.owner == nil || !relation.owner.Authoritative || relation.owner.Item == 0 {
			continue
		}
		for _, img := range relation.owner.Images {
			field := relation.fields[img.Kind]
			if field == "" || img.Path == "" {
				continue
			}
			if _, exists := out[field+"_url"]; exists {
				continue
			}
			token, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device(r), Item: relation.owner.Item, Image: img})
			if err != nil {
				continue
			}
			prefix := ""
			if r.Context().Value(sharedPrefix{}) != nil {
				prefix = "/silo"
			}
			out[field+"_url"] = prefix + "/api/v2/images/" + token
			if img.ThumbHash != "" {
				out[field+"_thumbhash"] = img.ThumbHash
			}
		}
	}
}

// imageCapabilities matches the reference Silo document the Apple TV client
// reads before it asks for sized artwork. Widths are the sizes this server
// can actually scale to.
const imageCapabilities = `{"revision":"2ba41576a16a8b6a3c8f7d0647f849a505c9cddcfb83ed6520e9bde2c794319b","state":"available","allowed":true,"episode_still_provenance":true,"episode_parent_artwork":true,"extended_item_artwork":true,"season_list_artwork_param":"include_artwork","param":"image_size","slot_width_params":{"backdrop":"backdrop_width","logo":"logo_width","photo":"photo_width","poster":"poster_width","still":"still_width"},"sizes":["small","medium","large","original"],"widths":{"backdrop":{"small":300,"medium":1920,"large":2560},"banner":{"small":500,"medium":500,"large":1000},"clearart":{"small":500,"medium":500,"large":1000},"disc":{"small":300,"medium":500,"large":1000},"logo":{"small":500,"medium":500,"large":1280},"poster":{"small":300,"medium":500,"large":1000},"profile":{"small":300,"medium":500,"large":500},"still":{"small":300,"medium":500,"large":1280},"thumb":{"small":300,"medium":500,"large":1280}},"original_max_width_px":3840,"storage_backend":"local","delivery":"server"}`

func (i *instance) imageCapabilities(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	etag := `"2ba41576a16a8b6a3c8f7d0647f849a505c9cddcfb83ed6520e9bde2c794319b"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(imageCapabilities))
}

func (i *instance) imageRoutes() error {
	if err := i.router.Handle("GET", "/api/v2/images/capabilities", http.HandlerFunc(i.imageCapabilities)); err != nil {
		return err
	}
	for _, method := range []string{"GET", "HEAD"} {
		if err := i.router.Handle(method, "/api/v2/images/{ticket}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i.host.ImageGrants == nil {
				http.NotFound(w, r)
				return
			}
			i.host.ImageGrants.Serve(w, r, r.PathValue("ticket"))
		})); err != nil {
			return err
		}
	}
	return nil
}
