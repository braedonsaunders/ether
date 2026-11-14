package plex

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

var dashDiagnosticValue = regexp.MustCompile(`^[a-zA-Z0-9_.:,;/-]{1,160}$`)

// summarizeDASHResponse projects only protocol attributes. URLs are described
// structurally: neither session identities, credentials, hosts nor arbitrary
// path/query text enter the support journal. Parsing runs only during capture.
func summarizeDASHResponse(raw []byte) *frontend.DiagnosticResponseSummary {
	d := xml.NewDecoder(bytes.NewReader(raw))
	out := &frontend.DiagnosticResponseSummary{}
	root := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !root {
			if el.Name.Local != "MPD" {
				return nil
			}
			root = true
		}
		for _, a := range el.Attr {
			value := a.Value
			switch el.Name.Local + "." + a.Name.Local {
			case "MPD.profiles", "MPD.type", "MPD.minBufferTime", "MPD.mediaPresentationDuration", "MPD.maxSegmentDuration", "Period.start", "Period.duration", "Representation.id", "Representation.codecs", "Representation.mimeType", "Representation.bandwidth", "Representation.width", "Representation.height", "Representation.audioSamplingRate", "AdaptationSet.contentType", "AdaptationSet.segmentAlignment", "SegmentTemplate.timescale", "SegmentTemplate.duration", "SegmentTemplate.startNumber", "AudioChannelConfiguration.schemeIdUri", "AudioChannelConfiguration.value":
				if !dashDiagnosticValue.MatchString(value) {
					continue
				}
			case "MPD.availabilityStartTime":
				value = "present"
			case "SegmentTemplate.initialization", "SegmentTemplate.media":
				value = dashDiagnosticURL(value)
			default:
				continue
			}
			if len(out.Manifest) >= 64 {
				out.Truncated = true
				return out
			}
			out.Manifest = append(out.Manifest, el.Name.Local+"."+a.Name.Local+"="+value)
		}
	}
	if !root {
		return nil
	}
	// XML parsing already verifies escaping. Record whether token separators on
	// the wire used XML entities, without retaining any credential values.
	out.Manifest = append(out.Manifest, fmt.Sprintf("xml.escapedAmpersand=%t", bytes.Contains(raw, []byte("&amp;"))))
	return out
}

func dashDiagnosticURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	kind := "relative"
	if u.IsAbs() || u.Host != "" {
		kind = "absolute"
	}
	if strings.HasPrefix(u.Path, "/") && kind == "relative" {
		kind = "root-relative"
	}
	return fmt.Sprintf("%s;representation=%t;number=%t;time=%t;header=%t;streamGrant=%t;readGrant=%t", kind, strings.Contains(u.Path, "$RepresentationID$"), strings.Contains(u.Path, "$Number$"), strings.Contains(u.Path, "$Time$"), strings.HasSuffix(u.Path, "/header"), u.Query().Has(streamGrantParam), u.Query().Has(readCapParam))
}
