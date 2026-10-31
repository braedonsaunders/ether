package silo

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) creditIDs(r *http.Request, viewer *frontend.Viewer, d frontend.ItemDetail) map[int64]string {
	var ids []int64
	seen := map[int64]bool{}
	for _, group := range [][]frontend.Person{d.Title.Cast, d.Title.Crew, d.Title.Directors, d.Title.Writers} {
		for _, person := range group {
			if person.TMDB > 0 && !seen[person.TMDB] {
				seen[person.TMDB] = true
				ids = append(ids, person.TMDB)
			}
		}
	}
	result := map[int64]string{}
	for start := 0; start < len(ids); start += 256 {
		allowed, err := i.host.Experience.PersonFacetIDs(r.Context(), viewer, device(r), d.Title.Key, ids[start:min(start+256, len(ids))])
		if err != nil {
			return nil
		}
		for id, value := range allowed {
			if seen[id] && value != "" {
				result[id] = value
			}
		}
	}
	return result
}
