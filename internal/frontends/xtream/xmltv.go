package xtream

import (
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lru"
)

type programmeXMLIcon struct {
	Source string `xml:"src,attr"`
}

func (i *instance) xmltv(w http.ResponseWriter, r *http.Request, l login) {
	// Write the complete document before committing a response. Guide
	// exports can be large, so stage them on disk one batch at a time.
	f, err := os.CreateTemp("", "ether-xmltv-*")
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := i.writeXMLTV(f, r, l); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.Copy(w, f)
}

func (i *instance) writeXMLTV(w io.Writer, r *http.Request, l login) error {
	cs, err := i.host.LiveTV.Channels(r.Context(), l.viewer)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, xml.Header+"<tv>"); err != nil {
		return err
	}
	e := xml.NewEncoder(w)
	for _, c := range cs {
		if err := e.Encode(struct {
			XMLName xml.Name `xml:"channel"`
			ID      string   `xml:"id,attr"`
			Name    string   `xml:"display-name"`
		}{ID: c.ID, Name: c.Name}); err != nil {
			return err
		}
	}
	now := i.liveNow()
	const batchSize = frontend.MaxLiveGuideExportChannels
	for start := 0; start < len(cs); start += batchSize {
		batch := cs[start:min(start+batchSize, len(cs))]
		ids := make([]string, len(batch))
		for j, c := range batch {
			ids[j] = c.ID
		}
		ps, err := i.xmltvGuide(r, l, now, frontend.LiveGuideQuery{
			Device: l.device, Channels: ids, From: now.Add(-6 * time.Hour), Until: now.Add(48 * time.Hour),
			Limit: 10000, PerChannelLimit: true,
		})
		if err != nil {
			return err
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
				if err := e.Encode(struct {
					XMLName xml.Name          `xml:"programme"`
					Channel string            `xml:"channel,attr"`
					Start   string            `xml:"start,attr"`
					Stop    string            `xml:"stop,attr"`
					Title   string            `xml:"title"`
					Desc    string            `xml:"desc"`
					Icon    *programmeXMLIcon `xml:"icon,omitempty"`
				}{Icon: icon, Channel: channel.ID, Start: p.Start.UTC().Format("20060102150405 -0700"), Stop: p.End.UTC().Format("20060102150405 -0700"), Title: p.Title, Desc: p.Summary}); err != nil {
					return err
				}
			}
		}
	}
	if err := e.Flush(); err != nil {
		return err
	}
	_, err = io.WriteString(w, "</tv>")
	return err
}

type xmltvGuideKey struct {
	viewer   int64
	device   frontend.Device
	fresh    frontend.PageFreshness
	minute   int64
	channels [frontend.MaxLiveGuideExportChannels]string
}

// Cache programme facts only. Artwork URLs are issued in the current
// request's scope, and channel membership is checked afresh on every export.
func (i *instance) xmltvGuide(r *http.Request, l login, now time.Time, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	if i.host.PageFreshness == nil {
		return i.host.LiveTV.Guide(r.Context(), l.viewer, q)
	}
	key := xmltvGuideKey{viewer: l.viewer.ID, device: l.device, fresh: i.host.PageFreshness(), minute: now.Unix() / 60}
	copy(key.channels[:], q.Channels)
	i.xmltvMu.Lock()
	if i.xmltvCache == nil {
		i.xmltvCache = lru.New(lru.Options[xmltvGuideKey, []frontend.LiveProgramme]{MaxEntries: 128, MaxCost: 32 << 20, Cost: xmltvGuideCost})
	}
	programmes, ok := i.xmltvCache.Get(key)
	i.xmltvMu.Unlock()
	if ok {
		return programmes, nil
	}
	programmes, err := i.host.LiveTV.Guide(r.Context(), l.viewer, q)
	if err != nil {
		return nil, err
	}
	i.xmltvMu.Lock()
	i.xmltvCache.Add(key, programmes)
	i.xmltvMu.Unlock()
	return programmes, nil
}

func xmltvGuideCost(_ xmltvGuideKey, programmes []frontend.LiveProgramme) int64 {
	var cost int64
	for _, p := range programmes {
		cost += int64(512 + len(p.ID) + len(p.ChannelID) + len(p.Title) + len(p.Summary) + len(p.Artwork) + len(p.Rating) + len(p.MediaType) + len(p.ShowTitle) + len(p.EpisodeTitle))
		for _, genre := range p.Genres {
			cost += int64(16 + len(genre))
		}
		for _, im := range p.Images {
			cost += int64(128 + len(im.Kind) + len(im.Path) + len(im.Tag) + len(im.Subject) + len(im.BlurHash) + len(im.ThumbHash))
		}
	}
	return cost
}
