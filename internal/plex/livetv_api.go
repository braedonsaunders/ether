package plex

// Live TV wire shapes and route names follow Plex's official PMS OpenAPI 1.2.3,
// https://developer.plex.tv/pms/ (DVR, Devices, EPG and Live TV groups). In
// particular the grid attaches broadcast times to Media, not the Video item.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/go-chi/chi/v5"
)

const liveProviderPath = "/" + livetv.Provider

type liveChannel struct {
	ID       string `plex:"id"`
	GridKey  string `plex:"gridKey"`
	VCN      string `plex:"vcn"`
	IsHD     bool   `plex:"isHd"`
	Thumb    string `plex:"thumb"`
	Title    string `plex:"title"`
	CallSign string `plex:"callSign"`
	Language string `plex:"language"`
	Key      string `plex:"key"`
}
type liveMapping struct {
	ChannelKey       string `plex:"channelKey"`
	DeviceIdentifier string `plex:"deviceIdentifier"`
	Enabled          string `plex:"enabled"`
	LineupIdentifier string `plex:"lineupIdentifier"`
}
type liveDevice struct {
	Key            string        `plex:"key"`
	Make           string        `plex:"make"`
	Model          string        `plex:"model"`
	Protocol       string        `plex:"protocol"`
	Sources        string        `plex:"sources"`
	State          string        `plex:"state"`
	Status         string        `plex:"status"`
	Tuners         string        `plex:"tuners"`
	URI            string        `plex:"uri"`
	UUID           string        `plex:"uuid"`
	ChannelMapping []liveMapping `plex:"ChannelMapping"`
}
type liveDVR struct {
	Key         string       `plex:"key"`
	Language    string       `plex:"language"`
	Lineup      string       `plex:"lineup"`
	UUID        string       `plex:"uuid"`
	RefreshedAt int64        `plex:"refreshedAt"`
	Device      []liveDevice `plex:"Device"`
}
type liveGrabber struct {
	Title      string `plex:"title"`
	Identifier string `plex:"identifier"`
	Protocol   string `plex:"protocol"`
}
type liveContainer struct {
	Size           int           `plex:"size"`
	Identifier     string        `plex:"identifier"`
	DVR            []liveDVR     `plex:"Dvr"`
	Device         []liveDevice  `plex:"Device"`
	Channel        []liveChannel `plex:"Channel"`
	ChannelMapping []liveMapping `plex:"ChannelMapping"`
	MediaGrabber   []liveGrabber `plex:"MediaGrabber"`
}

func (s *Server) liveTVEnabled() bool { return s.d.LiveTV != nil && s.identity().MyPlexSubscription }
func (s *Server) liveTVProvider() *model.MediaProvider {
	if !s.liveTVEnabled() {
		return nil
	}
	return &model.MediaProvider{Extra: []model.Attr{model.A("id", 1), model.A("parentID", 1), model.A("providerIdentifier", "tv.plex.providers.epg.xmltv"), model.A("epgSource", "xmltv")}, Identifier: livetv.Provider, Title: "Live TV", Types: "video", Protocols: "livetv", Feature: []model.Feature{
		{Type: "content", Key: liveProviderPath, Directory: []model.ProviderDirectory{{ID: "livetv", Key: liveProviderPath + "/lineups/dvr/channels", HubKey: liveProviderPath + "/hubs", Title: "Live TV", Type: "livetv"}}},
		{Type: "grid", Key: liveProviderPath + "/grid"}, {Type: "metadata", Key: liveProviderPath + "/metadata"}, {Type: "livetv", Key: "/livetv/dvrs"},
	}}
}

func (s *Server) registerLiveTVRoutes(r chi.Router) {
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.liveTVEnabled() {
				writeHTTPError(w, 404)
				return
			}
			if c := chi.URLParam(r, "consumer"); c != "" && c != "0" {
				writeHTTPError(w, 404)
				return
			}
			pr := FromContext(r.Context())
			if p, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
				writeHTTPError(w, 403)
				return
			}
			h(w, r)
		}
	}
	get := func(path string, h http.HandlerFunc) { r.Get(path, guard(h)); r.Head(path, guard(h)) }
	get("/media/subscriptions", s.handleLiveSubscriptions)
	get("/media/subscriptions/{subscription}", s.handleLiveSubscription)
	r.Delete("/media/subscriptions/{subscription}", guard(s.handleLiveSubscriptionStop))
	get("/media/grabbers/operations/{operation}", s.handleLiveOperation)
	r.Delete("/media/grabbers/operations/{operation}", guard(s.handleLiveOperationStop))
	get("/livetv/dvrs", s.handleLiveDVRs)
	get("/livetv/dvrs/{dvr}", s.handleLiveDVRs)
	r.Post("/livetv/dvrs/{dvr}/channels/{channel}/tune", guard(s.handleLiveTune))
	get("/media/grabbers", s.handleLiveGrabbers)
	get("/media/grabbers/devices", s.handleLiveDevices)
	get("/media/grabbers/devices/{device}", s.handleLiveDevices)
	get("/media/grabbers/devices/{device}/channels", s.handleLiveChannels)
	get("/media/grabbers/devices/{device}/channelmap", s.handleLiveChannels)
	for _, path := range []string{"/livetv/epg/channels", "/livetv/epg/lineupchannels", "/livetv/epg/channelmap", liveProviderPath + "/lineups/dvr/channels"} {
		get(path, s.handleLiveChannels)
	}
	get(liveProviderPath, s.handleLiveRoot)
	get(liveProviderPath+"/grid", s.handleLiveGuide)
	get(liveProviderPath+"/metadata/{programme}", s.handleLiveGuide)
	get(liveProviderPath+"/channels/{channel}", s.handleLiveChannel)
	get(liveProviderPath+"/hubs", s.handleLiveHubs)
	get(liveProviderPath+"/watchnow", s.handleLiveWatchNow)
	get(liveProviderPath+"/watchnow/all", s.handleLiveGuide)
	get(liveProviderPath+"/sections/{section}/all", s.handleLiveGuide)
	get("/livetv/guide.xml", func(w http.ResponseWriter, r *http.Request) {
		s.d.LiveTV.ServeXMLTV(w, r, FromContext(r.Context()).User)
	})
	get("/livetv/channels/{channel}/logo.svg", s.handleLiveLogo)
	get("/livetv/sessions", s.handleLiveSessions)
	get("/livetv/sessions/{session}", s.handleLiveSession)
	r.Delete("/livetv/sessions/{session}", guard(func(w http.ResponseWriter, r *http.Request) {
		err := s.d.LiveTV.Stop(r.Context(), chi.URLParam(r, "session"), FromContext(r.Context()).User)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.writeEmpty(w, r)
	}))
	get("/livetv/sessions/{session}/{consumer}/dash/{rep}/{part}", func(w http.ResponseWriter, r *http.Request) {
		rep, err := strconv.Atoi(chi.URLParam(r, "rep"))
		part := chi.URLParam(r, "part")
		init := part == "header"
		n, e := strconv.Atoi(strings.TrimSuffix(part, ".m4s"))
		if err != nil || rep < 0 || (!init && (e != nil || n < 0 || !strings.HasSuffix(part, ".m4s"))) {
			writeHTTPError(w, 404)
			return
		}
		s.d.LiveTV.ServeDASHPart(w, r, chi.URLParam(r, "session"), FromContext(r.Context()).User, rep, n, init)
	})
	get("/livetv/sessions/{session}/{consumer}/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if chi.URLParam(r, "consumer") != "0" {
			writeHTTPError(w, 404)
			return
		}
		s.d.LiveTV.ServePlaylist(w, r, chi.URLParam(r, "session"), FromContext(r.Context()).User)
	})
	get("/livetv/sessions/{session}/{consumer}/relay/{segment}", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimSuffix(chi.URLParam(r, "segment"), ".ts"))
		if err != nil {
			writeHTTPError(w, 404)
			return
		}
		s.d.LiveTV.ServeRelaySegment(w, r, chi.URLParam(r, "session"), n, FromContext(r.Context()).User)
	})

	get("/livetv/sessions/{session}/{consumer}/resource/{resource}", func(w http.ResponseWriter, r *http.Request) {
		s.d.LiveTV.ServeResource(w, r, chi.URLParam(r, "session"), chi.URLParam(r, "resource"), FromContext(r.Context()).User)
	})
	get("/livetv/sessions/{session}/{consumer}/program/{start}/{segment}", func(w http.ResponseWriter, r *http.Request) {
		start, e := strconv.ParseInt(chi.URLParam(r, "start"), 10, 64)
		n, e2 := strconv.Atoi(strings.TrimSuffix(chi.URLParam(r, "segment"), ".ts"))
		if e != nil || e2 != nil {
			writeHTTPError(w, 404)
			return
		}
		s.d.LiveTV.ServeSegment(w, r, chi.URLParam(r, "session"), start, n, FromContext(r.Context()).User)
	})
}
func (s *Server) liveDevices(r *http.Request) ([]liveDevice, error) {
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		return nil, err
	}
	maps := []liveMapping{}
	for _, c := range cs {
		maps = append(maps, liveMapping{c.ID, c.Number, "1", c.GuideID})
	}
	return []liveDevice{{Key: "1", Make: "Ether", Model: "Owner channels", Protocol: "livetv", Sources: "IPTV,Virtual", State: "1", Status: "1", Tuners: "4", URI: "device://tv.plex.grabbers.ether/1", UUID: s.identity().MachineIdentifier + "-livetv", ChannelMapping: maps}}, nil
}
func (s *Server) handleLiveDVRs(w http.ResponseWriter, r *http.Request) {
	if id := chi.URLParam(r, "dvr"); id != "" && id != "1" {
		writeHTTPError(w, 404)
		return
	}
	devices, err := s.liveDevices(r)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &liveContainer{Size: 1, Identifier: livetv.Provider, DVR: []liveDVR{{Key: "1", Language: "eng", Lineup: "lineup://" + livetv.Provider + "/dvr", UUID: s.identity().MachineIdentifier + "-livetv", RefreshedAt: s.liveLineupRefreshedAt(), Device: devices}}})
}

// liveLineupRefreshedAt is the clock Apple TV compares to drop a cached lineup.
func (s *Server) liveLineupRefreshedAt() int64 {
	if s.d.LiveTV != nil {
		if at := s.d.LiveTV.LineupChanged(); !at.IsZero() {
			return at.Unix()
		}
	}
	return s.now().Unix()
}

// NotifyLiveLineup tells connected Plex clients to reload the DVR lineup and
// guide. Channel create and delete call it through LiveTV.SetOnChange.
func (s *Server) NotifyLiveLineup() {
	now := s.liveLineupRefreshedAt()
	s.publishNotification(model.NotificationContainer{
		Type: "activity",
		Size: 1,
		ActivityNotification: []model.ActivityNotification{{
			Event: "ended",
			UUID:  "ether-livetv-lineup",
			Activity: &model.Activity{
				UUID:     "ether-livetv-lineup",
				Type:     "livetv.guide",
				Title:    "Live TV lineup",
				Progress: 100,
			},
		}},
	}, 0)
	s.publishNotification(model.NotificationContainer{
		Type: "timeline",
		Size: 1,
		TimelineEntry: []model.TimelineEntry{{
			Identifier: livetv.Provider,
			SectionID:  "1",
			ItemID:     "1",
			Type:       1,
			Title:      "Live TV",
			State:      5,
			UpdatedAt:  now,
		}},
	}, 0)
}
func (s *Server) handleLiveGrabbers(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &liveContainer{Size: 1, Identifier: livetv.Provider, MediaGrabber: []liveGrabber{{Title: "Ether owner channels", Identifier: "tv.plex.grabbers.ether", Protocol: "livetv"}}})
}
func (s *Server) handleLiveDevices(w http.ResponseWriter, r *http.Request) {
	if id := chi.URLParam(r, "device"); id != "" && id != "1" {
		writeHTTPError(w, 404)
		return
	}
	devices, err := s.liveDevices(r)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &liveContainer{Size: len(devices), Identifier: livetv.Provider, Device: devices})
}
func liveLogo(c livetv.Channel) string {
	if c.Logo != "" {
		return c.Logo
	}
	return "/livetv/channels/" + url.PathEscape(c.ID) + "/logo.svg"
}
func (s *Server) handleLiveChannels(w http.ResponseWriter, r *http.Request) {
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	out := []liveChannel{}
	maps := []liveMapping{}
	for _, c := range cs {
		out = append(out, liveChannel{ID: c.ID, GridKey: c.ID, VCN: c.Number, Thumb: liveLogo(c), Title: c.Name, CallSign: c.Name, Language: "eng", Key: liveProviderPath + "/grid?channelGridKey=" + url.QueryEscape(c.ID)})
		maps = append(maps, liveMapping{c.ID, c.Number, "1", c.GuideID})
	}
	s.write(w, r, &liveContainer{Size: len(out), Identifier: livetv.Provider, Channel: out, ChannelMapping: maps})
}
func liveProgrammeMetadata(c livetv.Channel, p livetv.Programme) model.Metadata {
	dur := p.End.Sub(p.Start).Milliseconds()
	rating := p.Item.Rating
	if rating == "" {
		rating = c.Rating
	}
	return model.Metadata{RatingKey: p.ID, Key: liveProviderPath + "/metadata/" + url.PathEscape(p.ID), GUID: "ether://livetv/" + p.ID, Type: "clip", Title: p.Item.Title, Summary: p.Item.Summary, ContentRating: rating, Thumb: p.Item.Thumb, Duration: dur, Extra: []model.Attr{model.A("live", true)}, Media: []model.Media{{ID: p.Start.Unix(), Duration: dur, Protocol: "livetv", Extra: []model.Attr{model.A("beginsAt", p.Start.Unix()), model.A("endsAt", p.End.Unix()), model.A("channelIdentifier", c.ID), model.A("channelID", c.ID), model.A("channelTitle", c.Name), model.A("channelCallSign", c.Name), model.A("channelVcn", c.Number), model.A("channelThumb", liveLogo(c)), model.A("gridKey", c.ID)}}}}
}

// errLiveBadRequest marks a guide query the client got wrong (400, not 500).
var errLiveBadRequest = errors.New("invalid guide request")

func (s *Server) liveGuideMetadata(r *http.Request) ([]model.Metadata, error) {
	return s.liveGuideMetadataCapture(r, nil)
}

func (s *Server) liveGuideMetadataCapture(r *http.Request, captured *[]livetv.Programme) ([]model.Metadata, error) {
	from := s.now()
	until := from.Add(time.Second)
	q := r.URL.Query()
	if date := q.Get("date"); date != "" {
		var err error
		from, err = time.Parse("2006-01-02", date)
		if err != nil {
			return nil, fmt.Errorf("%w: date", errLiveBadRequest)
		}
		until = from.Add(24 * time.Hour)
	}
	if raw := q.Get("beginsAt"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: beginsAt", errLiveBadRequest)
		}
		from = time.Unix(n, 0)
		until = from.Add(24 * time.Hour)
	}
	if raw := q.Get("endsAt"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: endsAt", errLiveBadRequest)
		}
		until = time.Unix(n, 0)
	}
	if id := chi.URLParam(r, "programme"); id != "" {
		dash := strings.LastIndexByte(id, '-')
		if dash < 0 {
			return nil, core.ErrNotFound
		}
		ms, err := strconv.ParseInt(id[dash+1:], 10, 64)
		if err == nil {
			from = time.UnixMilli(ms)
			until = from.Add(time.Second)
		} else {
			from = s.now().Add(-24 * time.Hour)
			until = s.now().Add(6 * 24 * time.Hour)
		}
	}
	if !until.After(from) || until.Sub(from) > 7*24*time.Hour {
		return nil, fmt.Errorf("%w: window", errLiveBadRequest)
	}
	ps, err := s.d.LiveTV.Guide(r.Context(), FromContext(r.Context()).User, from, until)
	if err != nil {
		return nil, err
	}
	cs, err := s.d.LiveTV.Channels(r.Context())
	if err != nil {
		return nil, err
	}
	byID := map[string]livetv.Channel{}
	for _, c := range cs {
		byID[c.ID] = c
	}
	out := []model.Metadata{}
	for _, p := range ps {
		if id := chi.URLParam(r, "programme"); id != "" && p.ID != id {
			continue
		}
		if id := q.Get("channelGridKey"); id != "" && p.ChannelID != id {
			continue
		}
		out = append(out, liveProgrammeMetadata(byID[p.ChannelID], p))
		if captured != nil {
			*captured = append(*captured, p)
		}
	}
	return out, nil
}
func (s *Server) handleLiveGuide(w http.ResponseWriter, r *http.Request) {
	md, err := s.liveGuideMetadata(r)
	if errors.Is(err, errLiveBadRequest) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if chi.URLParam(r, "programme") != "" && len(md) == 0 {
		writeHTTPError(w, 404)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: len(md), Identifier: livetv.Provider, Metadata: md})
}
func (s *Server) handleLiveRoot(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{Size: 2, Identifier: livetv.Provider, Directory: []model.Directory{{Key: liveProviderPath + "/lineups/dvr/channels", Title: "Guide", Type: "livetv"}, {Key: liveProviderPath + "/watchnow/all", Title: "On Now", Type: "mixed"}}})
}
func (s *Server) handleLiveWatchNow(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{Size: 1, Identifier: livetv.Provider, Type: []model.MetaType{{Active: true, Key: liveProviderPath + "/watchnow/all", Title: "All Channels", Type: "mixed"}}})
}
func (s *Server) liveTVHub(r *http.Request) *model.Hub {
	h, _ := s.liveTVHubProjection(r)
	return h
}

func (s *Server) liveTVHubProjection(r *http.Request) (*model.Hub, frontend.Row) {
	if !s.liveTVEnabled() {
		return nil, frontend.Row{}
	}
	var programmes []livetv.Programme
	md, err := s.liveGuideMetadataCapture(r, &programmes)
	if err != nil {
		return nil, frontend.Row{}
	}
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		return nil, frontend.Row{}
	}
	found := map[string]bool{}
	for _, item := range md {
		for _, media := range item.Media {
			for _, a := range media.Extra {
				if a.Name == "channelIdentifier" {
					if id, ok := a.Value.(string); ok {
						found[id] = true
					}
				}
			}
		}
	}
	for _, c := range cs {
		if !found[c.ID] {
			md = append(md, s.liveChannelMetadata(c))
		}
	}
	if len(md) == 0 {
		return nil, frontend.Row{}
	}
	if len(md) > 24 {
		md = md[:24]
	}
	h := &model.Hub{Key: liveProviderPath + "/watchnow/all", Title: "On Now", Type: "mixed", HubIdentifier: "livetv.onnow", Context: "hub.livetv.onnow", Size: len(md), Style: "shelf", Promoted: model.Ptr(true), Metadata: md}
	row := frontend.Row{ID: "livetv.onnow", Title: "On Now", Purpose: frontend.RowLive, Style: "shelf", Promoted: true}
	channels := make(map[string]livetv.Channel, len(cs))
	for _, c := range cs {
		channels[c.ID] = c
	}
	for _, item := range md {
		var id string
		for _, media := range item.Media {
			for _, attr := range media.Extra {
				if attr.Name == "channelIdentifier" {
					id, _ = attr.Value.(string)
				}
			}
		}
		c, ok := channels[id]
		if !ok {
			continue
		}
		entry := &frontend.RowLiveChannel{ID: c.ID, Number: c.Number, Name: c.Name, Logo: liveLogo(c), GuideReference: c.GuideID}
		now := s.now()
		for _, p := range programmes {
			if p.ChannelID != id || !p.End.After(now) {
				continue
			}
			programme := &frontend.RowProgramme{ID: p.ID, Title: p.Item.Title, Start: p.Start, End: p.End, Item: p.Item.Key}
			if !p.Start.After(now) {
				if entry.Current == nil || p.Start.Before(entry.Current.Start) {
					entry.Current = programme
				}
			} else if entry.Next == nil || p.Start.Before(entry.Next.Start) {
				entry.Next = programme
			}
		}
		row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryLiveChannel, LiveChannel: entry})
	}
	return h, row
}
func (s *Server) liveChannelMetadata(c livetv.Channel) model.Metadata {
	return model.Metadata{RatingKey: "channel-" + c.ID, Key: liveProviderPath + "/channels/" + url.PathEscape(c.ID), Type: "clip", Title: c.Name, Summary: "Live channel. No programme guide supplied.", Thumb: liveLogo(c), ContentRating: c.Rating, Extra: []model.Attr{model.A("live", true), model.A("dvrChannel", true)}, Media: []model.Media{{Protocol: "livetv", Extra: []model.Attr{model.A("origin", "livetv"), model.A("channelIdentifier", c.ID), model.A("channelTitle", c.Name), model.A("channelVcn", c.Number), model.A("onAir", true)}}}}
}
func (s *Server) handleLiveChannel(w http.ResponseWriter, r *http.Request) {
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	for _, c := range cs {
		if c.ID == chi.URLParam(r, "channel") {
			s.write(w, r, &model.MediaContainer{Size: 1, Identifier: livetv.Provider, Metadata: []model.Metadata{s.liveChannelMetadata(c)}})
			return
		}
	}
	writeHTTPError(w, 404)
}

func (s *Server) handleLiveHubs(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{Identifier: livetv.Provider}
	if h := s.liveTVHub(r); h != nil {
		mc.Hub = []model.Hub{*h}
		mc.Size = 1
	}
	s.write(w, r, mc)
}
func (s *Server) liveSessionMetadata(info livetv.SessionInfo) model.Metadata {
	md := model.Metadata{RatingKey: info.ID, Key: "/livetv/sessions/" + info.ID, GUID: "plex://livetv/session/" + info.ID, Type: "clip", Title: info.Channel.Name, Summary: "", Extra: []model.Attr{model.A("live", true), model.A("genuineMediaAnalysis", false)}}
	if info.Programme != nil {
		md.Title = info.Programme.Item.Title
		md.Summary = info.Programme.Item.Summary
		md.Thumb = info.Programme.Item.Thumb
	}
	md.Media = []model.Media{{Protocol: "hls", Container: "mpegts", Extra: []model.Attr{model.A("uuid", info.ID), model.A("origin", "livetv"), model.A("beginsAt", info.StartedAt.Unix()), model.A("channelIdentifier", info.Channel.ID), model.A("channelTitle", info.Channel.Name), model.A("channelCallSign", info.Channel.Name), model.A("channelVcn", info.Channel.Number), model.A("onAir", true)}, Part: []model.Part{{Key: "/livetv/sessions/" + info.ID + "/0/index.m3u8", Container: "mpegts", Protocol: "hls", Decision: "directplay"}}}}
	if info.MediaInfo != nil && info.MediaInfo.Video() != nil {
		md.Extra = []model.Attr{model.A("live", true), model.A("genuineMediaAnalysis", true)}
		live := md.Media[0]
		m := buildMedia(&core.Title{Title: info.Channel.Name}, *info.MediaInfo, info.StartedAt.Unix())
		m.Extra = live.Extra
		m.Protocol = "hls"
		m.Container = "mpegts"
		m.ID = tagID("live", info.ID) + 1
		m.Duration = 0
		m.Part[0] = live.Part[0]
		m.Part[0].ID = m.ID
		m.Part[0].Stream = s.buildStreams(0, *info.MediaInfo, selection{}, nil)
		md.Media = []model.Media{m}
	}
	return md
}
func (s *Server) handleLiveTune(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "dvr") != "1" {
		writeHTTPError(w, 404)
		return
	}
	pr := FromContext(r.Context())
	channel := chi.URLParam(r, "channel")
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), pr.User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	id := ""
	for _, c := range cs {
		if c.ID == channel || c.Number == channel {
			id = c.ID
			break
		}
	}
	if id == "" {
		writeHTTPError(w, 404)
		return
	}
	info, err := s.d.LiveTV.TuneWithSession(r.Context(), pr.User, id, s.clientCaps(pr), pr.SessionID)
	if err != nil {
		s.log.Warn("live tune failed", "channel", id, "client", pr.ClientID, "product", pr.Product, "err", err)
		if errors.Is(err, context.DeadlineExceeded) {
			w.Header().Set("Retry-After", "1")
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		}
		s.writeErr(w, r, err)
		return
	}
	s.log.Info("live tune ready", "channel", info.Channel.Name, "session", info.ID, "client", pr.ClientID, "product", pr.Product, "analyzed", info.MediaInfo != nil)
	s.writeLiveSubscription(w, r, info)
}
func (s *Server) handleLiveSessions(w http.ResponseWriter, r *http.Request) {
	infos := s.d.LiveTV.Sessions(r.Context(), FromContext(r.Context()).User)
	md := []model.Metadata{}
	for _, info := range infos {
		md = append(md, s.liveSessionMetadata(info))
	}
	s.write(w, r, &model.MediaContainer{Size: len(md), Identifier: livetv.Provider, Metadata: md})
}
func (s *Server) handleLiveSession(w http.ResponseWriter, r *http.Request) {
	info, err := s.d.LiveTV.Session(r.Context(), chi.URLParam(r, "session"), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, Identifier: livetv.Provider, Metadata: []model.Metadata{s.liveSessionMetadata(info)}})
}

// handleLiveTVPlayback hooks the universal decision/start endpoints before the
// catalogue parser. Plex passes the tune response's /livetv/sessions key here.
func (s *Server) handleLiveTVPlayback(w http.ResponseWriter, r *http.Request) bool {
	if s.refuseCapability(w, r) {
		return true
	}
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/livetv/sessions/") {
		return false
	}
	if !s.liveTVEnabled() {
		writeHTTPError(w, 404)
		return true
	}
	pr := FromContext(r.Context())
	if p, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !p.CanPlay(pr.User, pr.Remote, s.now()) {
		writeHTTPError(w, 403)
		return true
	}
	id := strings.TrimPrefix(path, "/livetv/sessions/")
	if strings.Contains(id, "/") {
		writeHTTPError(w, 404)
		return true
	}
	info, err := s.d.LiveTV.Session(r.Context(), id, FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/decision") {
		s.write(w, r, &model.MediaContainer{Size: 1, Identifier: livetv.Provider, Extra: []model.Attr{model.A("generalDecisionCode", 1001), model.A("generalDecisionText", "Live TV stream available."), model.A("transcodeDecisionCode", 1001), model.A("transcodeDecisionText", "Live TV HLS streaming.")}, Metadata: []model.Metadata{s.liveSessionMetadata(info)}})
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/start.mpd") {
		if err := s.d.LiveTV.ServeDASH(w, r, id, pr.User); err != nil {
			s.writeLiveDASHError(w, r, info, err)
		}
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/start") && r.URL.Query().Get("protocol") == core.ProtocolHTTP {
		if err := s.d.LiveTV.ServeHTTP(w, r, id, pr.User); err != nil {
			s.writeErr(w, r, err)
		}
		return true
	}
	// Native players can retry this endpoint without following a redirect.
	// The Live TV playlist already uses rooted, authenticated child URLs.
	s.d.LiveTV.ServePlaylist(w, r, id, pr.User)
	return true
}

// writeLiveDASHError is the Plex decision document for a virtual channel that
// cannot produce DASH. Plex Web treats a bare 503 as s1001 (Network) and never
// shows the reason. Decision codes at or above 2000 are the message it displays.
func (s *Server) writeLiveDASHError(w http.ResponseWriter, r *http.Request, info livetv.SessionInfo, err error) {
	var dashErr *livetv.VirtualDASHError
	if !errors.As(err, &dashErr) {
		s.writeErr(w, r, err)
		return
	}
	msg := "This live channel cannot be played as DASH."
	if dashErr.Message != "" {
		msg = dashErr.Message
	} else if dashErr.Err != nil && !errors.Is(dashErr.Err, core.ErrNotAvailable) {
		msg = playbackMessage(dashErr.Err)
	}
	s.write(w, r, &model.MediaContainer{
		Size: 1, Identifier: livetv.Provider,
		Extra: []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayError),
			model.A("directPlayDecisionText", msg),
			model.A("generalDecisionCode", codeGeneralError),
			model.A("generalDecisionText", msg),
			model.A("transcodeDecisionCode", codeTranscodeError),
			model.A("transcodeDecisionText", msg),
		},
		Metadata: []model.Metadata{s.liveSessionMetadata(info)},
	})
}
func (s *Server) handleLiveLogo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "channel")
	cs, err := s.d.LiveTV.AllowedChannels(r.Context(), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	for _, c := range cs {
		if c.ID == id {
			setArtworkHeaders(w.Header(), "image/svg+xml")
			w.Header().Set("Cache-Control", "private, max-age=300")
			if r.Method != "HEAD" {
				fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 640 360"><rect width="640" height="360" rx="24" fill="#182d45"/><text x="40" y="125" fill="#64dfc7" font-size="68" font-family="sans-serif">%s</text><text x="40" y="220" fill="white" font-size="30" font-family="sans-serif">%s</text></svg>`, html.EscapeString(c.Number), html.EscapeString(c.Name))
			}
			return
		}
	}
	writeHTTPError(w, 404)
}

// Plex Web 4.160's LiveTVSessionModel consumes the active subscription and
// grab operation in addition to the OpenAPI's top-level Metadata sample. The
// activity ID is the real Ether live session's identity, not a Plex entitlement.
type liveOperation struct {
	Key          string         `plex:"key"`
	ID           string         `plex:"id"`
	Subscription string         `plex:"mediaSubscriptionID"`
	Provider     string         `plex:"provider"`
	MediaIndex   int            `plex:"mediaIndex"`
	Status       string         `plex:"status"`
	Rolling      bool           `plex:"rolling"`
	Metadata     model.Metadata `plex:"Metadata,object"`
}
type liveTimeline struct {
	Metadata []model.Metadata `plex:"Metadata"`
}
type liveSubscription struct {
	Key       string        `plex:"key"`
	Type      int           `plex:"type"`
	Title     string        `plex:"title"`
	Operation liveOperation `plex:"MediaGrabOperation,object"`
	Timeline  liveTimeline  `plex:"Timeline,object"`
}
type liveSubscriptionContainer struct {
	Size         int                `plex:"size"`
	Identifier   string             `plex:"identifier"`
	Metadata     []model.Metadata   `plex:"Metadata"`
	Subscription []liveSubscription `plex:"MediaSubscription"`
	Operation    *liveOperation     `plex:"MediaGrabOperation,object"`
	Extra        []model.Attr
}

func (s *Server) liveSubscriptionFor(info livetv.SessionInfo) liveSubscription {
	md := s.liveSessionMetadata(info)
	timeline := md
	if info.Programme != nil {
		timeline = liveProgrammeMetadata(info.Channel, *info.Programme)
		timeline.Media[0].Extra = append(timeline.Media[0].Extra, model.A("onAir", true))
	}
	op := liveOperation{Key: "/media/grabbers/operations/" + info.ID, ID: info.ID, Subscription: info.ID, Provider: livetv.Provider, Status: "inprogress", Rolling: true, Metadata: md}
	return liveSubscription{Key: info.ID, Type: 12, Title: info.Channel.Name, Operation: op, Timeline: liveTimeline{Metadata: []model.Metadata{timeline}}}
}

func (s *Server) writeLiveSubscription(w http.ResponseWriter, r *http.Request, info livetv.SessionInfo) {
	if nativeLiveRequest(r) {
		s.writeNativeLiveTune(w, r, info)
		return
	}
	w.Header().Set("X-Plex-Activity", "livetv-"+info.ID)
	s.write(w, r, &liveSubscriptionContainer{Size: 1, Identifier: livetv.Provider, Metadata: []model.Metadata{s.liveSessionMetadata(info)}, Subscription: []liveSubscription{s.liveSubscriptionFor(info)}})
}
func (s *Server) handleLiveSubscriptions(w http.ResponseWriter, r *http.Request) {
	infos := s.d.LiveTV.Sessions(r.Context(), FromContext(r.Context()).User)
	if nativeLiveRequest(r) {
		out := []nativeLiveSubscription{}
		for _, info := range infos {
			out = append(out, s.nativeLiveSubscriptionFor(info))
		}
		s.write(w, r, &nativeLiveContainer{Size: len(out), Subscriptions: out})
		return
	}
	out := []liveSubscription{}
	for _, info := range infos {
		out = append(out, s.liveSubscriptionFor(info))
	}
	s.write(w, r, &liveSubscriptionContainer{Size: len(out), Identifier: livetv.Provider, Subscription: out})
}
func (s *Server) handleLiveSubscription(w http.ResponseWriter, r *http.Request) {
	info, err := s.liveSubscriptionSession(r, chi.URLParam(r, "subscription"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeLiveSubscription(w, r, info)
}
func (s *Server) handleLiveSubscriptionStop(w http.ResponseWriter, r *http.Request) {
	info, err := s.liveSubscriptionSession(r, chi.URLParam(r, "subscription"))
	if err == nil {
		err = s.d.LiveTV.Stop(r.Context(), info.ID, FromContext(r.Context()).User)
	}
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeEmpty(w, r)
}
func (s *Server) handleLiveOperation(w http.ResponseWriter, r *http.Request) {
	info, err := s.d.LiveTV.Session(r.Context(), chi.URLParam(r, "operation"), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if nativeLiveRequest(r) {
		s.write(w, r, &nativeLiveContainer{Size: 1, Grabs: s.nativeLiveSubscriptionFor(info).Grab})
		return
	}
	op := s.liveSubscriptionFor(info).Operation
	s.write(w, r, &liveSubscriptionContainer{Size: 1, Identifier: livetv.Provider, Operation: &op})
}
func (s *Server) handleLiveOperationStop(w http.ResponseWriter, r *http.Request) {
	err := s.d.LiveTV.Stop(r.Context(), chi.URLParam(r, "operation"), FromContext(r.Context()).User)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeEmpty(w, r)
}
