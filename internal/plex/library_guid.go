package plex

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// GUID lookups are exact identity requests, never a fall-through to browsing.
// The catalogue remains responsible for release eligibility; the same viewer
// policy used for metadata applies before anything is rendered or counted.
func (s *Server) libraryByGUID(w http.ResponseWriter, r *http.Request, sec *Section) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	pr := FromContext(ctx)
	q := r.URL.Query()
	mc := libraryContainer()
	if sec != nil {
		mc = s.sectionContainer(*sec)
	} else {
		mc.Mixed = model.Ptr(true)
	}
	typ := typeFromParam(q.Get("type"))
	if (q.Has("type") && typ != core.TypeMovie && typ != core.TypeShow) || (sec != nil && (!s.canSeeSection(pr.User, sec.ID) || (typ != "" && typ != sec.Type))) {
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}
	if sec != nil {
		typ = sec.Type
	}
	var guids []string
	for _, value := range q["guid"] {
		for _, guid := range strings.Split(value, ",") {
			if len(guids) == 20 {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			guids = append(guids, strings.TrimSpace(guid))
		}
	}
	var titles []core.Title
	seen := map[core.RatingKey]bool{}
	for _, guid := range guids {
		keys, err := s.guidKeys(ctx, guid, typ)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			s.writeErr(w, r, err)
			return
		}
		for _, key := range keys {
			if seen[key] {
				continue
			}
			seen[key] = true
			title, err := s.d.Catalog.Get(ctx, key)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				s.writeErr(w, r, err)
				return
			}
			if title == nil || (typ != "" && title.Type != typ) || (title.Type != core.TypeMovie && title.Type != core.TypeShow) || (!title.Released.IsZero() && title.Released.After(s.now())) || !s.visibleTitle(ctx, pr.User, title) {
				continue
			}
			titles = append(titles, *title)
		}
	}
	offset, size := pr.pageOf(0)
	total := len(titles)
	start := min(offset, total)
	titles = titles[start:min(start+size, total)]
	mc.Metadata = s.renderTitles(ctx, pr, titles, renderOpts{media: true, inSection: sec != nil})
	mc.Size = len(mc.Metadata)
	mc.SetPage(offset, total)
	s.write(w, r, mc)
}

func (s *Server) guidKeys(ctx context.Context, guid string, typ core.MediaType) ([]core.RatingKey, error) {
	scheme, id, ok := strings.Cut(guid, "://")
	if !ok || id == "" {
		return nil, core.ErrNotFound
	}
	switch scheme {
	case "plex":
		if s.d.PlexGUID == nil {
			return nil, core.ErrNotFound
		}
		k, err := s.d.PlexGUID.Lookup(ctx, guid)
		if err != nil {
			return nil, err
		}
		return []core.RatingKey{k}, nil
	case "tmdb":
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 || n >= 1_000_000_000_000_000 || strconv.FormatInt(n, 10) != id {
			return nil, core.ErrNotFound
		}
		var keys []core.RatingKey
		if typ == "" || typ == core.TypeMovie {
			keys = append(keys, core.MovieKey(n))
		}
		if typ == "" || typ == core.TypeShow {
			keys = append(keys, core.ShowKey(n))
		}
		return keys, nil
	case "imdb", "tvdb":
		raw := id
		if scheme == "imdb" {
			if !strings.HasPrefix(raw, "tt") {
				return nil, core.ErrNotFound
			}
			raw = strings.TrimPrefix(raw, "tt")
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 || strings.ContainsAny(raw, "+- /") {
			return nil, core.ErrNotFound
		}
		t, err := s.d.Catalog.ByExternalID(ctx, scheme, id)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, core.ErrNotFound
		}
		return []core.RatingKey{t.Key}, nil
	}
	return nil, core.ErrNotFound
}
