package livetv

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

var m3uAttr = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)

func stableID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:10])
}
func validHTTP(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ParseM3U preserves only explicitly provided HTTP(S) streams. Executable,
// file and media-tool pseudo protocols are rejected.
func ParseM3U(r io.Reader, sourceID string) ([]Channel, error) {
	limited := &hardLimit{r: r, max: maxPlaylistBytes, kind: "playlist"}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 4096), 1<<20)
	var out []Channel
	var pending *Channel
	header := false
	seen := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" {
			continue
		}
		if !header {
			if !strings.HasPrefix(line, "#EXTM3U") {
				return nil, fmt.Errorf("playlist must start with #EXTM3U")
			}
			header = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			attrs := map[string]string{}
			for _, m := range m3uAttr.FindAllStringSubmatch(line, -1) {
				attrs[m[1]] = m[2]
			}
			// Commas inside quoted attributes are not the display-name separator.
			quote := false
			comma := -1
			for i, ch := range line {
				if ch == '"' {
					quote = !quote
				}
				if ch == ',' && !quote {
					comma = i
					break
				}
			}
			name := attrs["tvg-name"]
			if comma >= 0 && strings.TrimSpace(line[comma+1:]) != "" {
				name = strings.TrimSpace(line[comma+1:])
			}
			pending = &Channel{Name: name, Number: attrs["tvg-chno"], Logo: attrs["tvg-logo"], GuideID: attrs["tvg-id"], Group: attrs["group-title"], SourceID: sourceID, Kind: "iptv", Enabled: true}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if pending == nil {
			return nil, fmt.Errorf("stream has no #EXTINF entry")
		}
		if !validHTTP(line) {
			return nil, fmt.Errorf("channel %q needs an HTTP(S) stream", pending.Name)
		}
		pending.StreamURL = line
		identity := pending.GuideID
		if identity == "" {
			identity = line
		}
		pending.ID = stableID(sourceID, identity)
		if seen[pending.ID] {
			pending.ID = stableID(sourceID, identity, pending.Name, pending.Number, pending.Group)
			if seen[pending.ID] {
				pending.ID = stableID(sourceID, identity, line)
			}
			if seen[pending.ID] {
				pending = nil
				continue
			}
		}
		seen[pending.ID] = true
		pending.StreamSources = []StreamSource{{ID: stableID(pending.ID, "playlist"), Name: "Imported stream", URL: line, Enabled: true}}
		if pending.Name == "" {
			pending.Name = pending.GuideID
		}
		if pending.Name == "" {
			pending.Name = "Channel"
		}
		if pending.Number == "" {
			pending.Number = fmt.Sprint(len(out) + 1)
		}
		out = append(out, *pending)
		pending = nil
	}
	if err := sc.Err(); err != nil {
		if lim := limitCause(err); lim != nil {
			return nil, lim
		}
		return nil, err
	}
	if err := limited.overflow(); err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, fmt.Errorf("playlist ends before a stream URL")
	}
	if !header {
		return nil, fmt.Errorf("empty playlist")
	}
	return out, nil
}

type xmlText struct {
	Value string `xml:",chardata"`
}
type xmlChannel struct {
	ID   string    `xml:"id,attr"`
	Name []xmlText `xml:"display-name"`
	Icon struct {
		Src string `xml:"src,attr"`
	} `xml:"icon"`
}
type xmlProgramme struct {
	Channel string    `xml:"channel,attr"`
	Start   string    `xml:"start,attr"`
	Stop    string    `xml:"stop,attr"`
	Title   []xmlText `xml:"title"`
	Desc    []xmlText `xml:"desc"`
	Rating  []struct {
		Value string `xml:"value"`
	} `xml:"rating"`
	Icon struct {
		Src string `xml:"src,attr"`
	} `xml:"icon"`
}
type XMLGuide struct {
	Channels   map[string]Channel
	Programmes []Programme
	// Names holds every display-name of each XMLTV channel, in order.
	Names map[string][]string
}

func xmlTime(s string) (time.Time, error) {
	for _, layout := range []string{"20060102150405 -0700", "200601021504 -0700", "20060102150405-0700", "200601021504-0700", "20060102150405", "200601021504"} {
		if t, e := time.Parse(layout, strings.TrimSpace(s)); e == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid XMLTV timestamp %q", s)
}
func ParseXMLTV(r io.Reader) (XMLGuide, error) {
	g, _, err := parseXMLTV(r, guideFilter{})
	return g, err
}

// guideFilter keeps programmes that overlap [From, Until) and, when IDs is
// non-nil, only those channel ids (case-insensitive). A nil IDs map keeps
// every channel, which is a guide-only source. A zero From means no time cut.
type guideFilter struct {
	From, Until time.Time
	IDs         map[string]struct{}
}

func (f guideFilter) allows(id string) bool {
	if f.IDs == nil {
		return true
	}
	_, ok := f.IDs[strings.ToLower(strings.TrimSpace(id))]
	return ok
}

func (f guideFilter) timed() bool { return !f.From.IsZero() }

// parseXMLTV streams an XMLTV document. dropped is true when the filter
// discarded a channel or programme, so the caller can store the trimmed
// guide instead of the original text.
func parseXMLTV(r io.Reader, f guideFilter) (XMLGuide, bool, error) {
	g := XMLGuide{Channels: map[string]Channel{}, Names: map[string][]string{}}
	limited := &hardLimit{r: r, max: maxGuideBytes, kind: "guide"}
	d := xml.NewDecoder(limited)
	root := false
	dropped := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if lim := limitCause(err); lim != nil {
				return g, dropped, lim
			}
			return g, dropped, err
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !root {
			if el.Name.Local != "tv" {
				return g, dropped, fmt.Errorf("XMLTV root must be tv")
			}
			root = true
			continue
		}
		switch el.Name.Local {
		case "channel":
			id := xmlAttr(el, "id")
			if !f.allows(id) {
				dropped = true
				if err := d.Skip(); err != nil {
					if lim := limitCause(err); lim != nil {
						return g, dropped, lim
					}
					return g, dropped, err
				}
				continue
			}
			var c xmlChannel
			if err := d.DecodeElement(&c, &el); err != nil {
				if lim := limitCause(err); lim != nil {
					return g, dropped, lim
				}
				return g, dropped, err
			}
			name := c.ID
			if len(c.Name) > 0 {
				name = c.Name[0].Value
			}
			g.Channels[c.ID] = Channel{GuideID: c.ID, Name: name, Logo: c.Icon.Src}
			for _, n := range c.Name {
				g.Names[c.ID] = append(g.Names[c.ID], strings.TrimSpace(n.Value))
			}
		case "programme":
			channel := strings.TrimSpace(xmlAttr(el, "channel"))
			if channel == "" || !f.allows(channel) {
				if channel != "" && !f.allows(channel) {
					dropped = true
				}
				if err := d.Skip(); err != nil {
					if lim := limitCause(err); lim != nil {
						return g, dropped, lim
					}
					return g, dropped, err
				}
				continue
			}
			if f.timed() {
				start, err := xmlTime(xmlAttr(el, "start"))
				if err != nil {
					return g, dropped, err
				}
				stop := strings.TrimSpace(xmlAttr(el, "stop"))
				var end time.Time
				if stop != "" {
					if end, err = xmlTime(stop); err != nil {
						return g, dropped, err
					}
				}
				if !start.Before(f.Until) || (!end.IsZero() && !end.After(f.From)) || (end.IsZero() && start.Before(f.From.Add(-24*time.Hour))) {
					dropped = true
					if err := d.Skip(); err != nil {
						if lim := limitCause(err); lim != nil {
							return g, dropped, lim
						}
						return g, dropped, err
					}
					continue
				}
			}
			var p xmlProgramme
			if err := d.DecodeElement(&p, &el); err != nil {
				if lim := limitCause(err); lim != nil {
					return g, dropped, lim
				}
				return g, dropped, err
			}
			if strings.TrimSpace(p.Channel) == "" {
				continue
			}
			start, err := xmlTime(p.Start)
			if err != nil {
				return g, dropped, err
			}
			var end time.Time
			if strings.TrimSpace(p.Stop) != "" {
				if end, err = xmlTime(p.Stop); err != nil {
					return g, dropped, err
				}
				if !end.After(start) {
					continue
				}
			}
			item := Item{Thumb: p.Icon.Src}
			if !end.IsZero() {
				item.DurationMS = end.Sub(start).Milliseconds()
			}
			if len(p.Title) > 0 {
				item.Title = p.Title[0].Value
			}
			if len(p.Desc) > 0 {
				item.Summary = p.Desc[0].Value
			}
			if len(p.Rating) > 0 {
				item.Rating = p.Rating[0].Value
			}
			g.Programmes = append(g.Programmes, Programme{ID: stableID(p.Channel, p.Start), ChannelID: p.Channel, Start: start, End: end, Item: item})
		}
	}
	if err := limited.overflow(); err != nil {
		return g, dropped, err
	}
	if !root {
		return g, dropped, fmt.Errorf("empty XMLTV guide")
	}
	g.Programmes = inferMissingStops(g.Programmes)
	if f.timed() {
		kept := g.Programmes[:0]
		for _, p := range g.Programmes {
			if p.Start.Before(f.Until) && p.End.After(f.From) {
				kept = append(kept, p)
				continue
			}
			dropped = true
		}
		g.Programmes = kept
	}
	return g, dropped, nil
}

func xmlAttr(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// formatXMLTV writes the guide that was actually kept, so a stored upload
// is the trimmed document rather than the provider's full dump.
func formatXMLTV(g XMLGuide) (string, error) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<tv>")
	ids := make([]string, 0, len(g.Channels))
	for id := range g.Channels {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := g.Channels[id]
		b.WriteString(`<channel id="`)
		if err := xmlEscape(&b, id); err != nil {
			return "", err
		}
		b.WriteString(`">`)
		names := g.Names[id]
		if len(names) == 0 && c.Name != "" {
			names = []string{c.Name}
		}
		for _, n := range names {
			b.WriteString("<display-name>")
			if err := xmlEscape(&b, n); err != nil {
				return "", err
			}
			b.WriteString("</display-name>")
		}
		if c.Logo != "" {
			b.WriteString(`<icon src="`)
			if err := xmlEscape(&b, c.Logo); err != nil {
				return "", err
			}
			b.WriteString(`"/>`)
		}
		b.WriteString("</channel>")
	}
	for _, p := range g.Programmes {
		b.WriteString(`<programme channel="`)
		if err := xmlEscape(&b, p.ChannelID); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, `" start="%s" stop="%s"><title>`, p.Start.UTC().Format("20060102150405 -0700"), p.End.UTC().Format("20060102150405 -0700"))
		if err := xmlEscape(&b, p.Item.Title); err != nil {
			return "", err
		}
		b.WriteString("</title>")
		if p.Item.Summary != "" {
			b.WriteString("<desc>")
			if err := xmlEscape(&b, p.Item.Summary); err != nil {
				return "", err
			}
			b.WriteString("</desc>")
		}
		if p.Item.Rating != "" {
			b.WriteString("<rating><value>")
			if err := xmlEscape(&b, p.Item.Rating); err != nil {
				return "", err
			}
			b.WriteString("</value></rating>")
		}
		if p.Item.Thumb != "" {
			b.WriteString(`<icon src="`)
			if err := xmlEscape(&b, p.Item.Thumb); err != nil {
				return "", err
			}
			b.WriteString(`"/>`)
		}
		b.WriteString("</programme>")
	}
	b.WriteString("</tv>")
	return b.String(), nil
}

func xmlEscape(b *strings.Builder, s string) error {
	return xml.EscapeText(b, []byte(s))
}

// inferMissingStops ends each programme without a stop time at the start
// of the next later programme on its channel; one with no successor has no
// knowable end and is dropped.
func inferMissingStops(ps []Programme) []Programme {
	open := false
	for _, p := range ps {
		if p.End.IsZero() {
			open = true
			break
		}
	}
	if !open {
		return ps
	}
	starts := map[string][]time.Time{}
	for _, p := range ps {
		starts[p.ChannelID] = append(starts[p.ChannelID], p.Start)
	}
	for _, ts := range starts {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	}
	out := ps[:0]
	for _, p := range ps {
		if p.End.IsZero() {
			ts := starts[p.ChannelID]
			i := sort.Search(len(ts), func(i int) bool { return ts[i].After(p.Start) })
			if i == len(ts) {
				continue
			}
			p.End = ts[i]
			p.Item.DurationMS = p.End.Sub(p.Start).Milliseconds()
		}
		out = append(out, p)
	}
	return out
}
