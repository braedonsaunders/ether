package plex

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"

	"github.com/braedonsaunders/ether/internal/conform"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type Client = conform.Client
type Request = conform.Request
type Response = conform.Response
type DiscoveredIDs = conform.DiscoveredIDs

// Clients returns representative Plex Web, iOS and Apple TV request
// profiles, with the X-Plex-Platform values the real apps send (PMS picks
// its client profile from them and answers 400 to a decision request from
// an unknown platform such as "iPhone").
func Clients() map[string]Client {
	return map[string]Client{
		"web":     {Name: "Plex Web", Headers: headers("Plex Web", "Chrome", "129.0", "Windows", "Chrome", "4.160.0")},
		"ios":     {Name: "Plex for iOS", Headers: headers("Plex for iOS", "iOS", "18.0", "iPhone", "iPhone", "8.40")},
		"appletv": {Name: "Plex for Apple TV", Headers: headers("Plex for Apple TV", "tvOS", "18.0", "Apple TV", "Apple TV", "8.40")},
	}
}
func headers(product, platform, platformVersion, device, deviceName, version string) http.Header {
	h := http.Header{}
	h.Set("X-Plex-Product", product)
	h.Set("X-Plex-Version", version)
	h.Set("X-Plex-Platform", platform)
	h.Set("X-Plex-Platform-Version", platformVersion)
	h.Set("X-Plex-Device", device)
	h.Set("X-Plex-Device-Name", deviceName)
	h.Set("X-Plex-Client-Identifier", "pmsdiff-client")
	h.Set("X-Plex-Provides", "player,controller")
	h.Set("X-Plex-Token", "{{token}}")
	return h
}

// Corpus returns probes used when a Plex application connects and browses a server.
func Corpus() []Request {
	paths := []string{"/", "/identity", "/library/sections", "/library/sections/{{section}}/all?X-Plex-Container-Start=0&X-Plex-Container-Size=50", "/library/sections/{{movieSection}}/prefs", "/library/sections/{{showSection}}/prefs", "/library/metadata/{{movieKey}}?includeExtras=1&includeRelated=1&includeOnDeck=1&includeChapters=1&includeGuids=1", "/library/metadata/{{showKey}}/children", "/library/metadata/{{seasonKey}}/children", "/library/metadata/{{episodeKey}}", "/hubs", "/hubs/promoted", "/hubs/sections/{{section}}", "/hubs/search?query=", "/library/onDeck", "/hubs/continueWatching", "/media/providers", "/library/collections/{{collectionKey}}", "/playlists/{{playlistKey}}", "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=1&directStream=1&videoQuality=8&X-Plex-Client-Profile-Extra=", "/status/sessions", "/myplex/account", "/:/prefs",
		// Current apps run a client-side media decision engine (hasMDE=1):
		// with direct play allowed PMS only reports mdeDecisionCode; Plex
		// Web asks with directPlay=0 for a stream.
		"/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=1&directStream=1&videoQuality=8",
		"/video/:/transcode/universal/decision?hasMDE=1&path=%2Flibrary%2Fmetadata%2F{{movieKey}}&mediaIndex=0&partIndex=0&protocol=hls&fastSeek=1&directPlay=0&directStream=1&subtitleSize=100&audioBoost=100&location=lan",
		// Searches with results (the query matches the discovered movie).
		"/hubs/search?query=burbs&limit=10&includeCollections=1",
		"/library/search?query=burbs&limit=10&searchTypes=movies%2Ctv&includeMetadata=1"}
	clients := Clients()
	out := make([]Request, 0, len(paths))
	for _, p := range paths {
		hs := map[string]http.Header{}
		for k, c := range clients {
			h := c.Headers.Clone()
			h.Set("Accept", "application/xml")
			hs[k] = h
			j := h.Clone()
			j.Set("Accept", "application/json")
			hs[k+"-json"] = j
		}
		out = append(out, Request{Name: p, Method: http.MethodGet, Path: p, Headers: hs})
	}
	return out
}

type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:"-"`
	Children []xmlNode  `xml:"-"`
}

func (n *xmlNode) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	n.XMLName = start.Name
	n.Attrs = start.Attr
	for {
		t, e := d.Token()
		if e != nil {
			return e
		}
		switch x := t.(type) {
		case xml.StartElement:
			var c xmlNode
			if e := d.DecodeElement(&c, &x); e != nil {
				return e
			}
			n.Children = append(n.Children, c)
		case xml.EndElement:
			if x.Name == start.Name {
				return nil
			}
		}
	}
}
func (n xmlNode) attr(k string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == k {
			return a.Value
		}
	}
	return ""
}
func (n xmlNode) descendants(name string) []xmlNode {
	var out []xmlNode
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
		out = append(out, c.descendants(name)...)
	}
	return out
}
func (n xmlNode) direct(names ...string) []xmlNode {
	var out []xmlNode
	for _, c := range n.Children {
		for _, name := range names {
			if c.XMLName.Local == name {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
func (n xmlNode) first(names ...string) xmlNode {
	for _, name := range names {
		for _, c := range n.Children {
			if c.XMLName.Local == name {
				return c
			}
		}
	}
	return xmlNode{}
}
func parsePlexXML(b []byte) (xmlNode, error) { var n xmlNode; e := xml.Unmarshal(b, &n); return n, e }

// Discover reads sections and representative items on one server. If align is non-nil,
// matching item GUIDs from that server are preferred; absent matches use the first item.
func Discover(ctx context.Context, client *http.Client, base, token, profile string, align *DiscoveredIDs) DiscoveredIDs {
	out := DiscoveredIDs{Values: map[string]string{}}
	get := func(path string) (xmlNode, error) {
		req := Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{}}
		c := Clients()[profile]
		h := c.Headers.Clone()
		h.Set("Accept", "application/xml")
		req.Headers[profile] = h
		r, e := Fetch(ctx, client, base, token, req, profile, nil, false)
		if e != nil {
			return xmlNode{}, e
		}
		if r.Status < 200 || r.Status >= 300 {
			return xmlNode{}, fmt.Errorf("HTTP %d", r.Status)
		}
		return parsePlexXML(r.Body)
	}
	root, e := get("/library/sections")
	if e != nil {
		out.Warnings = append(out.Warnings, "sections discovery failed: "+e.Error())
		return out
	}
	sections := root.descendants("Directory")
	sectionByType := map[string]string{}
	for _, s := range sections {
		typ := strings.ToLower(s.attr("type"))
		if typ == "movie" || typ == "show" {
			if sectionByType[typ] == "" {
				sectionByType[typ] = s.attr("key")
			}
		}
	}
	for _, typ := range []string{"movie", "show"} {
		sid := sectionByType[typ]
		if sid == "" {
			out.Warnings = append(out.Warnings, "no "+typ+" section; using fallback section ID 1")
			continue
		}
		out.Values[typ+"Section"] = sid
		listing, err := get("/library/sections/" + sid + "/all?X-Plex-Container-Start=0&X-Plex-Container-Size=50")
		if err != nil {
			out.Warnings = append(out.Warnings, typ+" listing failed: "+err.Error())
			continue
		}
		items := listing.direct("Video", "Metadata")
		if len(items) == 0 {
			items = listing.direct("Directory")
		}
		if len(items) == 0 {
			out.Warnings = append(out.Warnings, "no "+typ+" item; using fallback ratingKey 1")
			continue
		}
		chosen := selectByGuid(items, alignedGuid(align, typ))
		key := nodeKey(chosen)
		if key == "" {
			key = "1"
			out.Warnings = append(out.Warnings, typ+" item has no key; using fallback ratingKey 1")
		}
		out.Values[typ+"Key"] = key
		guid := nodeGuid(chosen)
		if detail, er := get("/library/metadata/" + key + "?includeExtras=1&includeRelated=1&includeOnDeck=1&includeChapters=1&includeGuids=1"); er == nil {
			detailItems := append(detail.descendants("Metadata"), detail.descendants("Video")...)
			for _, item := range detailItems {
				if g := nodeGuid(item); strings.HasPrefix(g, "tmdb://") || strings.HasPrefix(g, "imdb://") {
					guid = g
					break
				}
			}
		}
		out.Values[typ+"Guid"] = guid
		if typ == "show" {
			seasons, err := get("/library/metadata/" + key + "/children")
			if err != nil {
				out.Warnings = append(out.Warnings, "season discovery failed: "+err.Error())
				continue
			}
			ss := seasons.direct("Metadata")
			if len(ss) == 0 {
				ss = seasons.direct("Directory")
			}
			filtered := ss[:0]
			for _, season := range ss {
				if !strings.Contains(strings.ToLower(season.attr("key")), "allleaves") && !strings.EqualFold(season.attr("title"), "All episodes") {
					filtered = append(filtered, season)
				}
			}
			ss = filtered
			if len(ss) > 0 {
				season := ss[0]
				sk := nodeKey(season)
				if sk != "" {
					out.Values["seasonKey"] = sk
					eps, er := get("/library/metadata/" + sk + "/children")
					if er == nil {
						es := eps.direct("Video", "Metadata")
						if len(es) > 0 {
							out.Values["episodeKey"] = nodeKey(es[0])
						}
					} else {
						out.Warnings = append(out.Warnings, "episode discovery failed: "+er.Error())
					}
				}
			}
		}
	}
	for _, entry := range []struct{ key, path, tag string }{{"collectionKey", "/library/collections", "Directory"}, {"playlistKey", "/playlists", "Playlist"}} {
		n, er := get(entry.path)
		if er != nil && entry.key == "collectionKey" && out.Values["movieSection"] != "" {
			n, er = get("/library/sections/" + out.Values["movieSection"] + "/all?type=18&X-Plex-Container-Start=0&X-Plex-Container-Size=50")
		}
		if er != nil && entry.key == "playlistKey" {
			n, er = get("/playlists/all")
		}
		if er != nil {
			out.Warnings = append(out.Warnings, entry.key+" discovery failed: "+er.Error())
			continue
		}
		xs := n.descendants(entry.tag)
		if len(xs) > 0 {
			v := nodeKey(xs[0])
			if v != "" {
				out.Values[entry.key] = v
			}
		}
	}
	if out.Values["section"] == "" {
		if out.Values["movieSection"] != "" {
			out.Values["section"] = out.Values["movieSection"]
		} else if out.Values["showSection"] != "" {
			out.Values["section"] = out.Values["showSection"]
		}
	}
	for k, v := range map[string]string{"section": "1", "movieKey": "1", "showKey": "1", "seasonKey": "1", "episodeKey": "1", "collectionKey": "1", "playlistKey": "1"} {
		if out.Values[k] == "" {
			out.Values[k] = v
			out.Warnings = append(out.Warnings, "using fallback "+k+" ID "+v)
		}
	}
	return out
}
func nodeKey(n xmlNode) string {
	for _, k := range []string{"ratingKey", "id", "key"} {
		if v := n.attr(k); v != "" {
			if k == "key" && strings.HasPrefix(v, "/") {
				parts := strings.Split(strings.Trim(v, "/"), "/")
				for i := len(parts) - 1; i >= 0; i-- {
					if _, err := strconv.ParseInt(parts[i], 10, 64); err == nil {
						return parts[i]
					}
				}
			}
			return v
		}
	}
	return ""
}
func nodeGuid(n xmlNode) string {
	if v := n.attr("guid"); strings.HasPrefix(v, "tmdb://") || strings.HasPrefix(v, "imdb://") {
		return v
	}
	var fallback string
	if v := n.attr("guid"); v != "" {
		fallback = v
	}
	for _, g := range append([]xmlNode{n.first("Guid")}, n.descendants("Guid")...) {
		v := g.attr("id")
		if strings.HasPrefix(v, "tmdb://") || strings.HasPrefix(v, "imdb://") {
			return v
		}
		if fallback == "" {
			fallback = v
		}
	}
	return fallback
}
func alignedGuid(d *DiscoveredIDs, typ string) string {
	if d == nil {
		return ""
	}
	g := d.Values[typ+"Guid"]
	if strings.HasPrefix(g, "tmdb://") || strings.HasPrefix(g, "imdb://") {
		return g
	}
	return ""
}
func selectByGuid(xs []xmlNode, guid string) xmlNode {
	if guid != "" {
		for _, x := range xs {
			if nodeGuid(x) == guid {
				return x
			}
		}
	}
	return xs[0]
}

// Fetch issues a probe, substituting discovered IDs and token, and captures its response.
func Fetch(ctx context.Context, client *http.Client, base, token string, req Request, profile string, vars map[string]string, jsonMode bool) (Response, error) {
	path := req.Path
	for k, v := range vars {
		path = strings.ReplaceAll(path, "{{"+k+"}}", v)
	}
	if strings.Contains(path, "{{") {
		path = strings.ReplaceAll(path, "{{section}}", "1")
		path = strings.ReplaceAll(path, "{{ratingKey}}", "1")
	}
	h := req.Headers[profile]
	if jsonMode {
		h = req.Headers[profile+"-json"]
	}
	u := strings.TrimRight(base, "/") + path
	q, e := http.NewRequestWithContext(ctx, req.Method, u, bytes.NewReader(req.Body))
	if e != nil {
		return Response{}, e
	}
	for k, v := range h {
		for _, x := range v {
			q.Header.Add(k, strings.ReplaceAll(x, "{{token}}", token))
		}
	}
	res, e := client.Do(q)
	if e != nil {
		return Response{}, e
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if e != nil {
		return Response{}, e
	}
	return Response{Status: res.StatusCode, Headers: res.Header.Clone(), Body: b}, nil
}
