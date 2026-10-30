// Package m3u exports viewer-filtered channels and guide data.
package m3u

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

type Frontend struct{}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "m3u", Name: "M3U and XMLTV", Version: "1.0", Clients: []string{"Channels DVR", "TiviMate"}}
}
func (Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if host.Identity == nil || host.LiveTV == nil || host.Sessions == nil {
		return nil, errors.New("m3u requires identity, live TV and sessions")
	}
	i := &instance{host: host}
	if err := i.registerRoutes(); err != nil {
		return nil, err
	}
	return i, nil
}

// RegisteredRoutes enumerates the same routes mounted at runtime.
func RegisteredRoutes() ([]route.Route, error) {
	i := &instance{}
	if err := i.registerRoutes(); err != nil {
		return nil, err
	}
	return i.router.Routes(), nil
}

func (i *instance) registerRoutes() error {
	i.router = &route.Router{}
	if err := i.router.Handle(http.MethodGet, "/{credential}/guide.xml", http.HandlerFunc(i.guide)); err != nil {
		return err
	}
	if err := i.router.Handle(http.MethodGet, "/{credential}/playlist.m3u", http.HandlerFunc(i.playlist)); err != nil {
		return err
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if err := i.router.Handle(method, "/sessions/lineup/{tail...}", http.HandlerFunc(i.serveLazy)); err != nil {
			return err
		}
	}
	if err := i.router.Handle(http.MethodGet, "/image/{secret}", http.HandlerFunc(i.image)); err != nil {
		return err
	}
	return nil
}

type instance struct {
	host   frontend.Host
	router *route.Router
}

func (i *instance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/m3u/") {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Path = strings.TrimPrefix(u.Path, "/m3u")
			r.URL = &u
		}
		i.router.ServeHTTP(w, r)
	})
}
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/m3u"}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (*instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "XMLTV", Conformance: "partial"}
}
func (*instance) Close(context.Context) error { return nil }
func (i *instance) Routes() []route.Route     { return i.router.Routes() }

type xmlChannel struct {
	ID   string   `xml:"id,attr"`
	Name string   `xml:"display-name"`
	Icon *xmlIcon `xml:"icon,omitempty"`
}
type xmlIcon struct {
	Src string `xml:"src,attr"`
}
type xmlEpisode struct {
	System string `xml:"system,attr"`
	Value  string `xml:",chardata"`
}
type xmlProgramme struct {
	Start       string      `xml:"start,attr"`
	Stop        string      `xml:"stop,attr"`
	Channel     string      `xml:"channel,attr"`
	Title       string      `xml:"title"`
	Description string      `xml:"desc,omitempty"`
	Episode     string      `xml:"sub-title,omitempty"`
	Number      *xmlEpisode `xml:"episode-num,omitempty"`
	Rating      string      `xml:"rating>value,omitempty"`
	Icon        *xmlIcon    `xml:"icon,omitempty"`
}
type xmlGuide struct {
	XMLName    xml.Name       `xml:"tv"`
	Channels   []xmlChannel   `xml:"channel"`
	Programmes []xmlProgramme `xml:"programme"`
}

func (i *instance) guide(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	channels, err := i.host.LiveTV.Channels(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	sum := sha256.Sum256([]byte(credential))
	device := frontend.Device{ID: "m3u-" + hex.EncodeToString(sum[:16]), Client: "M3U", Product: "M3U"}
	if i.host.ClientSeen != nil {
		i.host.ClientSeen(device.ID)
	}
	now := time.Now().UTC()
	channelIDs := make([]string, 0, len(channels))
	for _, channel := range channels {
		if channel.ID != "" {
			channelIDs = append(channelIDs, channel.ID)
		}
	}
	programmes, err := i.collectGuide(r.Context(), viewer, frontend.LiveGuideQuery{Device: device, Channels: channelIDs, From: now.Add(-6 * time.Hour), Until: now.Add(48 * time.Hour), Limit: 10000})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	result := xmlGuide{}
	allowed := make(map[string]bool, len(channels))
	for _, channel := range channels {
		if channel.ID != "" {
			allowed[channel.ID] = true
			item := xmlChannel{ID: channel.ID, Name: channel.Name}
			if src := i.imageURL(r, credential, device, 0, channel.ID, channel.Images); src != "" {
				item.Icon = &xmlIcon{Src: src}
			}
			result.Channels = append(result.Channels, item)
		}
	}
	imageCache := make(map[frontend.RatingKey]string)
	for _, p := range programmes {
		if !allowed[p.ChannelID] || !p.End.After(p.Start) {
			continue
		}
		item := xmlProgramme{Start: p.Start.UTC().Format("20060102150405 -0700"), Stop: p.End.UTC().Format("20060102150405 -0700"), Channel: p.ChannelID, Title: p.Title, Description: p.Summary, Episode: p.EpisodeTitle, Rating: p.Rating}
		if p.Item != 0 {
			src, ok := imageCache[p.Item]
			if !ok {
				src = i.imageURL(r, credential, device, p.Item, "", p.Images)
				imageCache[p.Item] = src
			}
			if src != "" {
				item.Icon = &xmlIcon{Src: src}
			}
		}
		if p.Season > 0 && p.Episode > 0 {
			item.Number = &xmlEpisode{System: "xmltv_ns", Value: fmt.Sprintf("%d.%d.", p.Season-1, p.Episode-1)}
		}
		result.Programmes = append(result.Programmes, item)
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(result)
}

// collectGuide divides capped responses until each subquery is complete. A
// programme crossing a time split is returned only once.
func (i *instance) collectGuide(ctx context.Context, viewer *frontend.Viewer, query frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
	if len(query.Channels) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	var result []frontend.LiveProgramme
	var collect func(frontend.LiveGuideQuery) error
	collect = func(q frontend.LiveGuideQuery) error {
		page, err := i.host.LiveTV.Guide(ctx, viewer, q)
		if err != nil {
			return err
		}
		if len(page) >= q.Limit {
			left, right := q, q
			if len(q.Channels) > 1 {
				middle := len(q.Channels) / 2
				left.Channels, right.Channels = q.Channels[:middle], q.Channels[middle:]
			} else {
				if q.Until.Sub(q.From) <= time.Second {
					return errors.New("guide result exceeds capacity")
				}
				middle := q.From.Add(q.Until.Sub(q.From) / 2)
				left.Until, right.From = middle, middle
			}
			if err := collect(left); err != nil {
				return err
			}
			return collect(right)
		}
		for _, p := range page {
			key := fmt.Sprintf("%q:%q:%d:%d:%q", p.ChannelID, p.ID, p.Start.UnixNano(), p.End.UnixNano(), p.Title)
			if !seen[key] {
				seen[key] = true
				result = append(result, p)
			}
		}
		return nil
	}
	if err := collect(query); err != nil {
		return nil, err
	}
	return result, nil
}

func (i *instance) image(w http.ResponseWriter, r *http.Request) {
	if i.host.ImageGrants == nil {
		http.NotFound(w, r)
		return
	}
	i.host.ImageGrants.Serve(w, r, r.PathValue("secret"))
}

func (i *instance) imageURL(r *http.Request, credential string, device frontend.Device, item frontend.RatingKey, channel string, images []frontend.Image) string {
	if i.host.ImageGrants == nil || i.host.SurfaceURL == nil || len(images) == 0 {
		return ""
	}
	base, err := i.host.SurfaceURL(r.Context(), r)
	if err != nil {
		return ""
	}
	secret, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: credential, Device: device, Item: item, Channel: channel, Image: images[0]})
	if err != nil {
		return ""
	}
	return strings.TrimRight(base, "/") + "/image/" + secret
}
