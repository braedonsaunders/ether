package plex

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// Playlist search never expands a smart query or scans the catalogue. Section
// membership comes from stored member identities or the smart query's section.
func (s *Server) searchPlaylists(ctx context.Context, pr *Request, query, directories string, limit int) []model.Metadata {
	if s.d.Playlists == nil || pr.User == nil || query == "" {
		return nil
	}
	lists, err := s.d.Playlists.List(ctx, pr.User.ID)
	if err != nil {
		return nil
	}
	var out []model.Metadata
	for i := range lists {
		pl := &lists[i]
		if pl.UserID != pr.User.ID || playlistSearchScore(pl.Title, query) == 0 {
			continue
		}
		if directories != "" && !s.playlistInDirectories(pl, directories) {
			continue
		}
		m := s.playlistListingMetadata(ctx, pl)
		m.Score = strconv.FormatFloat(playlistSearchScore(pl.Title, query), 'f', 5, 64)
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Server) playlistInDirectories(pl *core.Playlist, raw string) bool {
	wants := strings.Split(raw, ",")
	matches := func(id int) bool {
		for _, want := range wants {
			if strconv.Itoa(id) == want {
				return true
			}
		}
		return false
	}
	if pl.SmartURI != "" {
		path, err := uriPath(pl.SmartURI)
		if err != nil {
			return false
		}
		u, err := url.Parse(path)
		if err != nil {
			return false
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "library" && parts[1] == "sections" {
			id, _ := strconv.Atoi(parts[2])
			return matches(id)
		}
	}
	for _, key := range pl.Items {
		var typ core.MediaType
		switch key.Kind() {
		case core.KindMovie:
			typ = core.TypeMovie
		case core.KindShow, core.KindSeason, core.KindEpisode:
			typ = core.TypeShow
		}
		if sec, ok := s.sectionFor(typ); ok && matches(sec.ID) {
			return true
		}
	}
	return false
}

func containsExactSearchToken(raw, token string) bool {
	for _, v := range strings.Split(raw, ",") {
		if v == token {
			return true
		}
	}
	return false
}

// PMS's playlist search ladder sits below title results at each match class.
func playlistSearchScore(title, query string) float64 {
	t, q := searchFoldName(title), searchFoldName(query)
	if q == "" {
		return 0
	}
	if t == q {
		return .92
	}
	if strings.HasPrefix(t, q) {
		return .72
	}
	words, terms := strings.Fields(strings.ToLower(title)), strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return 0
	}
	for start := range words {
		if start+len(terms) > len(words) {
			break
		}
		match := true
		for i, term := range terms {
			if !strings.HasPrefix(searchFoldName(words[start+i]), searchFoldName(term)) {
				match = false
				break
			}
		}
		if match {
			if start == 0 {
				return .52
			}
			return .32
		}
	}
	return 0
}

func (s *Server) sectionPlaylistsHub(ctx context.Context, pr *Request, section, count int) (model.Hub, bool) {
	h := model.Hub{Title: "Recent Playlists", Type: "playlist", HubIdentifier: "recent.library.playlists." + strconv.Itoa(section), Style: "shelf"}
	h.Key = "/playlists/all?type=15&playlistType=video&sort=lastViewedAt:desc&sectionID=" + strconv.Itoa(section)
	if pr.User == nil || s.d.Playlists == nil {
		return h, false
	}
	pls, err := s.d.Playlists.List(ctx, pr.User.ID)
	if err != nil {
		return h, false
	}
	for _, pl := range pls {
		if pl.UserID != pr.User.ID {
			continue
		}
		if len(pl.Items) > 0 && !s.playlistInDirectories(&pl, strconv.Itoa(section)) {
			continue
		}
		h.Metadata = append(h.Metadata, s.playlistListingMetadata(ctx, &pl))
	}
	sort.SliceStable(h.Metadata, func(i, j int) bool { return h.Metadata[i].LastViewedAt > h.Metadata[j].LastViewedAt })
	h.More = len(h.Metadata) > count
	if h.More {
		h.Metadata = h.Metadata[:count]
	}
	h.Size = len(h.Metadata)
	keys := make([]string, 0, h.Size)
	for _, md := range h.Metadata {
		keys = append(keys, md.RatingKey)
	}
	if len(keys) > 0 {
		h.HubKey = "/library/metadata/" + strings.Join(keys, ",")
	}
	return h, true
}
