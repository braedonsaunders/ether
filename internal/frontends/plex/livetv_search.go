package plex

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

// Search shares the indexed, policy-filtered Live TV search with other apps.
func (s *Server) handleLiveChannelSearch(w http.ResponseWriter, r *http.Request) {
	limit, ok := searchLimit(w, r, 10, 50)
	if !ok {
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	if utf8.RuneCountInString(query) > 256 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	mc := &model.MediaContainer{Identifier: frontend.LiveTVProvider}
	if query == "" {
		s.write(w, r, mc)
		return
	}
	search, ok := s.svc().liveTV.(frontend.LiveTVSearch)
	if !ok {
		s.write(w, r, mc)
		return
	}
	results, err := search.SearchLiveTV(r.Context(), FromContext(r.Context()).User, query, limit)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	for _, c := range results.Channels {
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 1, Channel: s.liveSearchChannel(c)})
	}
	parents := map[string]frontend.LiveTVChannel{}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	for _, p := range results.Programmes {
		md := s.liveProgrammeMetadata(parents[p.ChannelID], p)
		mc.SearchResult = append(mc.SearchResult, model.SearchResult{Score: 0.9, Metadata: &md})
	}
	mc.Size = len(mc.SearchResult)
	s.write(w, r, mc)
}

func (s *Server) liveSearchChannel(c frontend.LiveTVChannel) *model.SearchChannel {
	categories := model.StringList{}
	if c.Group != "" {
		categories = append(categories, c.Group)
	}
	return &model.SearchChannel{ID: c.ID, GridKey: c.ID, VCN: c.Number, Title: c.Name, ShortTitle: c.Name, Categories: categories, Source: "server://" + s.identity().MachineIdentifier + "/" + frontend.LiveTVProvider}
}

func (s *Server) appendLiveSearchHubs(r *http.Request, mc *model.MediaContainer, query string, limit int) {
	search, ok := s.svc().liveTV.(frontend.LiveTVSearch)
	if !ok || r.URL.Query().Get("sectionId") != "" {
		return
	}
	results, err := search.SearchLiveTV(r.Context(), FromContext(r.Context()).User, query, limit)
	if err != nil {
		s.log.Warn("live search unavailable", "error", err)
		return
	}
	if len(results.Channels) > 0 {
		hub := model.Hub{Title: "Live TV", Type: "livetv", HubIdentifier: "livetv", Style: "shelf"}
		for _, c := range results.Channels {
			hub.Channel = append(hub.Channel, *s.liveSearchChannel(c))
		}
		hub.Size = len(hub.Channel)
		mc.Hub = append(mc.Hub, hub)
	}
	parents := map[string]frontend.LiveTVChannel{}
	for _, c := range results.ProgrammeChannels {
		parents[c.ID] = c
	}
	if len(results.Programmes) > 0 {
		hub := model.Hub{Title: "On TV", Type: "episode", HubIdentifier: "tv.plex.provider.epg", Style: "shelf"}
		for _, p := range results.Programmes {
			hub.Metadata = append(hub.Metadata, s.liveProgrammeMetadata(parents[p.ChannelID], p))
		}
		hub.Size = len(hub.Metadata)
		mc.Hub = append(mc.Hub, hub)
	}
}
