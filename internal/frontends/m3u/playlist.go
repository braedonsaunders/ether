package m3u

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// playlistEntry contains only already-authorized channel metadata and URLs.
// In particular, StreamURL must come from the host's scoped stream service.
type playlistEntry struct {
	ID, Name, LogoURL, StreamURL string
}

func playlistText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return ' '
		}
		return r
	}, value)
}

func playlistURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && !strings.ContainsAny(value, "\r\n\x00")
}

func writePlaylist(w io.Writer, entries []playlistEntry) error {
	for _, entry := range entries {
		if !playlistURL(entry.StreamURL) || (entry.LogoURL != "" && !playlistURL(entry.LogoURL)) {
			return fmt.Errorf("m3u: invalid scoped URL")
		}
	}
	if _, err := io.WriteString(w, "#EXTM3U\n"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(w, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\"", playlistText(entry.ID), playlistText(entry.Name)); err != nil {
			return err
		}
		if entry.LogoURL != "" {
			if _, err := fmt.Fprintf(w, " tvg-logo=\"%s\"", playlistText(entry.LogoURL)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, ",%s\n%s\n", playlistText(entry.Name), entry.StreamURL); err != nil {
			return err
		}
	}
	return nil
}
