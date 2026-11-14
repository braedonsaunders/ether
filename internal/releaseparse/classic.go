package releaseparse

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

var (
	reClassicPartEpisode    = regexp.MustCompile(`(?i)\b(?:Part|Pt)\.?\s*(\d{1,2})\b`)
	reClassicLeadingEpisode = regexp.MustCompile(`^\s*(\d{2,3})(?:\s*[-._]\s*|\s+)`)
)

// ClassicPackEpisodeMatches reports whether a file in a season pack names the
// wanted episode the classic way, when no SxxExx is present: "Season 2/05 -
// Title.mkv", "205 - Title.avi", or a miniseries "Part 2 - Title.mkv". A
// season folder in the path must be the wanted season.
func ClassicPackEpisodeMatches(filePath string, wantSeason, wantEp int) bool {
	if wantSeason == 0 {
		return false
	}
	normPath := strings.ReplaceAll(filePath, "\\", "/")
	dirInfo := Parse(strings.ReplaceAll(path.Dir(normPath), "/", " "))
	if len(dirInfo.Seasons) > 0 {
		seasonOK := false
		for _, s := range dirInfo.Seasons {
			if s == wantSeason {
				seasonOK = true
			}
		}
		if !seasonOK {
			return false
		}
	}
	base := path.Base(normPath)
	if wantSeason == 1 {
		if m := reClassicPartEpisode.FindStringSubmatch(base); m != nil {
			if ep, err := strconv.Atoi(m[1]); err == nil && ep == wantEp {
				return true
			}
		}
	}
	if m := reClassicLeadingEpisode.FindStringSubmatch(base); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return false
		}
		// Avoid mistaking 4-digit years or resolutions ("1080p") for episode numbers.
		if n == wantEp && (len(dirInfo.Seasons) > 0 || wantSeason == 1) {
			return true
		}
		if wantSeason > 0 && wantEp > 0 && wantEp < 100 && n == wantSeason*100+wantEp {
			return true
		}
	}
	return false
}
