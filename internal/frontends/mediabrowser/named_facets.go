package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// addNamedFacets uses only IDs issued for fields on this authorized title.
func (i *instance) addNamedFacets(r *http.Request, viewer *frontend.Viewer, detail frontend.ItemDetail, dto map[string]any) {
	for _, group := range []struct {
		kind, field string
		names       []string
	}{
		{"genre", "GenreItems", detail.Title.Genres}, {"studio", "Studios", detail.Title.Studios},
	} {
		names := make([]string, 0, min(len(group.names), 256))
		seen := map[string]bool{}
		for _, name := range group.names {
			if name == "" || seen[name] {
				continue
			}
			if len(names) == 256 {
				break
			}
			seen[name] = true
			names = append(names, name)
		}
		if len(names) == 0 {
			continue
		}
		authorized, err := i.host.Experience.NamedFacetIDs(r.Context(), viewer, device(r), detail.Title.Key, group.kind, names)
		if err != nil {
			continue
		}
		result := make([]any, 0, len(names))
		for _, name := range names {
			if id := authorized[name]; id != "" {
				result = append(result, map[string]any{"Id": id, "Name": name})
			}
		}
		if len(result) > 0 {
			dto[group.field] = result
		}
	}
}
