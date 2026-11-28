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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

var xtreamVODFile = regexp.MustCompile(`^[0-9]+\.[A-Za-z0-9]+$`)

var m3uAttr = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)

var m3uHeaderAttr = regexp.MustCompile(`([a-zA-Z0-9_-]+)=(?:"([^"]*)"|'([^']*)')`)

const maxPlaylistGuideURLs = 8

func stableID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:10])
}
func validHTTP(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// m3uStats records what ParseM3U left out and any #EXTM3U guide URLs.
type m3uStats struct {
	SkippedVOD     int
	SkippedInvalid int
	GuideURLs      []string
}

// parseM3UGuideURLs extracts HTTP(S) guide URLs from an #EXTM3U header's
// x-tvg-url and url-tvg attributes (each possibly comma-separated).
func parseM3UGuideURLs(headerLine string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range m3uHeaderAttr.FindAllStringSubmatch(headerLine, -1) {
		key := m[1]
		if !strings.EqualFold(key, "x-tvg-url") && !strings.EqualFold(key, "url-tvg") {
			continue
		}
		val := m[2]
		if val == "" {
			val = m[3]
		}
		for _, part := range strings.Split(val, ",") {
			u := strings.TrimSpace(part)
			if u == "" || strings.ContainsAny(u, "\r\n\x00") || !validHTTP(u) || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
			if len(out) >= maxPlaylistGuideURLs {
				return out
			}
		}
	}
	return out
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

// m3uArchiveAttrs takes the first alias present for each setting, so a
// channel can explicitly override a playlist default with an empty value.
func m3uArchiveAttrs(attrs map[string]string, defaults ChannelArchive) ChannelArchive {
	for _, key := range []string{"catchup", "catchup-type"} {
		if value, ok := attrs[key]; ok {
			defaults.Mode = strings.ToLower(strings.TrimSpace(value))
			break
		}
	}
	for _, key := range []string{"catchup-days", "tvg-rec", "timeshift"} {
		if value, ok := attrs[key]; ok {
			days, _ := strconv.Atoi(strings.TrimSpace(value))
			defaults.Days = min(days, 30)
			break
		}
	}
	if value, ok := attrs["catchup-source"]; ok {
		defaults.Source = strings.TrimSpace(value)
	}
	return defaults
}

func m3uChannelArchive(a ChannelArchive, stream string) *ChannelArchive {
	if a.Days <= 0 {
		return nil
	}
	switch a.Mode {
	case "xc", "xtream":
		if base, _, _, _ := xtreamLiveParts(stream); base != "" {
			a.Mode, a.Source = "xtream", ""
		} else if a.Mode == "xtream" || a.Source == "" {
			return nil
		}
	case "default", "append":
		if a.Source == "" {
			return nil
		}
	case "shift", "flussonic":
		a.Source = ""
	default:
		return nil
	}
	return &a
}

// ParseM3U preserves only explicitly provided HTTP(S) streams. Executable,
// file and media-tool pseudo protocols are skipped.
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
	var archiveDefaults, pendingArchive ChannelArchive
	header := false
	seen := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" {
			continue
		}
		if !header {
			if !strings.HasPrefix(line, "#EXTM3U") {
				continue
			}
			stats.GuideURLs = parseM3UGuideURLs(line)
			attrs := map[string]string{}
			for _, m := range m3uHeaderAttr.FindAllStringSubmatch(line, -1) {
				attrs[strings.ToLower(m[1])] = m[2] + m[3]
			}
			archiveDefaults = m3uArchiveAttrs(attrs, ChannelArchive{})
			header = true
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if pending != nil {
				stats.SkippedInvalid++
			}
			pendingHeaders = nil
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
			metadata := line
			if comma >= 0 {
				metadata = line[:comma]
			}
			attrs := map[string]string{}
			for _, m := range m3uAttr.FindAllStringSubmatch(metadata, -1) {
				attrs[strings.ToLower(m[1])] = m[2]
				playlistMediaHeader(&pendingHeaders, m[1], m[2])
			}
			name := attrs["tvg-name"]
			if comma >= 0 && strings.TrimSpace(line[comma+1:]) != "" {
				name = strings.TrimSpace(line[comma+1:])
			}
			pendingArchive = m3uArchiveAttrs(attrs, archiveDefaults)
			pending = &Channel{Name: name, TvgName: attrs["tvg-name"], Number: attrs["tvg-chno"], Logo: attrs["tvg-logo"], GuideID: attrs["tvg-id"], ProviderGuideID: attrs["tvg-id"], Group: attrs["group-title"], SourceID: sourceID, Kind: "iptv", Enabled: true}
			continue
		}
		if group, ok := strings.CutPrefix(line, "#EXTGRP:"); ok {
			if pending != nil && pending.Group == "" {
				pending.Group = strings.TrimSpace(group)
			}
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
		if pending == nil || !validHTTP(line) {
			stats.SkippedInvalid++
			pending = nil
			continue
		}
		if isVODStream(line) {
			stats.SkippedVOD++
			pending = nil
			continue
		}
		pending.StreamURL = line
		pending.Archive = m3uChannelArchive(pendingArchive, line)
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
		stats.SkippedInvalid++
	}
	if !header {
		return nil, stats, fmt.Errorf("playlist needs an #EXTM3U header")
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
	Rating     []struct {
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
	// LateChannels counts <channel> elements that appeared after the first
	// <programme>. A valid guide has none; they are skipped rather than
	// buffered, so name candidates they declare are missed.
	LateChannels      int
	SkippedProgrammes int
}

// mergeXMLGuide folds src into dst when a playlist advertises several XMLTV
// guide URLs.
func mergeXMLGuide(dst *XMLGuide, src XMLGuide) {
	if dst.Channels == nil {
		dst.Channels = map[string]Channel{}
	}
	if dst.Names == nil {
		dst.Names = map[string][]string{}
	}
	for id, ch := range src.Channels {
		if cur, ok := dst.Channels[id]; ok {
			if cur.Logo == "" && ch.Logo != "" {
				cur.Logo = ch.Logo
			}
			if (cur.Name == "" || cur.Name == id) && ch.Name != "" {
				cur.Name = ch.Name
			}
			dst.Channels[id] = cur
		} else {
			dst.Channels[id] = ch
		}
	}
	for id, names := range src.Names {
		existing := dst.Names[id]
		for _, n := range names {
			if !slices.Contains(existing, n) {
				existing = append(existing, n)
			}
		}
		dst.Names[id] = existing
	}
	dst.Programmes = append(dst.Programmes, src.Programmes...)
	dst.LateChannels += src.LateChannels
	dst.SkippedProgrammes += src.SkippedProgrammes
}

func xmlTime(s string) (time.Time, error) {
	raw := strings.TrimSpace(s)
	for _, layout := range []string{
		"20060102150405 -0700", "200601021504 -0700", "20060102150405-0700", "200601021504-0700",
		"20060102150405", "200601021504", time.RFC3339,
		"20060102150405 -07:00", "200601021504 -07:00", "20060102150405-07:00", "200601021504-07:00",
	} {
		if t, e := time.Parse(layout, raw); e == nil {
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
//
// Resolve, when set, runs once after the leading <channel> elements (at the
// first <programme>, or EOF) and may add ids for channels matched by name.
// XMLTV's content model puts every <channel> before the first <programme>, so
// candidates are found without buffering programmes or reading twice.
type guideFilter struct {
	From, Until time.Time
	ArchiveFrom map[string]time.Time
	IDs         map[string]struct{}
	Resolve     func(channels map[string]Channel, names map[string][]string) map[string]struct{}
}

func (f guideFilter) timed() bool { return !f.From.IsZero() }

func (f guideFilter) from(channel string) time.Time {
	if len(f.ArchiveFrom) > 0 {
		if at, ok := f.ArchiveFrom[strings.ToLower(strings.TrimSpace(channel))]; ok {
			return at
		}
	}
	return f.From
}

// parseXMLTV streams an XMLTV document. dropped is true when the filter
// discarded a channel or programme, so the caller can store the trimmed
// guide instead of the original text.
func parseXMLTV(r io.Reader, f guideFilter) (XMLGuide, bool, error) {
	g := XMLGuide{Channels: map[string]Channel{}, Names: map[string][]string{}}
	limited := &hardLimit{r: r, max: maxGuideBytes, kind: "guide"}
	d := xml.NewDecoder(limited)
	root := false
	dropped := false
	// Only the earliest valid start beyond the window can close an in-window programme.
	firstAfter := make(map[string]time.Time)
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
			rawStart := xmlAttr(el, "start")
			start, startErr := xmlTime(rawStart)
			var end time.Time
			var endErr error
			stop := strings.TrimSpace(xmlAttr(el, "stop"))
			if stop != "" {
				end, endErr = xmlTime(stop)
			}
			badTime := startErr != nil || endErr != nil || (stop != "" && !end.After(start))
			outside := false
			if badTime {
				g.SkippedProgrammes++
			} else if f.timed() {
				from := f.from(channel)
				outside = !start.Before(f.Until) || (!end.IsZero() && !end.After(from)) || (end.IsZero() && start.Before(from.Add(-24*time.Hour)))
				if !start.Before(f.Until) {
					id := xmlAttr(el, "channel")
					if first, ok := firstAfter[id]; !ok || start.Before(first) {
						firstAfter[id] = start
					}
				}
			}
			if badTime || outside {
				dropped = true
				if err := d.Skip(); err != nil {
					if lim := limitCause(err); lim != nil {
						return g, dropped, lim
					}
					return g, dropped, err
				}
				continue
			}
			var p xmlProgramme
			if err := d.DecodeElement(&p, &el); err != nil {
				if lim := limitCause(err); lim != nil {
					return g, dropped, lim
				}
				return g, dropped, err
			}
			item := Item{Thumb: strings.TrimSpace(p.Icon.Src)}
			date := strings.TrimSpace(p.Date)
			if len(date) >= 4 {
				if year, err := strconv.Atoi(date[:4]); err == nil && year >= 1800 && year <= 2199 {
					item.Year = year
				}
			}
			if len(p.SubTitle) > 0 {
				item.EpisodeTitle = p.SubTitle[0].Value
			}
			for _, category := range p.Category {
				genre := strings.TrimSpace(category.Value)
				if genre != "" && !slices.Contains(item.Genres, genre) {
					item.Genres = append(item.Genres, genre)
				}
			}
			if frontend.LiveCategory(item.Genres, "movie", "") {
				item.MediaType = "movie"
			} else if frontend.LiveCategory(item.Genres, "series", "") {
				item.MediaType = "episode"
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
			g.Programmes = append(g.Programmes, Programme{ID: stableID(p.Channel, rawStart), ChannelID: p.Channel, Start: start, End: end, Item: item})
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
	g.Programmes = inferMissingStops(g.Programmes, firstAfter)
	if f.timed() {
		kept := g.Programmes[:0]
		for _, p := range g.Programmes {
			if p.Start.Before(f.Until) && p.End.After(f.from(p.ChannelID)) {
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
		if p.Item.EpisodeTitle != "" {
			b.WriteString("<sub-title>")
			if err := xmlEscape(&b, p.Item.EpisodeTitle); err != nil {
				return "", err
			}
			b.WriteString("</sub-title>")
		}
		if p.Item.Summary != "" {
			b.WriteString("<desc>")
			if err := xmlEscape(&b, p.Item.Summary); err != nil {
				return "", err
			}
			b.WriteString("</desc>")
		}
		if p.Item.Year > 0 {
			fmt.Fprintf(&b, "<date>%d</date>", p.Item.Year)
		}
		for _, genre := range p.Item.Genres {
			b.WriteString("<category>")
			if err := xmlEscape(&b, genre); err != nil {
				return "", err
			}
			b.WriteString("</category>")
		}
		if p.Item.Thumb != "" {
			b.WriteString(`<icon src="`)
			if err := xmlEscape(&b, p.Item.Thumb); err != nil {
				return "", err
			}
			b.WriteString(`"/>`)
		}
		if p.Item.Rating != "" {
			b.WriteString("<rating><value>")
			if err := xmlEscape(&b, p.Item.Rating); err != nil {
				return "", err
			}
			b.WriteString("</value></rating>")
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
func inferMissingStops(ps []Programme, firstAfter map[string]time.Time) []Programme {
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
	for id, ts := range starts {
		if first, ok := firstAfter[id]; ok {
			starts[id] = append(ts, first)
		}
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
