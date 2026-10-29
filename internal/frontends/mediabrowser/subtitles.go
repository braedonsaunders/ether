package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strconv"
	"strings"
)

func (i *instance) subtitle(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	service := i.host.Sessions
	if service == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if source := r.PathValue("source"); source != ids.RatingKeyGUID(key) {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		http.NotFound(w, r)
		return
	}
	name := strings.ToLower(r.PathValue("name"))
	format := strings.TrimPrefix(name, "stream.")
	if name == format || (format != "vtt" && format != "srt") {
		http.NotFound(w, r)
		return
	}
	vals := route.Query(r.URL.Query(), "PlaySessionId")
	if len(vals) != 1 || vals[0] == "" {
		http.Error(w, "Missing session", 400)
		return
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: i.protocol}
	sessions, err := i.host.Sessions.Sessions(r.Context(), access)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	found := false
	for _, s := range sessions {
		if s.ID == vals[0] && s.Item == key && s.Protocol == i.protocol {
			found = true
			break
		}
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	url, err := service.SubtitleURL(r.Context(), access, vals[0], index, format)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(url, "/sessions/media/") || strings.ContainsAny(url, "\r\n") {
		http.Error(w, "Unavailable", 503)
		return
	}
	if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
		url = prefix + url
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}
