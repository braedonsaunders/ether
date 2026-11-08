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
	"strconv"
	"strings"
	"time"
)

var xtreamVODFile = regexp.MustCompile(`^[0-9]+\.[A-Za-z0-9]+$`)

var m3uAttr = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)

func stableID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:10])
}
func validHTTP(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// m3uStats records what ParseM3U left out, for the import log.
type m3uStats struct {
	SkippedVOD int
}

// isVODStream reports whether an Xtream URL belongs to a VOD family: a path
// segment "movie" or "series" followed by exactly three more segments
// (username/password/id). The query is ignored. /live/ and /timeshift/ are
// never VOD, and non-Xtream layouts are left alone.
func isVODStream(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, segment := range segments {
		switch strings.ToLower(segment) {
		case "movie", "series":
			if len(segments)-i-1 == 3 && segments[i+1] != "" && segments[i+2] != "" && xtreamVODFile.MatchString(segments[i+3]) {
				return true
			}
		}
	}
	return false
}

// ParseM3U preserves only explicitly provided HTTP(S) streams. Executable,
// file and media-tool pseudo protocols are rejected.
func ParseM3U(r io.Reader, sourceID string) ([]Channel, error) {
	channels, _, err := parseM3U(r, sourceID)
	return channels, err
}

// parseM3U is ParseM3U with the counts an import logs. VOD entries are
// skipped by URL family; live and timeshift streams are always kept.
func parseM3U(r io.Reader, sourceID string) ([]Channel, m3uStats, error) {
	var stats m3uStats
	limited := &hardLimit{r: r, max: maxPlaylistBytes, kind: "playlist"}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 4096), 1<<20)
	var out []Channel
	var pending *Channel
	var pendingHeaders map[string]string
	header := false
	seen := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" {
			continue
		}
		if !header {
			if !strings.HasPrefix(line, "#EXTM3U") {
				return nil, stats, fmt.Errorf("playlist must start with #EXTM3U")
			}
			header = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			pendingHeaders = nil
			attrs := map[string]string{}
			for _, m := range m3uAttr.FindAllStringSubmatch(line, -1) {
				attrs[m[1]] = m[2]
				playlistMediaHeader(&pendingHeaders, m[1], m[2])
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
			pending = &Channel{Name: name, TvgName: attrs["tvg-name"], Number: attrs["tvg-chno"], Logo: attrs["tvg-logo"], GuideID: attrs["tvg-id"], Group: attrs["group-title"], SourceID: sourceID, Kind: "iptv", Enabled: true}
			continue
		}
		if pending != nil && strings.HasPrefix(line, "#EXTVLCOPT:") {
			if key, value, ok := strings.Cut(strings.TrimPrefix(line, "#EXTVLCOPT:"), "="); ok {
				playlistMediaHeader(&pendingHeaders, key, value)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if pending == nil {
			return nil, stats, fmt.Errorf("stream has no #EXTINF entry")
		}
		if !validHTTP(line) {
			return nil, stats, fmt.Errorf("channel %q needs an HTTP(S) stream", pending.Name)
		}
		if isVODStream(line) {
			stats.SkippedVOD++
			pending = nil
			continue
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
		pending.StreamSources = []StreamSource{{ID: stableID(pending.ID, "playlist"), Name: "Imported stream", URL: line, Enabled: true, Headers: pendingHeaders}}
		if pending.Name == "" {
			pending.Name = pending.GuideID
		}
		if pending.Name == "" {
			pending.Name = "Channel"
		}
		if pending.Number == "" {
			pending.numberMissing = true
			pending.Number = fmt.Sprint(len(out) + 1)
		} else {
			pending.ProviderNumber = pending.Number
		}
		out = append(out, *pending)
		pending = nil
	}
	if err := sc.Err(); err != nil {
		if lim := limitCause(err); lim != nil {
			return nil, stats, lim
		}
		return nil, stats, err
	}
	if err := limited.overflow(); err != nil {
		return nil, stats, err
	}
	if pending != nil {
		return nil, stats, fmt.Errorf("playlist ends before a stream URL")
	}
	if !header {
		return nil, stats, fmt.Errorf("empty playlist")
	}
	return out, stats, nil
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
type xmlIcon struct {
	Src string `xml:"src,attr"`
}
type xmlEpisodeNum struct {
	System string `xml:"system,attr"`
	Value  string `xml:",chardata"`
}
type xmlProgramme struct {
	Channel    string          `xml:"channel,attr"`
	Start      string          `xml:"start,attr"`
	Stop       string          `xml:"stop,attr"`
	Title      []xmlText       `xml:"title"`
	Desc       []xmlText       `xml:"desc"`
	SubTitle   []xmlText       `xml:"sub-title,omitempty"`
	Category   []xmlText       `xml:"category,omitempty"`
	Date       string          `xml:"date,omitempty"`
	EpisodeNum []xmlEpisodeNum `xml:"episode-num,omitempty"`
	New        *struct{}       `xml:"new,omitempty"`
	Premiere   *struct{}       `xml:"premiere,omitempty"`
	Live       *struct{}       `xml:"live,omitempty"`
	Rating     []struct {
		Value string `xml:"value"`
	} `xml:"rating,omitempty"`
	Icon []xmlIcon `xml:"icon,omitempty"`
}
type XMLGuide struct {
	Channels   map[string]Channel
	Programmes []Programme
	// Names holds every display-name of each XMLTV channel, in order.
	Names map[string][]string
	// LateChannels counts <channel> elements that appeared after the first
	// <programme>. A valid guide has none; they are skipped rather than
	// buffered, so name candidates they declare are missed.
	LateChannels int
}

func xmlTime(s string) (time.Time, error) {
	for _, layout := range []string{"20060102150405 -0700", "200601021504 -0700", "20060102150405-0700", "200601021504-0700", "20060102150405", "200601021504"} {
		if t, e := time.Parse(layout, strings.TrimSpace(s)); e == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid XMLTV timestamp %q", s)
}

func xmlDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"20060102150405 -0700",
		"20060102150405",
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02T15:04:05",
		"20060102",
		"2006-01-02",
		"2006/01/02",
		"2006",
	} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid XMLTV date %q", s)
}

var reEpisodeSxEx = regexp.MustCompile(`(?i)\bs(?:eason)?\s*(\d{1,4})[\s.]*(?:e|ep|episode)\.?\s*(\d{1,4})\b|\b(\d{1,4})x(\d{1,4})\b|\b(?:e|ep|episode)\.?\s*(\d{1,4})\b|\bs(?:eason)?\s*(\d{1,4})\b`)

func parseXMLEpisode(nums []xmlEpisodeNum) (int, int, bool) {
	// First pass: xmltv_ns is the canonical standard for XMLTV episode numbers.
	for _, num := range nums {
		if strings.EqualFold(strings.TrimSpace(num.System), "xmltv_ns") {
			val := strings.TrimSpace(num.Value)
			parts := strings.Split(val, ".")
			if len(parts) >= 2 {
				s, e := 0, 0
				hasSeason, hasEpisode := false, false
				sPart := strings.TrimSpace(parts[0])
				if idx := strings.IndexByte(sPart, '/'); idx >= 0 {
					sPart = sPart[:idx]
				}
				if n, err := strconv.Atoi(strings.TrimSpace(sPart)); err == nil && n >= 0 {
					s = n + 1
					hasSeason = true
				}
				ePart := strings.TrimSpace(parts[1])
				if idx := strings.IndexByte(ePart, '/'); idx >= 0 {
					ePart = ePart[:idx]
				}
				if n, err := strconv.Atoi(strings.TrimSpace(ePart)); err == nil && n >= 0 {
					e = n + 1
					hasEpisode = true
				}
				if hasSeason || hasEpisode {
					return s, e, true
				}
			}
		}
	}

	// Second pass: parse text/onscreen patterns, skipping known identifier systems.
	for _, num := range nums {
		sys := strings.ToLower(strings.TrimSpace(num.System))
		if sys == "dd_progid" || sys == "tms" || sys == "zap2it" || sys == "original-air-date" || sys == "thetvdb.com" || sys == "imdb.com" {
			continue
		}
		val := strings.TrimSpace(num.Value)
		if val == "" {
			continue
		}
		if m := reEpisodeSxEx.FindStringSubmatch(val); len(m) > 0 {
			if m[1] != "" && m[2] != "" {
				s, _ := strconv.Atoi(m[1])
				e, _ := strconv.Atoi(m[2])
				if s > 0 || e > 0 {
					return s, e, true
				}
			} else if m[3] != "" && m[4] != "" {
				s, _ := strconv.Atoi(m[3])
				e, _ := strconv.Atoi(m[4])
				if s > 0 || e > 0 {
					return s, e, true
				}
			} else if m[5] != "" {
				e, _ := strconv.Atoi(m[5])
				if e > 0 {
					return 0, e, true
				}
			} else if len(m) > 6 && m[6] != "" {
				s, _ := strconv.Atoi(m[6])
				if s > 0 {
					return s, 0, true
				}
			}
		}
	}
	return 0, 0, false
}

func ParseXMLTV(r io.Reader) (XMLGuide, error) {
	g, _, err := parseXMLTV(r, guideFilter{})
	return g, err
}

// guideFilter keeps programmes that overlap [From, Until) and, when IDs is
// non-nil, only those channel ids (case-insensitive). A nil IDs map keeps
// every channel, which is a guide-only source. A zero From means no time cut.
//
// Resolve, when set, runs once after the leading <channel> elements (at the
// first <programme>, or EOF) and may add ids for channels matched by name.
// XMLTV's content model puts every <channel> before the first <programme>, so
// candidates are found without buffering programmes or reading twice.
type guideFilter struct {
	From, Until time.Time
	IDs         map[string]struct{}
	Resolve     func(channels map[string]Channel, names map[string][]string) map[string]struct{}
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
	// allow starts as the caller's set. Resolve replaces it with a copy plus
	// its candidates; the caller's map is never mutated.
	allow := f.IDs
	allows := func(id string) bool {
		if allow == nil {
			return true
		}
		_, ok := allow[strings.ToLower(strings.TrimSpace(id))]
		return ok
	}
	resolved := false
	sawProgramme := false
	// Leading <channel> elements are kept whole while name candidates can
	// still be resolved. A guide-only source (IDs nil) already keeps them.
	collecting := func() bool { return !resolved && !sawProgramme && f.Resolve != nil && f.IDs != nil }
	resolve := func() {
		if resolved {
			return
		}
		resolved = true
		if f.Resolve == nil || f.IDs == nil {
			return
		}
		extra := f.Resolve(g.Channels, g.Names)
		merged := make(map[string]struct{}, len(f.IDs)+len(extra))
		for id := range f.IDs {
			merged[id] = struct{}{}
		}
		for id := range extra {
			if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
				merged[id] = struct{}{}
			}
		}
		allow = merged
		for id := range g.Channels {
			if _, ok := merged[strings.ToLower(strings.TrimSpace(id))]; ok {
				continue
			}
			delete(g.Channels, id)
			delete(g.Names, id)
			dropped = true
		}
	}
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
			if sawProgramme {
				// Invalid per the DTD: only count it. Name matching has
				// already run, and buffering would not help.
				g.LateChannels++
			}
			if !collecting() && !allows(id) {
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
			if !sawProgramme {
				sawProgramme = true
				resolve()
			}
			channel := strings.TrimSpace(xmlAttr(el, "channel"))
			if channel == "" || !allows(channel) {
				if channel != "" && !allows(channel) {
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
			item := Item{}
			if len(p.Icon) > 0 && p.Icon[0].Src != "" {
				item.Thumb = p.Icon[0].Src
				item.Art = p.Icon[0].Src
				if len(p.Icon) > 1 && p.Icon[1].Src != "" {
					item.Art = p.Icon[1].Src
				}
			}
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
			if len(p.SubTitle) > 0 && p.SubTitle[0].Value != "" {
				item.EpisodeTitle = p.SubTitle[0].Value
				item.ShowTitle = item.Title
				item.MediaType = "episode"
			}
			for _, cat := range p.Category {
				if v := strings.TrimSpace(cat.Value); v != "" {
					item.Genres = append(item.Genres, v)
				}
			}
			if p.Date != "" {
				if t, err := xmlDate(p.Date); err == nil {
					item.ReleasedMS = t.UnixMilli()
				}
			}
			if s, ep, ok := parseXMLEpisode(p.EpisodeNum); ok {
				item.Season = s
				item.Episode = ep
				item.MediaType = "episode"
				if item.ShowTitle == "" {
					item.ShowTitle = item.Title
				}
			}
			if p.New != nil || p.Premiere != nil {
				item.Premiere = true
			}
			if p.Live != nil {
				item.Live = true
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
	// A guide with no programmes still prunes its channels at EOF.
	resolve()
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

// dedupeProgrammes keeps the last entry for each channel and start time, the
// same result one-row ON CONFLICT DO UPDATE produced. Provider guides repeat
// entries, and a multi-row ON CONFLICT cannot touch one row twice.
func dedupeProgrammes(ps []Programme) []Programme {
	seen := make(map[string]int, len(ps))
	out := ps[:0]
	for _, p := range ps {
		key := p.ChannelID + "\x00" + strconv.FormatInt(p.Start.UnixMilli(), 10)
		if i, ok := seen[key]; ok {
			out[i] = p
			continue
		}
		seen[key] = len(out)
		out = append(out, p)
	}
	return out
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
