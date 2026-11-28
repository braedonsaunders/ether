package xtream

import (
	"bufio"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lineup"
)

func xtreamOwnerGroupRef(id string) string {
	return "owner\n" + id
}

func (i *instance) channels(r *http.Request, l login) ([]frontend.LiveChannel, error) {
	cs, err := i.host.LiveTV.Channels(r.Context(), l.viewer)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(cs); start += 250 {
		refs := []string{}
		groups := []string{}
		seen := map[string]bool{}
		for _, c := range cs[start:min(start+250, len(cs))] {
			refs = append(refs, c.ID)
			if len(c.Groups) > 0 {
				for _, g := range c.Groups {
					ref := xtreamOwnerGroupRef(g.ID)
					if !seen[ref] {
						groups = append(groups, ref)
						seen[ref] = true
					}
				}
				continue
			}
			if group := strings.TrimSpace(c.Group); group != "" && !seen[group] {
				groups = append(groups, group)
				seen[group] = true
			}
		}
		allocated, err := i.host.IDs.CompactBatch(r.Context(), "xtream", "channel", refs)
		if err != nil {
			return nil, err
		}
		for ref, id := range allocated {
			l.channels[ref] = id
		}
		if len(groups) > 0 {
			allocated, err := i.host.IDs.CompactBatch(r.Context(), "xtream", "live-group", groups)
			if err != nil {
				return nil, err
			}
			for ref, id := range allocated {
				if id <= 0 || id > 2147483645 {
					return nil, fmt.Errorf("invalid live group identity")
				}
				l.groups[ref] = id + 2 // preserve the existing ungrouped categories 1 and 2
			}
		}
	}
	return cs, nil
}

// channelArtwork prepares just the logos the current response will render.
func (i *instance) channelArtwork(r *http.Request, l login, channels []frontend.LiveChannel, category string) *http.Request {
	batch, ok := i.host.ImageGrants.(frontend.ChannelArtworkGrantBatch)
	if !ok {
		return r
	}
	var ids []string
	for _, c := range channels {
		if len(c.Images) > 0 && liveChannelInCategory(l, c, category) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return r
	}
	ctx, err := batch.PrepareChannelArtwork(r.Context(), l.credential, ids)
	if err != nil {
		return r
	}
	return r.WithContext(ctx)
}

func liveCategoryInt(l login, c frontend.LiveChannel) int32 {
	if len(c.Groups) > 0 {
		if id := l.groups[xtreamOwnerGroupRef(c.Groups[0].ID)]; id > 0 {
			return id
		}
	}
	if id := l.groups[strings.TrimSpace(c.Group)]; id > 0 {
		return id
	}
	if c.Kind == "iptv" {
		return 2
	}
	return 1
}

func liveCategory(l login, c frontend.LiveChannel) string {
	return strconv.FormatInt(int64(liveCategoryInt(l, c)), 10)
}

func liveCategoryIDs(l login, c frontend.LiveChannel) []int32 {
	if len(c.Groups) > 0 {
		out := make([]int32, 0, len(c.Groups))
		for _, g := range c.Groups {
			if id := l.groups[xtreamOwnerGroupRef(g.ID)]; id > 0 {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []int32{liveCategoryInt(l, c)}
}

func liveChannelInCategory(l login, c frontend.LiveChannel, cat string) bool {
	if cat == "" || cat == "0" {
		return true
	}
	if len(c.Groups) > 0 {
		for _, g := range c.Groups {
			if id := l.groups[xtreamOwnerGroupRef(g.ID)]; id > 0 && strconv.FormatInt(int64(id), 10) == cat {
				return true
			}
		}
		return false
	}
	return cat == liveCategory(l, c)
}

func (i *instance) live(w http.ResponseWriter, r *http.Request, l login, action string) {
	channels, err := i.channels(r, l)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if action == "get_live_categories" {
		out := []map[string]any{}
		seen := map[string]bool{}
		var ownerGroups []frontend.LiveGroup
		if lg, ok := i.host.LiveTV.(frontend.LiveTVGroups); ok {
			ownerGroups, err = lg.Groups(r.Context(), l.viewer)
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
		} else {
			seenOwner := map[string]bool{}
			for _, c := range channels {
				for _, g := range c.Groups {
					if !seenOwner[g.ID] {
						seenOwner[g.ID] = true
						ownerGroups = append(ownerGroups, g)
					}
				}
			}
		}
		for _, g := range ownerGroups {
			if id := l.groups[xtreamOwnerGroupRef(g.ID)]; id > 0 {
				catID := strconv.FormatInt(int64(id), 10)
				if !seen[catID] {
					seen[catID] = true
					out = append(out, map[string]any{"category_id": catID, "category_name": g.Name, "parent_id": 0})
				}
			}
		}
		for _, c := range channels {
			if len(c.Groups) > 0 {
				continue
			}
			id := liveCategory(l, c)
			if !seen[id] {
				seen[id] = true
				name := "On Air"
				if id == "2" {
					name = "IPTV"
				}
				if group := strings.TrimSpace(c.Group); group != "" {
					name = group
				}
				out = append(out, map[string]any{"category_id": id, "category_name": name, "parent_id": 0})
			}
		}
		jsonResponse(w, out)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("["))
	first := true
	num := 0
	catFilter := r.URL.Query().Get("category_id")
	r = i.channelArtwork(r, l, channels, catFilter)
	for _, c := range channels {
		if !liveChannelInCategory(l, c, catFilter) {
			continue
		}
		if !first {
			_, _ = w.Write([]byte(","))
		}
		first = false
		num++
		archive := 0
		if c.ArchiveDays > 0 {
			archive = 1
		}
		_ = jsonEncoder(w, map[string]any{"num": num, "name": c.Name, "stream_type": "live", "stream_id": l.channels[c.ID], "stream_icon": i.image(r, l, 0, c.ID, c.Images), "epg_channel_id": c.ID, "added": "0", "category_id": liveCategory(l, c), "category_ids": liveCategoryIDs(l, c), "custom_sid": "", "tv_archive": archive, "tv_archive_duration": c.ArchiveDays, "direct_source": ""})
	}
	_, _ = w.Write([]byte("]"))
}

func (i *instance) findChannel(r *http.Request, l login, id string) (frontend.LiveChannel, bool) {
	n, err := parseCompactID(id)
	if err != nil {
		return frontend.LiveChannel{}, false
	}
	ref, err := i.host.IDs.Resolve(r.Context(), "xtream", "channel", n)
	if err != nil {
		return frontend.LiveChannel{}, false
	}
	var channels []frontend.LiveChannel
	if selectChannels, ok := i.host.LiveTV.(frontend.LiveChannelSelection); ok {
		channels, err = selectChannels.ReadChannelSelection(r.Context(), l.viewer, []string{ref})
	} else {
		channels, err = i.host.LiveTV.Channels(r.Context(), l.viewer)
	}
	if err != nil {
		return frontend.LiveChannel{}, false
	}
	for _, c := range channels {
		if c.ID == ref {
			return c, true
		}
	}
	return frontend.LiveChannel{}, false
}

func (i *instance) liveNow() time.Time {
	if i.host.Now != nil {
		return i.host.Now().UTC()
	}
	return time.Now().UTC()
}

func (i *instance) programmes(r *http.Request, l login, c frontend.LiveChannel, from, until time.Time) ([]frontend.LiveProgramme, error) {
	return i.host.LiveTV.Guide(r.Context(), l.viewer, frontend.LiveGuideQuery{Device: l.device, Channels: []string{c.ID}, From: from, Until: until, Limit: 10000})
}

func (i *instance) programmeIcon(r *http.Request, l login, p frontend.LiveProgramme) string {
	return frontend.ProgrammeIconURL(r.Context(), i.host.ImageGrants, i.base(r), p, l.credential, l.device)
}

func (i *instance) epg(w http.ResponseWriter, r *http.Request, l login, action string) {
	streamID := r.URL.Query().Get("stream_id")
	c, ok := i.findChannel(r, l, streamID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	now := i.liveNow()
	from := now.Add(-6 * time.Hour)
	until := now.Add(48 * time.Hour)
	if action == "get_simple_data_table" && c.ArchiveDays > 0 {
		from = now.AddDate(0, 0, -min(c.ArchiveDays, 30))
	}
	ps, err := i.programmes(r, l, c, from, until)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	limit := 10000
	if action == "get_short_epg" {
		limit = 4
		if n, e := strconv.Atoi(r.URL.Query().Get("limit")); e == nil && n > 0 {
			limit = min(n, 1000)
		}
	}
	out := []map[string]any{}
	for _, p := range ps {
		if p.ChannelID != c.ID || !p.End.After(p.Start) || action == "get_short_epg" && !p.End.After(now) {
			continue
		}
		playing := 0
		if !p.Start.After(now) && p.End.After(now) {
			playing = 1
		}
		hasArchive := 0
		if ended, _ := frontend.ArchiveFlags(c.ArchiveDays, p.Start, p.End, now); ended {
			hasArchive = 1
		}
		row := map[string]any{"id": p.ID, "epg_id": streamID, "channel_id": c.ID, "title": base64.StdEncoding.EncodeToString([]byte(p.Title)), "description": base64.StdEncoding.EncodeToString([]byte(p.Summary)), "lang": "", "start": p.Start.UTC().Format("2006-01-02 15:04:05"), "end": p.End.UTC().Format("2006-01-02 15:04:05"), "start_timestamp": strconv.FormatInt(p.Start.Unix(), 10), "stop_timestamp": strconv.FormatInt(p.End.Unix(), 10), "now_playing": playing, "has_archive": hasArchive}
		if icon := i.programmeIcon(r, l, p); icon != "" {
			row["icon"] = icon
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	jsonResponse(w, map[string]any{"epg_listings": out})
}

type programmeXMLIcon struct {
	Source string `xml:"src,attr"`
}

func (i *instance) xmltv(w http.ResponseWriter, r *http.Request, l login) {
	cs, err := i.host.LiveTV.Channels(r.Context(), l.viewer)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header + "<tv>"))
	e := xml.NewEncoder(w)
	for _, c := range cs {
		_ = e.Encode(struct {
			XMLName xml.Name `xml:"channel"`
			ID      string   `xml:"id,attr"`
			Name    string   `xml:"display-name"`
		}{ID: c.ID, Name: c.Name})
	}
	now := i.liveNow()
	const batchSize = frontend.MaxLiveGuideExportChannels
	for start := 0; start < len(cs); start += batchSize {
		batch := cs[start:min(start+batchSize, len(cs))]
		ids := make([]string, len(batch))
		for j, c := range batch {
			ids[j] = c.ID
		}
		ps, err := i.host.LiveTV.Guide(r.Context(), l.viewer, frontend.LiveGuideQuery{
			Device: l.device, Channels: ids, From: now.Add(-6 * time.Hour), Until: now.Add(48 * time.Hour),
			Limit: 10000, PerChannelLimit: true,
		})
		if err != nil {
			return
		}
		byChannel := make(map[string][]frontend.LiveProgramme, len(batch))
		for _, p := range ps {
			byChannel[p.ChannelID] = append(byChannel[p.ChannelID], p)
		}
		for _, channel := range batch {
			for _, p := range byChannel[channel.ID] {
				if !p.End.After(p.Start) {
					continue
				}
				var icon *programmeXMLIcon
				if src := i.programmeIcon(r, l, p); src != "" {
					icon = &programmeXMLIcon{Source: src}
				}
				_ = e.Encode(struct {
					XMLName xml.Name          `xml:"programme"`
					Channel string            `xml:"channel,attr"`
					Start   string            `xml:"start,attr"`
					Stop    string            `xml:"stop,attr"`
					Title   string            `xml:"title"`
					Desc    string            `xml:"desc"`
					Icon    *programmeXMLIcon `xml:"icon,omitempty"`
				}{Icon: icon, Channel: channel.ID, Start: p.Start.UTC().Format("20060102150405 -0700"), Stop: p.End.UTC().Format("20060102150405 -0700"), Title: p.Title, Desc: p.Summary})
			}
		}
	}
	_ = e.Flush()
	_, _ = w.Write([]byte("</tv>"))
}

func (i *instance) playlist(w http.ResponseWriter, r *http.Request, l login) {
	base := i.base(r)
	if base == "" {
		http.Error(w, "Unavailable", 503)
		return
	}
	var cached io.ReadCloser
	if i.lineups != nil {
		var err error
		cached, err = i.lineups.Open(r.Context(), lineup.ViewerKey(l.viewer, r.URL.Query().Get("category_id")))
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		defer cached.Close()
	}
	cs, err := i.channels(r, l)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	r = i.channelArtwork(r.WithContext(frontend.WithExportTitlesOnly(r.Context())), l, cs, "")
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	out := bufio.NewWriterSize(w, 64<<10)
	_, _ = fmt.Fprintln(out, "#EXTM3U")
	ext := "m3u8"
	if output := r.URL.Query().Get("output"); strings.EqualFold(output, "ts") || strings.EqualFold(output, "mpegts") {
		ext = "ts"
	}
	for _, c := range cs {
		if err := lineup.WriteLive(out, c, i.image(r, l, 0, c.ID, c.Images), base, l.username, l.password, l.channels[c.ID], ext); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	if cached != nil {
		err = lineup.WriteSnapshot(out, cached, base, l.username, l.password)
	} else {
		err = i.writeLineup(r, l, out, base)
	}
	if err != nil || out.Flush() != nil {
		panic(http.ErrAbortHandler)
	}
}
