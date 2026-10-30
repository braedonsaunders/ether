package mediabrowser

import (
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) groupingOptions(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	result := make([]any, 0, len(views))
	for _, view := range views {
		if view.Type != "movie" && view.Type != "show" {
			continue
		}
		n, err := ids.ParseDecimal(view.ID)
		if err != nil {
			continue
		}
		id, err := ids.GUID(ids.View, n)
		if err != nil {
			continue
		}
		result = append(result, map[string]string{"Id": id, "Name": view.Name})
	}
	jsonReply(w, result)
}
