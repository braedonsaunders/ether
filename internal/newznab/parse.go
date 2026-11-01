package newznab

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

func releaseID(indexer, guid, fallback string) string {
	key := guid
	if key == "" {
		key = fallback
	}
	h := sha1.Sum([]byte(indexer + "|" + key))
	return hex.EncodeToString(h[:])
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339,
		"Mon, 02 Jan 2006 15:04:05 -0700", "2006-01-02 15:04:05",
		// Single-digit days ("Mon, 2 Jan 2006"), which RFC1123 layouts reject.
		"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

type rawItem struct {
	title, guid, link, pubDate string
	size                       int64
	grabs                      int
	password                   bool
	category                   int
	usenetDate                 string
	// Torznab
	infoHash, magnet, encType string
	seeders, peers            int
	ids                       core.IDs
}

func (r rawItem) toRelease(indexer string) core.Release {
	name := strings.TrimSpace(r.title)
	nzbURL := r.link
	pub := parseDate(r.pubDate)
	if pub.IsZero() {
		pub = parseDate(r.usenetDate)
	}
	rel := core.Release{
		GUID: r.guid, Indexer: indexer, Name: name, NZBURL: nzbURL,
		Size: r.size, PubDate: pub, Grabs: r.grabs, Password: r.password, Category: r.category,
		TitleIDs: r.ids, MediaType: categoryMedia(r.category),
	}
	rel.ID = releaseID(indexer, r.guid, nzbURL+name)
	if name != "" {
		rel.Info = releaseparse.Parse(name)
	}
	rel.Seeders, rel.Peers = r.seeders, r.peers
	magnet := r.magnet
	if magnet == "" && strings.HasPrefix(strings.ToLower(r.link), "magnet:") {
		magnet = r.link
	}
	if strings.HasPrefix(strings.ToLower(rel.NZBURL), "magnet:") {
		rel.NZBURL = "" // a magnet is not a downloadable document
	}
	rel.MagnetURL = magnet
	rel.InfoHash = NormalizeInfoHash(r.infoHash)
	if rel.InfoHash == "" && magnet != "" {
		rel.InfoHash = InfoHashFromMagnet(magnet)
	}
	if rel.InfoHash != "" || magnet != "" || strings.Contains(strings.ToLower(r.encType), "bittorrent") {
		rel.Protocol = core.ProtocolTorrent
	}
	return rel
}

// parseFeed parses an RSS/XML or JSON search response, translating
// Newznab error documents into typed errors.
func parseFeed(body []byte, indexer string) ([]core.Release, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("newznab: empty response")
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return parseJSONFeed(trimmed, indexer)
	}
	return parseXMLFeed(trimmed, indexer)
}

func parseXMLFeed(body []byte, indexer string) ([]core.Release, error) {
	return parseXMLStream(bytes.NewReader(body), indexer, nil)
}

func parseXMLStream(reader io.Reader, indexer string, arrived func([]core.Release)) ([]core.Release, error) {
	dec := xml.NewDecoder(reader)
	// Indexer feeds are often not well-formed XML: a bare "&" in a link or
	// description, or an HTML entity ("&nbsp;"). One bad character must not
	// fail the whole search.
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	var out []core.Release
	var cur *rawItem
	var field string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("newznab: parse response: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "error":
				code, desc := 0, ""
				for _, a := range t.Attr {
					if a.Name.Local == "code" {
						code, _ = strconv.Atoi(a.Value)
					}
					if a.Name.Local == "description" {
						desc = a.Value
					}
				}
				return nil, documentError(code, desc)
			case "item":
				cur = &rawItem{}
			case "enclosure":
				if cur != nil {
					for _, a := range t.Attr {
						if a.Name.Local == "url" && cur.link == "" {
							cur.link = a.Value
						}
						if a.Name.Local == "length" {
							if n, err := strconv.ParseInt(a.Value, 10, 64); err == nil && cur.size == 0 {
								cur.size = n
							}
						}
						if a.Name.Local == "type" {
							cur.encType = a.Value
						}
					}
				}
			case "attr":
				if cur != nil {
					var name, val string
					for _, a := range t.Attr {
						if a.Name.Local == "name" {
							name = strings.ToLower(a.Value)
						}
						if a.Name.Local == "value" {
							val = a.Value
						}
					}
					applyAttr(cur, name, val)
				}
			default:
				if cur != nil {
					field = t.Name.Local
					// guid may carry text; enclosure-style attrs ignored.
				}
			}
		case xml.CharData:
			if cur != nil && field != "" {
				text := string(t)
				switch field {
				case "title":
					cur.title += text
				case "guid":
					cur.guid += text
				case "link":
					cur.link += text
				case "pubDate":
					cur.pubDate += text
				case "size":
					if n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
						cur.size = n
					}
				case "category":
					if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && cur.category == 0 {
						cur.category = n
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "item" && cur != nil {
				out = append(out, cur.toRelease(indexer))
				if arrived != nil {
					arrived([]core.Release{out[len(out)-1]})
				}
				cur = nil
			}
			field = ""
		}
	}

	return out, nil
}

func applyAttr(r *rawItem, name, val string) {
	switch name {
	case "imdb", "imdbid":
		v := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(val)), "tt")
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && len(v) <= 10 {
			r.ids.IMDB = fmt.Sprintf("tt%07d", n)
		}
	case "tmdb", "tmdbid":
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && n > 0 {
			r.ids.TMDB = n
		}
	case "tvdb", "tvdbid":
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil && n > 0 {
			r.ids.TVDB = n
		}
	case "size":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			r.size = n
		}
	case "grabs", "grab":
		if n, err := strconv.Atoi(val); err == nil {
			r.grabs = n
		}
	case "password", "haspassword":
		r.password = val == "1" || strings.EqualFold(val, "true") || strings.EqualFold(val, "yes")
	case "category":
		// May be "2040" or "Movies > HD"; keep first numeric token.
		for _, f := range strings.FieldsFunc(val, func(ru rune) bool { return ru == '>' || ru == ',' || ru == ' ' }) {
			if n, err := strconv.Atoi(f); err == nil {
				r.category = n
				break
			}
		}
	case "usenetdate", "usenet_date":
		r.usenetDate = val
	case "guid":
		if r.guid == "" {
			r.guid = val
		}
	// Torznab attributes.
	case "infohash":
		r.infoHash = val
	case "magneturl":
		r.magnet = val
	case "seeders":
		if n, err := strconv.Atoi(val); err == nil {
			r.seeders = n
		}
	case "peers":
		if n, err := strconv.Atoi(val); err == nil {
			r.peers = n
		}
	}
}

// --- JSON ---

func parseJSONFeed(body []byte, indexer string) ([]core.Release, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("newznab: parse json response: %w", err)
	}
	if m, ok := root.(map[string]any); ok {
		if e, ok := m["error"]; ok {
			if em, ok := e.(map[string]any); ok {
				code := 0
				if cv, ok := em["@attributes"].(map[string]any); ok {
					code, _ = strconv.Atoi(asString(cv["code"]))
					return nil, documentError(code, asString(cv["description"]))
				}
				code, _ = strconv.Atoi(asString(em["code"]))
				return nil, documentError(code, asString(em["description"]))
			}
		}
	}
	items := findItems(root)
	out := make([]core.Release, 0, len(items))
	for _, it := range items {
		r := rawItem{
			title:   asString(it["title"]),
			guid:    asString(it["guid"]),
			link:    asString(it["link"]),
			pubDate: asString(it["pubDate"]),
		}
		if g, ok := it["guid"].(map[string]any); ok {
			r.guid = asString(g["#text"])
		}
		if n, err := strconv.ParseInt(asString(it["size"]), 10, 64); err == nil {
			r.size = n
		}
		if enc, ok := it["enclosure"].(map[string]any); ok {
			attrs, _ := enc["@attributes"].(map[string]any)
			if attrs != nil {
				r.encType = asString(attrs["type"])
				if u := asString(attrs["url"]); u != "" && r.link == "" {
					r.link = u
				}
				if n, err := strconv.ParseInt(asString(attrs["length"]), 10, 64); err == nil && r.size == 0 {
					r.size = n
				}
			}
		}
		for _, a := range attrList(it) {
			applyAttr(&r, strings.ToLower(asString(a["name"])), asString(a["value"]))
		}
		out = append(out, r.toRelease(indexer))
	}
	return out, nil
}

func attrList(item map[string]any) []map[string]any {
	var out []map[string]any
	collect := func(v any) {
		switch vv := v.(type) {
		case []any:
			for _, e := range vv {
				if m, ok := e.(map[string]any); ok {
					if at, ok := m["@attributes"].(map[string]any); ok {
						out = append(out, at)
					} else {
						out = append(out, m)
					}
				}
			}
		case map[string]any:
			if at, ok := vv["@attributes"].(map[string]any); ok {
				out = append(out, at)
			} else {
				out = append(out, vv)
			}
		}
	}
	for _, key := range []string{"attr", "attributes"} {
		if v, ok := item[key]; ok {
			collect(v)
		}
	}
	// Nested under a namespaced key like "newznab:attr".
	for k, v := range item {
		if strings.HasSuffix(k, ":attr") {
			collect(v)
		}
	}
	return out
}

func findItems(v any) []map[string]any {
	switch vv := v.(type) {
	case []any:
		var out []map[string]any
		for _, e := range vv {
			if m, ok := e.(map[string]any); ok {
				if _, hasTitle := m["title"]; hasTitle {
					out = append(out, m)
				} else {
					out = append(out, findItems(e)...)
				}
			}
		}
		return out
	case map[string]any:
		if raw, ok := vv["item"]; ok {
			// XML-to-JSON converters emit a lone result as an object,
			// not a one-element array.
			if m, ok := raw.(map[string]any); ok {
				return []map[string]any{m}
			}
			return findItems(raw)
		}
		for _, e := range vv {
			if got := findItems(e); len(got) > 0 {
				return got
			}
		}
	}
	return nil
}

func asString(v any) string {
	switch vv := v.(type) {
	case string:
		return vv
	case float64:
		return strconv.FormatInt(int64(vv), 10)
	case nil:
		return ""
	default:
		return fmt.Sprint(vv)
	}
}
