package plex

import (
	"context"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
)

func (s *Server) historyStatsQuery(r *http.Request) (core.HistoryQuery, map[int64]core.User, error) {
	pr := FromContext(r.Context())
	q := core.HistoryQuery{Limit: 100}
	users := map[int64]core.User{}
	if pr.User != nil {
		users[pr.User.ID] = *pr.User
		if !pr.User.IsOwner {
			q.UserID = pr.User.ID
		}
	}
	if pr.User != nil && pr.User.IsOwner && s.d.Auth != nil {
		us, err := s.d.Auth.Users(r.Context())
		if err != nil {
			return q, nil, err
		}
		for _, u := range us {
			users[u.ID] = u
		}
	}
	values := r.URL.Query()
	if values.Has("accountID") {
		id, err := strconv.ParseInt(values.Get("accountID"), 10, 64)
		if err != nil {
			return q, nil, err
		}
		q.UserID = -1
		for uid, u := range users {
			if u.PlexID == id {
				q.UserID = uid
			}
		}
	}
	for _, field := range []struct {
		name string
		dest **int64
	}{{"viewedAt>", &q.Since}, {"viewedAt<", &q.Until}, {"viewedAt", &q.At}} {
		if values.Has(field.name) {
			v, err := strconv.ParseInt(values.Get(field.name), 10, 64)
			if err != nil {
				return q, nil, err
			}
			*field.dest = &v
		}
	}
	if values.Has("count") {
		n, err := strconv.ParseInt(values.Get("count"), 10, 32)
		if err != nil {
			return q, nil, err
		}
		q.Limit = int(max(0, min(n, 500)))
	}
	return q, users, nil
}
func (s *Server) handleHistoryTop(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	mc := &model.MediaContainer{}
	n, err := strconv.ParseInt(r.URL.Query().Get("type"), 10, 32)
	if err != nil {
		writeHTTPError(w, 400)
		return
	}
	q, users, err := s.historyStatsQuery(r)
	if err != nil {
		writeHTTPError(w, 500)
		return
	}
	if r.URL.Query().Has("count") && q.Limit == 0 {
		s.write(w, r, mc)
		return
	}
	kind := map[int64]core.KeyKind{1: core.KindMovie, 2: core.KindShow, 3: core.KindSeason, 4: core.KindEpisode}[n]
	store, ok := s.d.State.(core.HistoryTopStore)
	if !ok || kind == 0 {
		s.write(w, r, mc)
		return
	}
	if r.URL.Query().Has("librarySectionID") {
		id, err := strconv.ParseInt(r.URL.Query().Get("librarySectionID"), 10, 64)
		if err != nil {
			writeHTTPError(w, 500)
			return
		}
		sec, ok := s.sectionByID(int(id))
		if !ok || (sec.Type == core.TypeMovie) != (kind == core.KindMovie) {
			s.write(w, r, mc)
			return
		}
	}
	rows, err := store.HistoryTop(r.Context(), q, kind)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	keys := make([]core.RatingKey, len(rows))
	counts := map[core.RatingKey]core.HistoryCount{}
	for i, h := range rows {
		keys[i] = h.Key
		counts[h.Key] = h
	}
	titles := s.getTitles(r.Context(), keys)
	// Fetch is batched; retain the rank selected by the statistics index.
	byKey := map[core.RatingKey]core.Title{}
	for _, t := range titles {
		byKey[t.Key] = t
	}
	titles = nil
	for _, k := range keys {
		if t, ok := byKey[k]; ok && s.visibleTitle(r.Context(), pr.User, &t) {
			titles = append(titles, t)
		}
	}
	full, _ := r.Context().Value(historyPopularKey{}).(bool)
	mc.Metadata = s.renderTitles(r.Context(), pr, titles, renderOpts{media: true, storedOnly: true, detail: full, guids: full, detailEpisodes: full})
	for i := range mc.Metadata {
		md := &mc.Metadata[i]
		key, _ := core.ParseRatingKey(md.RatingKey)
		h := counts[key]
		if full {
			continue
		}
		md.GlobalViewCount = model.Ptr(h.Count)
		md.UserCount = model.Ptr(len(h.Users))
		for _, uid := range h.Users {
			if u, ok := users[uid]; ok {
				md.TopUsers = append(md.TopUsers, model.HistoryUser{ID: u.PlexID})
			}
		}
		if kind == core.KindShow || kind == core.KindSeason {
			md.SkipCount = 0
			md.LastRatedAt = 0
			md.ViewCount = 0
			md.LastViewedAt = 0
			md.UserRating = 0
		}
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}
func (s *Server) handleHistoryTopUsers(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	key, err := core.ParseRatingKey(chi.URLParam(r, "key"))
	if err != nil {
		writeHTTPError(w, 404)
		return
	}
	title, err := s.d.Catalog.Get(r.Context(), key)
	if err != nil || title == nil || !s.visibleTitle(r.Context(), pr.User, title) {
		writeHTTPError(w, 404)
		return
	}
	if key.Kind() == core.KindSeason {
		writeHTTPError(w, 500)
		return
	}
	q, users, err := s.historyStatsQuery(r)
	if err != nil {
		writeHTTPError(w, 500)
		return
	}
	mc := &model.MediaContainer{}
	if store, ok := s.d.State.(core.HistoryTopStore); ok {
		rows, err := store.LifetimeUsers(r.Context(), key, q)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		for _, h := range rows {
			u, ok := users[h.Users[0]]
			if !ok {
				continue
			}
			mc.Account = append(mc.Account, model.Account{ID: u.PlexID, Name: u.Username, GlobalViewCount: model.Ptr(h.Count)})
		}
	}
	mc.Size = len(mc.Account)
	s.write(w, r, mc)
}

func (s *Server) handleHistoryPopular(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "section"))
	if err != nil {
		writeHTTPError(w, 404)
		return
	}
	sec, ok := s.sectionByID(id)
	if !ok {
		writeHTTPError(w, 404)
		return
	}
	if sec.Type != core.TypeShow {
		s.writeEmpty(w, r)
		return
	}
	q := r.URL.Query()
	q.Set("type", "3")
	q.Set("librarySectionID", strconv.Itoa(id))
	r2 := r.Clone(r.Context())
	u := *r.URL
	r2.URL = &u
	r2.URL.RawQuery = q.Encode()
	r2 = r2.WithContext(context.WithValue(r2.Context(), historyPopularKey{}, true))
	s.handleHistoryTop(w, r2)
}

type historyPopularKey struct{}
