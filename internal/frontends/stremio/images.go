package stremio

import (
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) image(w http.ResponseWriter, r *http.Request) {
	if i.host.ImageGrants == nil {
		http.NotFound(w, r)
		return
	}
	i.host.ImageGrants.Serve(w, r, r.PathValue("secret"))
}

// imageBase obtains an owner-selected reachable address, not an untrusted Host header.
func (i *instance) imageBase(r *http.Request) *url.URL {
	if i.host.ClientIP != nil && i.host.SurfaceURL != nil {
		if peer, ok := i.host.ClientIP(r); ok && peer.IsLoopback() {
			base, err := i.host.SurfaceURL(r.Context(), r)
			u, parseErr := url.Parse(base)
			if err == nil && parseErr == nil && u.Scheme == "http" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasSuffix(u.Path, "/stremio") {
				if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
					u.Path += "/image/"
					u.RawPath = ""
					return u
				}
			}
		}
	}
	if i.host.SecurePortalURL == nil {
		return nil
	}
	portal, err := i.host.SecurePortalURL(r.Context(), r)
	if err != nil {
		return nil
	}
	u, err := url.Parse(portal)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil
	}
	if !strings.HasSuffix(u.Path, "/ether/me") {
		return nil
	}
	u.Path = strings.TrimSuffix(u.Path, "/ether/me") + "/stremio/image/"
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u
}

func (i *instance) artwork(r *http.Request, credential string, viewer *frontend.Viewer, device frontend.Device, titles []frontend.Title) map[frontend.RatingKey]string {
	result := make(map[frontend.RatingKey]string)
	batch, ok := i.host.ImageGrants.(frontend.ImageGrantBatch)
	base := i.imageBase(r)
	if !ok || base == nil {
		// No address a Stremio client can load Ether images from (no secure
		// portal, not on this machine): every title still gets a poster.
		for _, title := range titles {
			if poster := i.directPoster(r, viewer, device, title); poster != "" {
				result[title.Key] = poster
			}
		}
		return result
	}
	for start := 0; start < len(titles); start += 50 {
		end := start + 50
		if end > len(titles) {
			end = len(titles)
		}
		keys := make([]frontend.RatingKey, 0, end-start)
		for _, title := range titles[start:end] {
			keys = append(keys, title.Key)
		}
		items, err := batch.IssueBatch(r.Context(), frontend.ImageBatchRequest{Credential: credential, Device: device, Items: keys, Kinds: []string{frontend.ImagePoster}})
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.Authoritative && len(item.Images) != 0 {
				u := *base
				u.Path += item.Images[0].Secret
				result[item.Item] = u.String()
			}
		}
	}
	return result
}

// tmdbPosterBase serves TMDB's own poster files (~30 KB at w342, inside
// Stremio's 100 KB poster limit).
const tmdbPosterBase = "https://image.tmdb.org/t/p/w342"

// generatedPosterWidth keeps Ether's generated poster at about 11 KB
// (15 KB as a data URI), under Stremio's recommended 50 KB.
const generatedPosterWidth = 200

// directPoster is a poster URL a Stremio client loads without reaching
// Ether: TMDB's CDN for a real poster, else Ether's generated poster as a
// data: URI. stremio-core accepts the URL and stremio-web uses an image
// source; native Android TV/mobile rendering still needs device proof.
// Both come only for titles the
// experience already returned to this viewer; the generated one is read
// through the same artwork authorization as every other frontend.
func (i *instance) directPoster(r *http.Request, viewer *frontend.Viewer, device frontend.Device, title frontend.Title) string {
	if frontend.ValidArtworkPath(title.PosterPath) {
		return tmdbPosterBase + title.PosterPath
	}
	if !frontend.NeedsGeneratedPoster(&title) || i.host.Artwork == nil || viewer == nil {
		return ""
	}
	req := frontend.ArtworkRequest{Viewer: viewer, Device: device, Item: title.Key}
	images, _, err := i.host.Artwork.List(r.Context(), req)
	if err != nil {
		return ""
	}
	for _, img := range images {
		if img.Kind != frontend.ImagePoster {
			continue
		}
		data, ct, err := i.host.Artwork.Read(r.Context(), req, img, frontend.ArtworkTransform{Width: generatedPosterWidth, Format: "jpeg"})
		if err != nil || len(data) == 0 || len(data) > 48<<10 || !strings.HasPrefix(ct, "image/") {
			return ""
		}
		return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return ""
}

// backdropArtwork issues only bounded, already-selected row artwork; it never
// discovers extras or starts media resolution.
func (i *instance) backdropArtwork(r *http.Request, credential string, viewer *frontend.Viewer, device frontend.Device, titles []frontend.Title) map[frontend.RatingKey]string {
	out := map[frontend.RatingKey]string{}
	if len(titles) == 0 {
		return out
	}
	batch, ok := i.host.ImageGrants.(frontend.ImageGrantBatch)
	base := i.imageBase(r)
	if !ok || base == nil {
		for _, title := range titles {
			if frontend.ValidArtworkPath(title.BackdropPath) {
				out[title.Key] = "https://image.tmdb.org/t/p/w780" + title.BackdropPath
			}
		}
		return out
	}
	for start := 0; start < len(titles); start += 50 {
		end := min(start+50, len(titles))
		keys := make([]frontend.RatingKey, 0, end-start)
		for _, title := range titles[start:end] {
			keys = append(keys, title.Key)
		}
		rows, err := batch.IssueBatch(r.Context(), frontend.ImageBatchRequest{Credential: credential, Device: device, Items: keys, Kinds: []string{frontend.ImageBackdrop}})
		if err != nil {
			continue
		}
		for _, row := range rows {
			if !row.Authoritative {
				continue
			}
			for _, image := range row.Images {
				if image.Image.Kind == frontend.ImageBackdrop && image.Secret != "" {
					u := *base
					u.Path += image.Secret
					out[row.Item] = u.String()
					break
				}
			}
		}
	}
	return out
}
