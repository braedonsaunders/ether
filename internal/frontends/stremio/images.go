package stremio

import (
	"net/http"
	"net/url"

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
	if i.host.ViewerPortalURL == nil {
		return nil
	}
	portal, err := i.host.ViewerPortalURL(r.Context(), r)
	if err != nil {
		return nil
	}
	u, err := url.Parse(portal)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/stremio/image/", "", "", ""
	return u
}

func (i *instance) artwork(r *http.Request, credential string, device frontend.Device, titles []frontend.Title) map[frontend.RatingKey]string {
	result := make(map[frontend.RatingKey]string)
	batch, ok := i.host.ImageGrants.(frontend.ImageGrantBatch)
	base := i.imageBase(r)
	if !ok || base == nil {
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
