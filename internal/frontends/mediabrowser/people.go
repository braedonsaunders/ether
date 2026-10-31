package mediabrowser

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// peopleDTO uses parent-authorized facet IDs and title-bound portrait grants.
func (i *instance) peopleDTO(r *http.Request, viewer *frontend.Viewer, token string, detail frontend.ItemDetail) []any {
	type credit struct {
		id               int64
		name, role, kind string
	}
	var credits []credit
	for _, p := range detail.Title.Cast {
		credits = append(credits, credit{p.TMDB, p.Name, p.Role, "Actor"})
	}
	for _, p := range detail.Title.Directors {
		credits = append(credits, credit{p.TMDB, p.Name, p.Role, "Director"})
	}
	for _, p := range detail.Title.Writers {
		credits = append(credits, credit{p.TMDB, p.Name, p.Role, "Writer"})
	}
	ids := make([]int64, 0, min(len(credits), 256))
	seen := map[int64]bool{}
	for _, c := range credits {
		if c.id <= 0 || seen[c.id] {
			continue
		}
		if len(ids) == 256 {
			break
		}
		seen[c.id] = true
		ids = append(ids, c.id)
	}
	facets := map[int64]string{}
	if len(ids) > 0 {
		resolved, err := i.host.Experience.PersonFacetIDs(r.Context(), viewer, device(r), detail.Title.Key, ids)
		if err != nil {
			return nil
		}
		facets = resolved
	}
	portraits := map[string]frontend.Image{}
	if detail.ImagesAuthoritative && i.host.ImageGrants != nil {
		for _, img := range detail.Images {
			if img.Kind == frontend.ImagePerson && img.Subject != "" {
				if _, ok := portraits[img.Subject]; !ok {
					portraits[img.Subject] = img
				}
			}
		}
	}
	grants := map[int64]frontend.Image{}
	granted := map[int64]string{}
	attempted := map[int64]bool{}
	result := make([]any, 0, len(credits))
	for _, c := range credits {
		if c.name == "" {
			continue
		}
		id := ""
		if c.id > 0 {
			id = facets[c.id]
			if id == "" {
				continue
			}
		}
		dto := map[string]any{"Name": c.name, "Type": c.kind}
		if id != "" {
			dto["Id"] = id
		}
		if c.role != "" {
			dto["Role"] = c.role
		}
		if !attempted[c.id] {
			attempted[c.id] = true
			if img, ok := portraits["tmdb:"+strconv.FormatInt(c.id, 10)]; ok {
				grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Item: detail.Title.Key, Image: img})
				if err == nil {
					grants[c.id] = img
					granted[c.id] = grant
				}
			}
		}
		if grant := granted[c.id]; grant != "" {
			dto["PrimaryImageTag"] = grant
			i.addImageMetadata(dto, "Primary", grant, grants[c.id])
			// A person record carries the portrait hash, not the item aspect ratio.
			delete(dto, "PrimaryImageAspectRatio")
		}
		result = append(result, dto)
	}
	return result
}
