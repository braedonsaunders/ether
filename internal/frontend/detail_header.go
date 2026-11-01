package frontend

import "strings"

// ApplyDetailHeader places a viewer's micro-reason the way Plex detail does.
// An empty tagline takes the header, which is the line under the title.
// Otherwise the header prefixes the overview and the catalogue tagline stays.
// The header is never written into both places. An empty header changes nothing.
func ApplyDetailHeader(tagline, overview, header string) (string, string) {
	header = strings.TrimSpace(header)
	if header == "" {
		return tagline, overview
	}
	if strings.TrimSpace(tagline) == "" {
		return header, overview
	}
	overview = strings.TrimSpace(overview)
	if overview == "" {
		return tagline, header
	}
	return tagline, header + "\n\n" + overview
}
