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
	"github.com/braedonsaunders/ether/internal/plexguid"
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
	if (q.Has("type") && !validGUIDMediaType(typ)) || (sec != nil && (!s.canSeeSection(pr.User, sec.ID) || !sectionAllowsGUIDType(sec, typ))) {
		mc.SetPage(0, 0)
		s.write(w, r, mc)
		return
	}
	if sec != nil && typ == "" {
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
			if title == nil || (typ != "" && title.Type != typ) || !validGUIDMediaType(title.Type) || (!title.Released.IsZero() && title.Released.After(s.now())) || !s.visibleTitle(ctx, pr.User, title) {
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

func validGUIDMediaType(typ core.MediaType) bool {
	return typ == core.TypeMovie || typ == core.TypeShow || typ == core.TypeSeason || typ == core.TypeEpisode
}

func sectionAllowsGUIDType(sec *Section, typ core.MediaType) bool {
	if sec == nil || typ == "" || typ == sec.Type {
		return true
	}
	return sec.Type == core.TypeShow && (typ == core.TypeSeason || typ == core.TypeEpisode)
}

func (s *Server) guidKeys(ctx context.Context, guid string, typ core.MediaType) ([]core.RatingKey, error) {
	guid = strings.TrimSpace(guid)
	cleanGUID, _, _ := strings.Cut(guid, "?")
	scheme, id, ok := strings.Cut(cleanGUID, "://")
	if !ok {
		return nil, core.ErrNotFound
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	id = strings.Trim(strings.TrimSpace(id), "/")
	if id == "" {
		return nil, core.ErrNotFound
	}
	switch scheme {
	case "plex":
		if s.d.PlexGUID == nil {
			return nil, core.ErrNotFound
		}
		if strings.HasPrefix(cleanGUID, "plex://season/") || strings.HasPrefix(cleanGUID, "plex://episode/") {
			k, err := s.d.PlexGUID.ExactKey(ctx, cleanGUID)
			if err != nil {
				if errors.Is(err, plexguid.ErrAmbiguous) {
					return nil, core.ErrNotFound
				}
				return nil, err
			}
			return []core.RatingKey{k}, nil
		}
		k, err := s.d.PlexGUID.Lookup(ctx, cleanGUID)
		if err != nil {
			return nil, err
		}
		return []core.RatingKey{k}, nil
	case "tmdb", "themoviedb", "com.plexapp.agents.themoviedb":
		return tmdbGUIDKeys(id, typ)
	case "imdb", "com.plexapp.agents.imdb":
		if !strings.HasPrefix(id, "tt") {
			return nil, core.ErrNotFound
		}
		raw := strings.TrimPrefix(id, "tt")
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 || strings.ContainsAny(raw, "+- /") {
			return nil, core.ErrNotFound
		}
		t, err := s.d.Catalog.ByExternalID(ctx, "imdb", id)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, core.ErrNotFound
		}
		return []core.RatingKey{t.Key}, nil
	case "tvdb", "thetvdb", "com.plexapp.agents.thetvdb":
		return s.tvdbGUIDKeys(ctx, id, typ)
	}
	return nil, core.ErrNotFound
}

func parsePositiveTMDBID(raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || n >= 1_000_000_000_000_000 || strconv.FormatInt(n, 10) != raw {
		return 0, false
	}
	return n, true
}

func parseChildNumber(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n >= 10000 || strconv.Itoa(n) != raw {
		return 0, false
	}
	return n, true
}

func tmdbGUIDKeys(id string, typ core.MediaType) ([]core.RatingKey, error) {
	parts := strings.Split(id, "/")
	switch len(parts) {
	case 1:
		n, ok := parsePositiveTMDBID(parts[0])
		if !ok {
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
	case 2:
		showID, ok1 := parsePositiveTMDBID(parts[0])
		seasonNum, ok2 := parseChildNumber(parts[1])
		if !ok1 || !ok2 {
			return nil, core.ErrNotFound
		}
		switch typ {
		case core.TypeSeason:
			return []core.RatingKey{core.SeasonKey(showID, seasonNum)}, nil
		case "", core.TypeShow:
			return []core.RatingKey{core.ShowKey(showID)}, nil
		default:
			return nil, core.ErrNotFound
		}
	case 3:
		showID, ok1 := parsePositiveTMDBID(parts[0])
		seasonNum, ok2 := parseChildNumber(parts[1])
		epNum, ok3 := parseChildNumber(parts[2])
		if !ok1 || !ok2 || !ok3 {
			return nil, core.ErrNotFound
		}
		switch typ {
		case core.TypeEpisode:
			return []core.RatingKey{core.EpisodeKey(showID, seasonNum, epNum)}, nil
		case core.TypeSeason:
			return []core.RatingKey{core.SeasonKey(showID, seasonNum)}, nil
		case "", core.TypeShow:
			return []core.RatingKey{core.ShowKey(showID)}, nil
		default:
			return nil, core.ErrNotFound
		}
	default:
		return nil, core.ErrNotFound
	}
}

func (s *Server) tvdbGUIDKeys(ctx context.Context, id string, typ core.MediaType) ([]core.RatingKey, error) {
	parts := strings.Split(id, "/")
	if len(parts) < 1 || len(parts) > 3 {
		return nil, core.ErrNotFound
	}
	showRaw := parts[0]
	n, err := strconv.ParseInt(showRaw, 10, 64)
	if err != nil || n <= 0 || strings.ContainsAny(showRaw, "+- /") || strconv.FormatInt(n, 10) != showRaw {
		return nil, core.ErrNotFound
	}
	var seasonNum, epNum int
	if len(parts) >= 2 {
		var ok bool
		if seasonNum, ok = parseChildNumber(parts[1]); !ok {
			return nil, core.ErrNotFound
		}
	}
	if len(parts) == 3 {
		var ok bool
		if epNum, ok = parseChildNumber(parts[2]); !ok {
			return nil, core.ErrNotFound
		}
	}
	t, err := s.d.Catalog.ByExternalID(ctx, "tvdb", showRaw)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, core.ErrNotFound
	}
	if len(parts) == 1 {
		return []core.RatingKey{t.Key}, nil
	}
	if t.Key.Kind() != core.KindShow {
		return nil, core.ErrNotFound
	}
	switch typ {
	case core.TypeEpisode:
		if len(parts) == 3 {
			return []core.RatingKey{core.EpisodeKey(t.Key.TMDB(), seasonNum, epNum)}, nil
		}
		return nil, core.ErrNotFound
	case core.TypeSeason:
		return []core.RatingKey{core.SeasonKey(t.Key.TMDB(), seasonNum)}, nil
	case "", core.TypeShow:
		return []core.RatingKey{t.Key}, nil
	default:
		return nil, core.ErrNotFound
	}
}
