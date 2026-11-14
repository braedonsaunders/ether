package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strings"
)

func (i *instance) liveSearchItems(r *http.Request, viewer *frontend.Viewer, token, query string) ([]any, error) {
	out := []any{}
	search, ok := i.host.LiveTV.(frontend.LiveTVSearch)
	if !ok {
		return out, nil
	}
	results, err := search.SearchLiveTV(r.Context(), viewer, query, 100)
	if err != nil {
		return nil, err
	}
	include := strings.ToLower(strings.Join(route.Query(r.URL.Query(), "IncludeItemTypes"), ","))
	exclude := strings.ToLower(strings.Join(route.Query(r.URL.Query(), "ExcludeItemTypes"), ","))
	wants := func(names ...string) bool {
		for _, n := range names {
			if strings.Contains(exclude, n) {
				return false
			}
		}
		if include == "" {
			return true
		}
		for _, n := range names {
			if strings.Contains(include, n) {
				return true
			}
		}
		return false
	}
	if wants("livetvchannel", "tvchannel") {
		for _, c := range results.Channels {
			if dto, ok := i.channelDTO(r, token, frontend.LiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, Kind: c.Kind, Logo: c.Logo, Rating: c.Rating, Group: c.Group}); ok {
				out = append(out, dto)
			}
		}
	}
	if wants("livetvprogram", "program") {
		for _, p := range results.Programmes {
			if dto, ok := i.programmeDTO(frontend.LiveProgramme{ID: p.ID, ChannelID: p.ChannelID, Start: p.Start, End: p.End, Title: p.Item.Title, Summary: p.Item.Summary, Rating: p.Item.Rating, EpisodeTitle: p.Item.EpisodeTitle, ShowTitle: p.Item.ShowTitle, Genres: p.Item.Genres, MediaType: p.Item.MediaType, Season: p.Item.Season, Episode: p.Item.Episode}, frontend.LiveChannel{}); ok {
				out = append(out, dto)
			}
		}
	}
	return out, nil
}

func (i *instance) writeLiveSearchItems(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer) bool {
	terms := route.Query(r.URL.Query(), "SearchTerm")
	if len(terms) != 1 || strings.TrimSpace(terms[0]) == "" {
		return false
	}
	include := strings.ToLower(strings.Join(route.Query(r.URL.Query(), "IncludeItemTypes"), ","))
	if include != "" && !strings.Contains(include, "livetv") && !strings.Contains(include, "tvchannel") && include != "program" {
		return false
	}
	for _, kind := range strings.Split(include, ",") {
		if kind != "livetvchannel" && kind != "tvchannel" && kind != "livetvprogram" && kind != "program" {
			return false
		}
	}
	_, token, _ := i.viewer(r)
	live, err := i.liveSearchItems(r, viewer, token, terms[0])
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return true
	}
	offset := queryInt(r, "StartIndex", 0)
	limit := queryInt(r, "Limit", 50)
	jsonReply(w, i.queryResult(paging.Slice(live, offset, limit), len(live), offset))
	return true
}
