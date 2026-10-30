package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) mutate(w http.ResponseWriter, r *http.Request, kind string) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("itemId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch kind {
	case "played":
		if r.Method == "POST" {
			err = i.host.Experience.SetPlayed(r.Context(), v, key)
		} else {
			err = i.host.Experience.SetUnplayed(r.Context(), v, key)
		}
	case "likes":
		var likes *bool
		if r.Method == "POST" {
			values := route.Query(r.URL.Query(), "likes")
			if len(values) > 1 {
				http.Error(w, "Bad Request", 400)
				return
			}
			if len(values) == 1 {
				val, e := strconv.ParseBool(values[0])
				if e != nil {
					http.Error(w, "Bad Request", 400)
					return
				}
				likes = &val
			}
		}
		err = i.host.Experience.SetLikes(r.Context(), v, key, likes)
	case "favorite":
		err = i.host.Experience.SetFavorite(r.Context(), v, key, r.Method == "POST")
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	data, err := i.host.Experience.UserData(r.Context(), v, []frontend.RatingKey{key})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	st := data[key]
	id := ids.RatingKeyGUID(key)
	dto := i.userDataDTO(id, st)
	// Jellyfin omits a zero percentage on the played-item body. Item listings
	// still carry the key when the watch row has progress.
	if i.protocol == "jellyfin" {
		if pct, ok := dto["PlayedPercentage"].(float64); ok && pct == 0 {
			delete(dto, "PlayedPercentage")
		}
	}
	jsonReply(w, dto)
}
func (i *instance) mutationRoutes() error {
	for _, path := range []struct{ pattern, kind string }{{"/UserItems/{itemId}/Rating", "likes"}, {"/Users/{userId}/Items/{itemId}/Rating", "likes"}, {"/UserPlayedItems/{itemId}", "played"}, {"/Users/{userId}/PlayedItems/{itemId}", "played"}, {"/UserFavoriteItems/{itemId}", "favorite"}, {"/Users/{userId}/FavoriteItems/{itemId}", "favorite"}} {
		for _, method := range []string{"POST", "DELETE"} {
			kind := path.kind
			if err := i.router.Handle(method, path.pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.mutate(w, r, kind) })); err != nil {
				return err
			}
		}
	}
	return nil
}
