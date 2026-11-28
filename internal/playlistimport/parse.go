// Package playlistimport reads playlist files (M3U/M3U8 and CSV) into track
// descriptions a library's kind can identify.
package playlistimport

import (
	"encoding/csv"
	"errors"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// MaxTracks bounds one import.
const MaxTracks = 500

// ErrUnreadable means the file is not a playlist this package reads.
var ErrUnreadable = errors.New("playlistimport: unreadable playlist")

// Parse reads a playlist file of the given format ("m3u" or "csv") into its
// name (when the file names one) and tracks, at most MaxTracks.
func Parse(format, text string) (string, []core.ImportTrack, error) {
	text = strings.TrimPrefix(text, "\uFEFF")
	switch strings.ToLower(format) {
	case "m3u", "m3u8":
		return parseM3U(text)
	case "csv":
		return parseCSV(text)
	}
	return "", nil, ErrUnreadable
}

func parseM3U(text string) (string, []core.ImportTrack, error) {
	var (
		name   string
		out    []core.ImportTrack
		extinf *core.ImportTrack
	)
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || line == "#EXTM3U":
		case strings.HasPrefix(line, "#PLAYLIST:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "#PLAYLIST:"))
		case strings.HasPrefix(line, "#EXTINF:"):
			info := strings.TrimPrefix(line, "#EXTINF:")
			secs, rest := splitExtinf(info)
			if i := strings.IndexByte(secs, ' '); i >= 0 {
				secs = secs[:i] // attributes after the duration
			}
			t := splitArtistTitle(rest)
			if d, err := strconv.ParseFloat(strings.TrimSpace(secs), 64); err == nil && d > 0 {
				t.DurationSec = d
			}
			extinf = &t
		case strings.HasPrefix(line, "#"):
		default:
			t := core.ImportTrack{}
			if extinf != nil {
				t = *extinf
			} else {
				base := path.Base(strings.ReplaceAll(line, "\\", "/"))
				t = splitArtistTitle(strings.TrimSuffix(base, path.Ext(base)))
			}
			extinf = nil
			if t.Title != "" {
				out = append(out, t)
			}
		}
		if len(out) >= MaxTracks {
			break
		}
	}
	if len(out) == 0 {
		return "", nil, ErrUnreadable
	}
	return name, out, nil
}

// splitExtinf finds the display-name comma outside quoted attributes.
func splitExtinf(info string) (string, string) {
	var quote byte
	for i := range len(info) {
		c := info[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"':
			quote = c
		case '\'':
			if i > 0 && info[i-1] == '=' {
				quote = c
			}
		case ',':
			return info[:i], info[i+1:]
		}
	}
	return info, ""
}

// splitArtistTitle reads "Artist - Title" (a leading track number dropped).
func splitArtistTitle(s string) core.ImportTrack {
	s = strings.TrimSpace(trackNumber.ReplaceAllString(strings.TrimSpace(s), ""))
	if artist, title, ok := strings.Cut(s, " - "); ok && strings.TrimSpace(artist) != "" {
		return core.ImportTrack{Artist: strings.TrimSpace(artist), Title: strings.TrimSpace(title)}
	}
	return core.ImportTrack{Title: s}
}

// trackNumber is a leading "03 - " or "03. " on a file name.
var trackNumber = regexp.MustCompile(`^\d{1,3}(?:\s*-\s+|\.\s*)`)

func parseCSV(text string) (string, []core.ImportTrack, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil || len(rows) < 2 {
		return "", nil, ErrUnreadable
	}
	col := map[string]int{}
	durationMS, artistList := false, false
	for i, h := range rows[0] {
		key := strings.ToLower(strings.TrimSpace(h))
		switch {
		case key == "title" || key == "track" || key == "track name" || key == "name" || key == "song":
			setOnce(col, "title", i)
		case strings.HasPrefix(key, "artist"):
			if _, exists := col["artist"]; !exists {
				col["artist"] = i
				artistList = key == "artists" || key == "artist name(s)" || key == "artist names"
			}
		case key == "album" || key == "album name":
			setOnce(col, "album", i)
		case key == "isrc":
			setOnce(col, "isrc", i)
		case strings.HasPrefix(key, "duration") || key == "length" || key == "time":
			setOnce(col, "duration", i)
			durationMS = strings.Contains(key, "ms")
		}
	}
	if _, ok := col["title"]; !ok {
		return "", nil, ErrUnreadable
	}
	cell := func(row []string, k string) string {
		i, ok := col[k]
		if !ok || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	var out []core.ImportTrack
	for _, row := range rows[1:] {
		artist := cell(row, "artist")
		if artistList {
			artist = firstArtist(artist)
		}
		t := core.ImportTrack{Title: cell(row, "title"), Artist: artist, Album: cell(row, "album"), ISRC: strings.ToUpper(cell(row, "isrc"))}
		t.DurationSec = duration(cell(row, "duration"), durationMS)
		if t.Title != "" {
			out = append(out, t)
		}
		if len(out) >= MaxTracks {
			break
		}
	}
	if len(out) == 0 {
		return "", nil, ErrUnreadable
	}
	return "", out, nil
}

func setOnce(m map[string]int, k string, i int) {
	if _, ok := m[k]; !ok {
		m[k] = i
	}
}

// firstArtist keeps the main artist of "A, B" or "A; B".
func firstArtist(s string) string {
	for _, sep := range []string{";", ","} {
		if a, _, ok := strings.Cut(s, sep); ok {
			return strings.TrimSpace(a)
		}
	}
	return s
}

// duration reads seconds, milliseconds (ms columns) or m:ss.
func duration(s string, ms bool) float64 {
	if s == "" {
		return 0
	}
	if m, sec, ok := strings.Cut(s, ":"); ok {
		mi, err1 := strconv.Atoi(m)
		se, err2 := strconv.ParseFloat(sec, 64)
		if err1 == nil && err2 == nil {
			return float64(mi)*60 + se
		}
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	if ms {
		return v / 1000
	}
	return v
}
