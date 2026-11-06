package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"net/http"
	"strconv"
)

func writeStateMutationOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
}

// mutationKey implements the legacy writers' bare-key and refusal order.
func (s *Server) mutationKey(w http.ResponseWriter, r *http.Request, integer bool) (core.RatingKey, bool) {
	q := r.URL.Query()
	raw := q.Get("key")
	pr := FromContext(r.Context())
	if raw == "" && !integer {
		writeHTTPError(w, 400)
		return 0, false
	}
	if q.Get("identifier") == "" {
		writeHTTPError(w, 400)
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if integer && err != nil {
		writeHTTPError(w, 500)
		return 0, false
	}
	if q.Get("identifier") != "com.plexapp.plugins.library" {
		writeHTTPError(w, 404)
		return 0, false
	}
	key := core.RatingKey(n)
	if err != nil || key == 0 || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, 404)
		return 0, false
	}
	if key.Kind() == core.KindPlaylist && s.d.Playlists != nil {
		if _, err := s.d.Playlists.Get(r.Context(), pr.User.ID, key); err != nil {
			s.writeErr(w, r, err)
			return 0, false
		}
	} else if key.Kind() == core.KindCollection || key.Kind() == core.KindSmart {
		if _, err := s.collectionRaw(r.Context(), pr.User, key); err != nil {
			s.writeErr(w, r, err)
			return 0, false
		}
	} else {
		if s.d.Catalog == nil {
			writeHTTPError(w, 404)
			return 0, false
		}
		title, err := s.d.Catalog.Get(r.Context(), key)
		if err != nil || title == nil || !s.titleVisible(pr.User, title) {
			writeHTTPError(w, 404)
			return 0, false
		}
	}
	return key, true
}

// List watch state belongs to the list, never to its members.
func (s *Server) applyListWatch(ctx context.Context, m *model.Metadata, key core.RatingKey) {
	pr := FromContext(ctx)
	if pr == nil || pr.User == nil || s.d.State == nil {
		return
	}
	st, err := s.d.State.Get(ctx, pr.User.ID, key)
	if err != nil {
		return
	}
	m.UserRating = st.UserRating
	m.ViewOffset = ms(st.ViewOffset)
	m.ViewCount = st.ViewCount
	m.LastViewedAt = unixOrZero(st.LastViewedAt)
	m.LastRatedAt = unixOrZero(st.LastRatedAt)
}
