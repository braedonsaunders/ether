package plex

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"github.com/braedonsaunders/ether/internal/frontend"
	"io"
	"strconv"
	"strings"
)

func supportDocumentPath(path string) bool {
	p := strings.ToLower(path)
	if strings.Contains(p, "/transcode/universal/decision") || strings.HasSuffix(p, "/start.mpd") {
		return true
	}
	for _, segment := range []string{"/thumb", "/art", "/banner", "/theme", "/logo", "/image", "/posters", "/backgrounds"} {
		if strings.Contains(p, segment) {
			return false
		}
	}
	if path == liveProviderPath+"/grid" || strings.HasPrefix(path, liveProviderPath+"/metadata/") || path == liveProviderPath+"/lineups/dvr/channels" {
		return true
	}
	for _, prefix := range []string{"/hubs", "/library/sections", "/library/metadata", "/library/ondeck", "/playlists", "/search", "/media/providers"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

type journalItem struct {
	Title string `json:"title" xml:"title,attr"`
}
type journalHub struct {
	Identifier string        `json:"hubIdentifier" xml:"hubIdentifier,attr"`
	Title      string        `json:"title" xml:"title,attr"`
	Style      string        `json:"style" xml:"style,attr"`
	Type       string        `json:"type" xml:"type,attr"`
	Size       int           `json:"size" xml:"size,attr"`
	More       flexibleBool  `json:"more" xml:"more,attr"`
	Metadata   []journalItem `json:"Metadata" xml:"Metadata"`
	Directory  []journalItem `json:"Directory" xml:"Directory"`
	Videos     []journalItem `json:"-" xml:"Video"`
	Tracks     []journalItem `json:"-" xml:"Track"`
}
type journalContainer struct {
	Size                int           `json:"size" xml:"size,attr"`
	TotalSize           *int          `json:"totalSize" xml:"totalSize,attr"`
	Hubs                []journalHub  `json:"Hub" xml:"Hub"`
	Metadata            []journalItem `json:"Metadata" xml:"Metadata"`
	Directory           []journalItem `json:"Directory" xml:"Directory"`
	Videos              []journalItem `json:"-" xml:"Video"`
	Tracks              []journalItem `json:"-" xml:"Track"`
	GeneralDecisionCode *int          `json:"generalDecisionCode" xml:"generalDecisionCode,attr"`
	GeneralDecisionText string        `json:"generalDecisionText" xml:"generalDecisionText,attr"`
}
type flexibleBool bool

func (b *flexibleBool) UnmarshalJSON(p []byte) error {
	*b = string(p) == "true" || string(p) == "1"
	return nil
}
func (b *flexibleBool) UnmarshalXMLAttr(a xml.Attr) error {
	*b = a.Value == "1" || a.Value == "true"
	return nil
}
func journalTitles(a, b []journalItem) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, x := range append(a, b...) {
		if x.Title != "" {
			out = append(out, x.Title)
		}
	}
	return out
}
func summarizePlexResponse(raw []byte, truncated bool, encoding string) *frontend.DiagnosticResponseSummary {
	if truncated {
		return &frontend.DiagnosticResponseSummary{Truncated: true}
	}
	if strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return &frontend.DiagnosticResponseSummary{Truncated: true}
		}
		defer gz.Close()
		decoded, err := io.ReadAll(io.LimitReader(gz, (1<<20)+1))
		if err != nil || len(decoded) > 1<<20 {
			return &frontend.DiagnosticResponseSummary{Truncated: true}
		}
		raw = decoded
	}
	if out := summarizeDASHResponse(raw); out != nil {
		return out
	}
	var c journalContainer
	var doc struct {
		MediaContainer journalContainer `json:"MediaContainer"`
	}
	if json.Unmarshal(raw, &doc) == nil {
		c = doc.MediaContainer
	} else if xml.Unmarshal(raw, &c) != nil {
		return nil
	}
	c.Metadata = append(c.Metadata, c.Videos...)
	c.Metadata = append(c.Metadata, c.Tracks...)
	out := &frontend.DiagnosticResponseSummary{Size: c.Size, TotalSize: c.TotalSize, ItemCount: len(c.Metadata) + len(c.Directory), Items: journalTitles(c.Metadata, c.Directory)}
	if c.GeneralDecisionCode != nil {
		code := *c.GeneralDecisionCode
		switch {
		case code >= 2000:
			out.Decision = "failed"
			out.ErrorCode = strconv.Itoa(code)
			out.Error = c.GeneralDecisionText
		case code == 1000:
			out.Decision = "direct play permitted"
		case code == 1001:
			out.Decision = "conversion permitted"
		}
	}
	for _, h := range c.Hubs {
		h.Metadata = append(h.Metadata, h.Videos...)
		h.Metadata = append(h.Metadata, h.Tracks...)
		out.Hubs = append(out.Hubs, frontend.DiagnosticHubSummary{Identifier: h.Identifier, Title: h.Title, Style: h.Style, Type: h.Type, Size: h.Size, More: bool(h.More), ItemCount: len(h.Metadata) + len(h.Directory), Items: journalTitles(h.Metadata, h.Directory)})
	}
	return out
}
