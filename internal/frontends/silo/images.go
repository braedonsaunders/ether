package silo

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) artwork(r *http.Request, viewer *frontend.Viewer, credential string, key frontend.RatingKey, out map[string]any) {
	if i.host.Experience == nil || i.host.ImageGrants == nil {
		return
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil || !detail.ImagesAuthoritative {
		return
	}
	backdrops := []string{}
	for _, img := range detail.Images {
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
		}
		if img.Kind == "backdrop" {
			backdrops = append(backdrops, path)
		}
	}
	if r.PathValue("id") != "" && len(backdrops) > 0 {
		out["backdrop_urls"] = backdrops
	}
}
func (i *instance) imageRoutes() error {
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
