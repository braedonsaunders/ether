// Package releaseparse parses scene and P2P release names into
// core.ReleaseInfo. It is pure string work and safe for concurrent use.
package releaseparse

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

var (
	reYear                 = regexp.MustCompile(`\b(188[89]|189\d|19\d{2}|20\d{2})\b`)
	reTemporadaSE          = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])T(\d{1,2})E(\d{1,3})\b`)
	reSE                   = regexp.MustCompile(`(?i)\bS(\d{1,3})[\s\p{So}\x{FE0F}\x{200D}-]*E(\d{1,4})((?:\s*E\d{1,4})*)(?:\s*-\s*E?(\d{1,4}))?(?:v(\d))?\b`)
	reSpanishCapUnderscore = regexp.MustCompile(`(?i)\b(Cap(?:[ií]tulo)?[.\s]*\d{1,4})_(\d{1,4})\b`)
	reSpanishEpisode       = regexp.MustCompile(`(?i)\b(?:Cap|Cap[ií]tulo)\s*(\d{3,4})(?:\s*(?:-|al|a)\s*(?:Cap(?:[ií]tulo)?\s*)?(\d{1,4}))?\b`)
	reSpanishShortEpisode  = regexp.MustCompile(`(?i)\b(?:Cap|Cap[ií]tulo)\s*0?([1-9]\d?)(?:\s*(?:-|al|a)\s*(?:Cap(?:[ií]tulo)?\s*)?0?([1-9]\d?))?\b`)
	reBracketedSpanishCap  = regexp.MustCompile(`(?i)\[\s*(?:Cap|Cap[ií]tulo)\b`)
	reSeasonAndEp          = regexp.MustCompile(`(?i)\b(?:Season|Series|Saison|Stagione|Temporada)\s*(\d{1,2})\b[\s-]*\b(?:Episode|Ep)\s*(\d{1,3})\b`)
	reSeasonOnly           = regexp.MustCompile(`(?i)\bS(\d{1,2})(?:\s*(?:-|to)\s*S?(\d{1,2}))?\b`)
	reSeasonWord           = regexp.MustCompile(`(?i)\b(?:Seasons?|Saison|Stagione|Temporada)\s*(\d{1,2})(?:\s*(?:-|to)\s*(?:Seasons?|S)?\s*(\d{1,2}))?\b`)
	reXEpisode             = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{1,3})(?:\s*-\s*(?:\d{1,2}x)?(\d{1,3}))?(?:\s+(?:из|of)\s+(\d{1,3}))?\b`)
	reBareEpisode          = regexp.MustCompile(`(?i)\b(?:Episodes?\s*(\d{1,3})|Ep\s*(\d{1,3})|E(\d{2,3}))(?:\s*(?:-|~|to)\s*(?:Episodes?\s*|Ep\s*|E)?(\d{1,3}))?\b`)
	reDashEpisode          = regexp.MustCompile(`(?i)\s-\s*(\d{2,3})\s+(?:\d{3,4}[pi]\b|BD\b|BLURAY\b|WEB|HDTV\b|DVD|X264\b|X265\b|HEVC\b|H264\b|H265\b|AV1\b|XVID\b)`)
	reEpRange              = regexp.MustCompile(`(?i)E(\d{1,4})`)
	// Anime absolute numbering and "Season N - EE" (parseAbsoluteEpisode,
	// parseSeasonDashEpisode).
	reAbsoluteBracket     = regexp.MustCompile(`(?i)(?:^|\s)-\s+(\d{1,4})(?:\s*v\d{1,2})?(?:\s*[\[(]|\s*$|\s+(?:END|FINAL)\b)`)
	reAbsoluteTag         = regexp.MustCompile(`(?i)\s-\s(\d{1,4})(?:\s*v\d{1,2})?`)
	reDashNumber          = regexp.MustCompile(`\s-\s(\d{1,4})(?:\s*v\d{1,2})?\b`)
	reSeasonShortDashEp   = regexp.MustCompile(`(?i)\bS(\d{1,2})\s+-\s+(\d{1,4})(?:v\d{1,2})?\b`)
	reSeasonDashEp        = regexp.MustCompile(`(?i)\b(?:Season|Saison|Stagione|Temporada)\s*(\d{1,2})\s+-\s+(\d{1,4})(?:v\d{1,2})?\b`)
	reSeasonCourDashEp    = regexp.MustCompile(`(?i)\b(?:S(\d{1,2})|(?:Season|Saison|Stagione|Temporada)\s*(\d{1,2}))\s+(?:Part|Cour)\s*(\d{1,2})(?:\s+-\s+|\s+E?)(\d{1,3})(?:v(\d))?\b`)
	reCourToken           = regexp.MustCompile(`(?i)\bCour\s*(\d{1,2})\b`)
	reSeasonPartToken     = regexp.MustCompile(`(?i)\b(?:S\d{1,2}|(?:Season|Saison|Stagione|Temporada)\s*\d{1,2})\s+(?:Part|Cour)\s*(\d{1,2})\b`)
	reRevisionSuffix      = regexp.MustCompile(`(?i)(?:\bS\d{1,2}[\s-]*E\d{1,3}|\b\d{1,4})v([2-9])\b`)
	reStandaloneRev       = regexp.MustCompile(`(?i)(?:^|\s)v([2-9])\b`)
	reChannels            = regexp.MustCompile(`([257])[\s.]([01])\b`)
	reRawChannels         = regexp.MustCompile(`\b(5\.1|7\.1|2\.0|2\.1|6\.1)\b`)
	reGroupTail           = regexp.MustCompile(`-([A-Za-z0-9][A-Za-z0-9_]{1,})$`)
	reBracket             = regexp.MustCompile(`\[([^\]]+)\]`)
	reLeadingWWWBracket   = regexp.MustCompile(`(?i)^\[www\.[^\]]+\]\s*[-–—]?\s*`)
	reSpanishYear         = regexp.MustCompile(`(?i)\bA[ñn]o\s+(188[89]|189\d|19\d{2}|20\d{2})\b`)
	reTrailingBracketYear = regexp.MustCompile(`(?i)[\[(]\s*(?:A[ñn]o\s+)?(188[89]|189\d|19\d{2}|20\d{2})\s*[\])](?:\.(?:part\d+|rar|zip|7z|iso))?\s*$`)
	reBracketedYear       = regexp.MustCompile(`[\[(]\s*(?:188[89]|189\d|19\d{2}|20\d{2})\s*[\])]`)
)

var containers = map[string]bool{
	"mkv": true, "mp4": true, "avi": true, "m4v": true, "mov": true,
	"wmv": true, "ts": true, "m2ts": true, "webm": true, "mpg": true, "mpeg": true,
}

// languages are audio language tags. MULTI is read separately
// (multiAudioAfter) and subtitle-only tags such as VOSTFR live in
// subtitleTags. Regional variants are named by language.go's constants.
var languages = []struct{ token, name string }{
	{"FRENCH", "french"}, {"TRUEFRENCH", "french"}, {"VFF", "french"}, {"VFQ", "french"}, {"VF2", "french"},
	{"GERMAN", "german"}, {"SPANISH", "spanish"}, {"ESPANOL", "spanish"}, {"ESPAÑOL", "spanish"}, {"CASTELLANO", LanguageSpanishSpain}, {"CASTILIAN", LanguageSpanishSpain}, {"LATINO", LanguageSpanishLatino}, {"LATAM", LanguageSpanishLatino}, {"ITALIAN", "italian"},
	{"DUTCH", "dutch"}, {"PORTUGUESE", "portuguese"}, {"RUSSIAN", "russian"}, {"RUS", "russian"}, {"UKRAINIAN", "ukrainian"}, {"UKR", "ukrainian"}, {"POLISH", "polish"},
	{"KOREAN", "korean"}, {"JAPANESE", "japanese"}, {"CHINESE", "chinese"}, {"MANDARIN", "chinese"},
	{"HINDI", "hindi"}, {"TELUGU", "telugu"}, {"TAMIL", "tamil"}, {"TURKISH", "turkish"},
	{"ARABIC", "arabic"}, {"SWEDISH", "swedish"}, {"NORWEGIAN", "norwegian"}, {"DANISH", "danish"},
	{"FINNISH", "finnish"}, {"CZECH", "czech"}, {"HUNGARIAN", "hungarian"}, {"GREEK", "greek"},
	{"THAI", "thai"}, {"VIETNAMESE", "vietnamese"}, {"INDONESIAN", "indonesian"},
	{"ENGLISH", "english"}, {"INGLES", "english"}, {"INGLÉS", "english"}, {"ENG", "english"},
	{"ITA", "italian"}, {"JAP", "japanese"}, {"JPN", "japanese"},
	{"GER", "german"}, {"DEU", "german"}, {"FRE", "french"}, {"FRA", "french"},
	{"SPA", "spanish"}, {"POR", "portuguese"}, {"KOR", "korean"},
	// Scene audio codes of Spanish-language groups ("LAT.DDP5.1",
	// "ESP.AC3") and the remaining ISO 639-2 codes groups write. NORDIC
	// stays out: it marks Nordic subtitles over the original audio.
	{"LAT", LanguageSpanishLatino}, {"ESP", LanguageSpanishSpain}, {"CAST", LanguageSpanishSpain},
	{"POL", "polish"}, {"HUN", "hungarian"}, {"CZE", "czech"}, {"GRE", "greek"}, {"TUR", "turkish"},
	{"HEB", "hebrew"}, {"ARA", "arabic"}, {"HIN", "hindi"}, {"TAM", "tamil"}, {"TEL", "telugu"},
	{"SWE", "swedish"}, {"DAN", "danish"}, {"THA", "thai"}, {"VIE", "vietnamese"}, {"CHI", "chinese"},
	// Explicit regional language tags are used by WEB release groups.
	// Do not treat bare country codes (BR/US/GB) as audio languages.
	{"PT-BR", LanguagePortugueseBrazil}, {"BRAZILIAN", LanguagePortugueseBrazil}, {"DUBLADO", LanguagePortugueseBrazil}, {"NACIONAL", LanguagePortugueseBrazil}, {"PT-PT", LanguagePortuguesePortugal},
	{"ES-ES", LanguageSpanishSpain}, {"ES-MX", LanguageSpanishLatino}, {"ES-419", LanguageSpanishLatino},
	{"EN-US", "english"}, {"EN-GB", "english"},
	// Regional releases spell out languages as English names or endonyms.
	// These use the same metadata floor and subtitle-position rules as scene tags.
	{"MALAYALAM", "malayalam"}, {"KANNADA", "kannada"}, {"BENGALI", "bengali"}, {"MARATHI", "marathi"},
	{"CROATIAN", "croatian"}, {"SERBIAN", "serbian"}, {"BOSNIAN", "bosnian"}, {"SLOVENIAN", "slovenian"},
	{"SLOVAK", "slovak"}, {"ROMANIAN", "romanian"}, {"BULGARIAN", "bulgarian"}, {"MACEDONIAN", "macedonian"},
	{"ALBANIAN", "albanian"}, {"LITHUANIAN", "lithuanian"}, {"LATVIAN", "latvian"}, {"ESTONIAN", "estonian"},
	{"ICELANDIC", "icelandic"}, {"HEBREW", "hebrew"}, {"PERSIAN", "persian"}, {"URDU", "urdu"},
	{"MALAY", "malay"}, {"TAGALOG", "tagalog"}, {"CATALAN", "catalan"}, {"AFRIKAANS", "afrikaans"},
	{"GEORGIAN", "georgian"}, {"IRISH", "irish"}, {"MONGOLIAN", "mongolian"}, {"ROMANSH", "romansh"},
	{"HRVATSKI", "croatian"}, {"SRPSKI", "serbian"}, {"BOSANSKI", "bosnian"}, {"SLOVENSKI", "slovenian"},
	{"SLOVENSKY", "slovak"}, {"SLOVENSKÝ", "slovak"}, {"HRV", "croatian"}, {"FARSI", "persian"}, {"FILIPINO", "tagalog"},
	{"DEUTSCH", "german"}, {"FRANÇAIS", "french"}, {"FRANCAIS", "french"}, {"POLSKI", "polish"},
	{"ITALIANO", "italian"}, {"PORTUGUÊS", "portuguese"}, {"PORTUGUES", "portuguese"}, {"NEDERLANDS", "dutch"},
	{"ČESKY", "czech"}, {"CESKY", "czech"}, {"MAGYAR", "hungarian"}, {"SUOMI", "finnish"},
	{"SVENSKA", "swedish"}, {"NORSK", "norwegian"}, {"DANSK", "danish"}, {"TÜRKÇE", "turkish"}, {"TURKCE", "turkish"},
}

func normalizeSpanishCapRange(s string) string {
	return reSpanishCapUnderscore.ReplaceAllString(s, "${1}-${2}")
}

// Parse extracts structured info from a release name.
func Parse(name string) core.ReleaseInfo {
	var info core.ReleaseInfo
	raw := strings.TrimSpace(name)
	if raw == "" {
		return info
	}
	raw = strings.TrimSpace(reLeadingWWWBracket.ReplaceAllString(raw, ""))
	if raw == "" {
		return info
	}
	// Spanish and Portuguese groups write "T07E09" (Temporada 7, episode 9)
	// for "S07E09".
	raw = reTemporadaSE.ReplaceAllString(raw, "${1}S${2}E${3}")
	stem, group, container := parseGroup(raw)
	info.Group = group
	info.Container = container

	stem, leadingEpisode := stripLeadingEpisode(stem)
	normStem := normalizeSpanishCapRange(stem)
	spaced := spacify(normStem)
	upper := " " + strings.ToUpper(spaced) + " "

	// Season / episode.
	absolute, absoluteAt := parseAbsoluteEpisode(normStem, spaced)
	var markerAt int
	if absolute > 0 {
		// Explicit anime numbering must not be reinterpreted as packed chapters.
		markerAt = parseStandardEpisodes(spaced, upper, &info)
	} else {
		markerAt = parseEpisodesWithRaw(normStem, spaced, upper, &info)
	}
	if markerAt < 0 && leadingEpisode > 0 {
		info.Episodes = []int{leadingEpisode}
	}
	if abs, at := absolute, absoluteAt; abs > 0 && len(info.Seasons) == 0 {
		// Anime numbers episodes from the series start ("Show - 1071");
		// such a number is only an absolute episode, never a season's
		// episode, so movie titles and SxxEyy names keep their meaning.
		info.AbsoluteEpisodes = []int{abs}
		if markerAt < 0 || at < markerAt {
			markerAt = at
		}
	}
	// A parenthesized absolute number supplements, rather than replaces,
	// the release's explicit season/episode numbering.
	if abs, _, _ := seasonAbsoluteAnnotation(normStem); abs > 0 && len(info.Episodes) == 1 {
		info.AbsoluteEpisodes = []int{abs}
	}
	var yearAt int
	info.Year, yearAt = parseYearAtWithStem(normStem, spaced)
	metaAt := firstMetadataAt(normStem, spaced)
	// Words before the release year, episode/season marker, or post-title
	// technical metadata block are the title ("The.Spanish.Princess.S01E01",
	// "The.French.Dispatch.2021", "Pelicula [BluRay 1080p][Castellano] (2019)").
	// When no year or episode marker is present (floor < 0), floor stays -1 so
	// unbracketed post-title tags ("Movie.Title.FRENCH.1080p") still count.
	floor := yearAt
	if markerAt >= 0 && (floor < 0 || markerAt < floor) {
		floor = markerAt
	}
	if floor > 0 && metaAt > 0 && metaAt-1 < floor {
		floor = metaAt - 1
	}
	// "COMPLETE" before the release year is usually part of the title
	// ("A.Complete.Unknown.2024"), unless it names a pack. A string that
	// already names a specific episode ("Show.S01.COMPLETE/Show.S01E02.mkv")
	// is that episode, not a whole-season pack.
	if len(info.Episodes) == 0 {
		if idx := tokenIndex(spaced, "COMPLETE"); idx >= 0 && (idx > yearAt || len(info.Seasons) > 0 || namesPack(spaced[idx+len("COMPLETE"):])) {
			info.FullSeason = true
		}
	}
	info.Height = parseHeight(upper)
	source := parseSource(upper)
	// Short anime disc tags count only in release metadata. Do not mistake
	// "BD" in a title or an upload group for its source.
	metadata := spaced[max(floor, 0):]
	if source == "" && (tokenIndex(metadata, "BD") >= 0 || tokenIndex(metadata, "BDMUX") >= 0) {
		source = core.SourceBluRay
	}
	info.Source = overrideSource(source, spaced, floor)
	info.VideoCodec = parseVideoCodec(upper)
	info.AudioCodec, info.Atmos = parseAudio(upper)
	if m := reChannels.FindStringSubmatch(strings.ToUpper(spaced)); m != nil {
		info.Channels = m[1] + "." + m[2]
	}
	// DDP5.1 style, where dot was a separator: DD 5 1 handled above; also "5.1" in raw.
	if info.Channels == "" {
		if m := reRawChannels.FindStringSubmatch(raw); m != nil {
			info.Channels = m[1]
		}
	}
	if strings.Contains(upper, "HDR10+") || strings.Contains(upper, "HDR10PLUS") || strings.Contains(upper, " HDR10 ") || strings.Contains(upper, " HDR ") || strings.Contains(upper, " HDR-") || strings.Contains(upper, "HDR10") {
		info.HDR10 = true
	}
	if strings.Contains(upper, "DOLBY VISION") || strings.Contains(upper, " DOLBYVISION ") || strings.Contains(upper, " DOVI ") || strings.Contains(upper, " DV ") || strings.Contains(upper, " DV-") || strings.HasSuffix(strings.TrimSpace(upper), " DV") {
		info.DolbyVision = true
	}
	info.Edition = ParseEdition(name)
	info.HardSubbed = HardSubbed(name)
	info.Proper = strings.Contains(upper, " PROPER ") || strings.Contains(upper, " REAL PROPER ")
	info.Repack = strings.Contains(upper, " REPACK ") || strings.Contains(upper, " RERIP ")
	info.Revision = parseRevision(spaced, floor)
	if info.Revision >= 2 && !info.Repack {
		info.Proper = true
	}
	if info.Cour == 0 {
		info.Cour = parseCour(spaced, floor)
	}
	info.Subbed = parseSubbedAfter(spaced, floor)
	info.Dubbed = parseDubbedAfter(spaced, floor)
	info.Uncensored = languageTagAfter(spaced, "UNCENSORED", floor) || languageTagAfter(spaced, "UNCUT", floor)
	// Words of the title itself ("The.French.Dispatch.2021", "The Danish
	// Girl") are not audio languages.
	seen := map[string]bool{}
	if multiAudioAfter(spaced, floor) {
		seen[LanguageMulti] = true
		info.Languages = append(info.Languages, LanguageMulti)
	}
	dualKind := dualAudioKindAfter(spaced, floor)
	if dualKind == LanguageMulti && !seen[LanguageMulti] {
		seen[LanguageMulti] = true
		info.Languages = append(info.Languages, LanguageMulti)
	}
	subs := subtitlePositions(spaced, floor)
	shortAudio, subOnly := shortLanguageTags(spaced, floor, subs)
	for _, name := range shortAudio {
		if !seen[name] {
			seen[name] = true
			info.Languages = append(info.Languages, name)
		}
	}
	for _, l := range languages {
		audio, sub := languageTagRole(spaced, l.token, floor, subs)
		switch {
		case audio && !seen[l.name]:
			seen[l.name] = true
			info.Languages = append(info.Languages, l.name)
		case sub && !audio:
			subOnly = append(subOnly, l.name)
		}
	}
	if dualKind == LanguageDual && !seen[LanguageMulti] {
		if seen["english"] {
			seen[LanguageMulti] = true
			info.Languages = append(info.Languages, LanguageMulti)
		} else if !seen[LanguageDual] {
			seen[LanguageDual] = true
			info.Languages = append(info.Languages, LanguageDual)
		}
	}
	info.SubtitleLanguages = parseSubtitleLanguages(spaced, floor)
	for _, name := range subOnly {
		if !seen[name] && !slices.Contains(info.SubtitleLanguages, name) {
			info.SubtitleLanguages = append(info.SubtitleLanguages, name)
		}
	}
	if len(info.SubtitleLanguages) > 0 {
		info.Subbed = true
	}
	info.Title = extractTitle(spaced, yearAt, markerAt, metaAt, floor, &info)
	return info
}

func parseRevision(spaced string, floor int) int {
	if m := reRevisionSuffix.FindStringSubmatch(spaced); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 2 {
			return n
		}
	}
	if floor >= 0 && floor < len(spaced) {
		if m := reStandaloneRev.FindStringSubmatch(spaced[floor:]); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 2 {
				return n
			}
		}
	}
	return 0
}

func parseCour(spaced string, floor int) int {
	if m := reSeasonPartToken.FindStringSubmatch(spaced); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 && n <= 10 {
			return n
		}
	}
	meta := spaced
	if floor >= 0 && floor < len(spaced) {
		meta = spaced[floor:]
	}
	if m := reCourToken.FindStringSubmatch(meta); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 && n <= 10 {
			return n
		}
	}
	return 0
}

func parseSubbedAfter(spaced string, floor int) bool {
	for _, tag := range []string{"SUBBED", "ESUB", "ESUBS", "MSUB", "MSUBS", "SOFTSUB", "SOFTSUBS"} {
		if languageTagAfter(spaced, tag, floor) {
			return true
		}
	}
	return false
}

func parseDubbedAfter(spaced string, floor int) bool {
	for _, tag := range []string{"DUB", "DUBBED", "DUBLADO"} {
		if languageTagAfter(spaced, tag, floor) {
			return true
		}
	}
	return false
}

// parseGroup extracts the release group and the container extension from
// raw, returning the name with the extension stripped (stem), the group and
// the container ("" when the suffix is not a known container).
func parseGroup(raw string) (stem, group, container string) {
	// Leading / trailing bracket group, e.g. [HorribleSubs] or [YTS.MX].
	// A leading bracket names the group (anime convention); a trailing one
	// is only used when there is no -GROUP suffix ("-SPARKS[rarbg]" names
	// SPARKS, and [rarbg] is the uploader).
	bracketGroup, leadingGroup := "", ""
	hasBracket := strings.IndexByte(raw, '[') >= 0
	if hasBracket {
		if ms := reBracket.FindAllStringSubmatchIndex(raw, -1); len(ms) > 0 {
			for i, m := range ms {
				v := strings.TrimSpace(raw[m[2]:m[3]])
				if v != "" && len(v) <= 20 && !looksLikeMeta(v) {
					bracketGroup = v
					if i == 0 && m[0] == 0 {
						leadingGroup = v
					}
				}
			}
		}
	}
	// Container extension.
	stem = raw
	if i := strings.LastIndex(raw, "."); i > 0 {
		ext := strings.ToLower(raw[i+1:])
		// Only treat as container if the suffix is a bare extension.
		if containers[ext] {
			container = ext
			stem = raw[:i]
		}
	}
	// Group: trailing -GROUP (after stripping bracket chunks).
	noBrackets := stem
	if hasBracket {
		noBrackets = reBracket.ReplaceAllString(stem, " ")
	}
	if leadingGroup != "" {
		group = leadingGroup
	} else if m := reGroupTail.FindStringSubmatch(strings.TrimSpace(noBrackets)); m != nil && !isKnownTail(m[1]) {
		group = m[1]
	} else {
		group = bracketGroup
	}
	return stem, group, container
}

// overrideSource applies the CAM and screener overrides to source: an explicit
// low-quality tag outranks a contradictory WEB/BD claim. Only release
// metadata (spaced at or after floor) is inspected, never words in the
// film's title.
func overrideSource(source core.Source, spaced string, floor int) core.Source {
	metadata := " " + strings.ToUpper(spaced[max(floor, 0):]) + " "
	if containsAny(metadata, " HDCAM", " HD CAM", " CAM ", " CAM-", "CAMRIP", " HQCAM", " HDTS", " HD-TS", " TSRIP", " HDTC", " HD-TC", " TELESYNC", " TELECINE", " HQ PRE ", " HQ-PRE ") {
		return core.SourceCam
	}
	// Screeners, R5 retail telecines and workprints are pre-release copies
	// whatever disc or web tag rides along ("Movie.2024.SCREENER.WEBRip").
	if containsAny(metadata, "SCREENER", "DVDSCR", "BDSCR", "WEBSCR", " SCR ", " R5 ", " WORKPRINT ") {
		return core.SourceScreener
	}
	return source
}

func looksLikeMeta(v string) bool {
	trimmed := strings.TrimSpace(v)
	u := strings.ToUpper(trimmed)
	norm := spacify(normalizeSpanishCapRange(trimmed))
	if len(spanishEpisodeLocationsWithRaw("["+trimmed+"]", norm)) > 0 || reSpanishShortEpisode.MatchString(norm) {
		return true
	}
	if reYear.FindString(trimmed) == trimmed || reSpanishYear.MatchString(norm) {
		return true
	}
	if reSeasonOnly.FindString(norm) == norm || reSE.MatchString(norm) || reXEpisode.MatchString(norm) {
		return true
	}
	// An anime CRC32 checksum ("[ABCDEF12]") is not a release group.
	if len(u) == 8 && strings.Trim(u, "0123456789ABCDEF") == "" {
		return true
	}
	if isMetadataBracket(trimmed, norm) {
		return true
	}
	for _, k := range []string{"1080", "720", "2160", "480", "BLURAY", "WEB", "HDTV", "X264", "X265", "HEVC", "HDR"} {
		if strings.Contains(u, k) {
			return true
		}
	}
	return false
}

// isMetadataBracket reports whether the contents of a post-title [...] block
// carry release metadata (source/resolution/codec/audio/language/year/chapter
// or a www.* tracker tag) rather than title words or a release group.
func isMetadataBracket(rawInner, spacedInner string) bool {
	uRaw := strings.ToUpper(strings.TrimSpace(rawInner))
	if strings.HasPrefix(uRaw, "WWW.") {
		return true
	}
	if reSpanishYear.MatchString(spacedInner) || reSpanishEpisode.MatchString(spacedInner) || reSpanishShortEpisode.MatchString(spacedInner) {
		return true
	}
	for _, kw := range boundaryKeywords {
		if kw == "SEASON" || kw == "SEASONS" {
			continue
		}
		if tokenIndex(spacedInner, kw) >= 0 {
			return true
		}
	}
	return hasAudioOrLanguageTag(spacedInner)
}

func hasAudioOrLanguageTag(seg string) bool {
	for _, w := range strings.Fields(seg) {
		u := strings.ToUpper(strings.Trim(w, "-"))
		switch u {
		case "AAC", "AC3", "EAC3", "DDP", "DDP5", "DTS", "DTS-HD", "TRUEHD", "MP3", "FLAC", "OPUS",
			"SUBT", "SUBTITULADO", "VOSE", "VOS", "VOSTFR", "SUBS", "SUBBED", "DUBBED", "UNCENSORED", "DUAL", "MULTI":
			return true
		}
		for _, l := range languages {
			if u == l.token {
				return true
			}
		}
	}
	return false
}

// firstMetadataAt returns the earliest token offset in spaced of an
// unconditional technical keyword or a post-title [...] metadata bracket, or -1.
func firstMetadataAt(stem, spaced string) int {
	cut := -1
	consider := func(idx int) {
		if idx > 0 && (cut < 0 || idx < cut) {
			cut = idx
		}
	}
	for _, kw := range boundaryKeywords {
		if !titleWordKeywords[kw] {
			consider(tokenIndex(spaced, kw))
		}
	}
	if strings.IndexByte(stem, '[') >= 0 {
		for _, m := range reBracket.FindAllStringSubmatchIndex(stem, -1) {
			if m[0] == 0 {
				continue
			}
			// Require at least one non-bracket title word before this bracket
			// so leading "[Group] Title [WEB]" skips "[Group]" and stops at "[WEB]".
			beforeNoBrackets := spacify(reBracket.ReplaceAllString(stem[:m[0]], " "))
			if beforeNoBrackets == "" {
				continue
			}
			innerRaw := stem[m[2]:m[3]]
			innerSpaced := spacify(innerRaw)
			if innerSpaced == "" || !isMetadataBracket(innerRaw, innerSpaced) {
				continue
			}
			prefix := spacify(stem[:m[0]])
			if prefix != "" && len(prefix)+1 < len(spaced) {
				consider(len(prefix) + 1)
			}
		}
	}
	return cut
}

func isKnownTail(g string) bool {
	switch strings.ToUpper(g) {
	case "DL", "RIP", "CAM", "TS", "TC", "MA", "HD", "SD", "VISION", "DV", "HDR", "ATMOS", "AUDIO", "SUBS":
		return true
	}
	return false
}

var spacifyReplacer = strings.NewReplacer(".", " ", "_", " ", "(", " ", ")", " ", "[", " ", "]", " ", "\u2013", "-", "\u2014", "-")

// spacify turns separators into spaces, keeping hyphens inside tokens.
func spacify(s string) string {
	s = spacifyReplacer.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

type spanishChapterMatch struct {
	loc      []int
	season   int
	episodes []int
}

// spanishEpisodeMatches accepts canonical packed SSEE/SEE chapter numbers and
// ranges ("Cap.101", "Cap.101_120", "Cap.101-120", "Cap.101 al 120").
// Years and common resolution numbers are ambiguous and carry no episode
// evidence. Keep this validation shared with episode-title/year boundaries.
func spanishEpisodeMatches(raw, spaced string) []spanishChapterMatch {
	var out []spanishChapterMatch
	_, yearAt := parseYearAtWithStem(raw, spaced)
	bracketed := reBracketedSpanishCap.MatchString(raw)
	for _, loc := range reSpanishEpisode.FindAllStringSubmatchIndex(spaced, -1) {
		digits := spaced[loc[2]:loc[3]]
		n, _ := strconv.Atoi(digits)
		if digits[0] == '0' || n%100 == 0 || n >= 1888 && n <= 2099 {
			continue
		}
		// Like bare "Episode N", an unbracketed chapter-like phrase right before
		// a release year can belong to a movie title ("The Cap 205 2019").
		// Bracketed "[Cap.101]" or markers accompanied by technical/language tags
		// ("Serie [HDTV][Cap.101][Castellano] [2024]") are genuine TV chapters.
		if yearAt >= loc[1] && !bracketed && !releaseMarkerIn(spaced[:loc[0]]) && !releaseMarkerIn(spaced[loc[1]:yearAt]) && !hasAudioOrLanguageTag(spaced[loc[1]:yearAt]) {
			continue
		}
		switch n {
		case 360, 480, 576, 720, 1080, 2160:
			continue
		}
		season, firstEp := n/100, n%100
		if season < 1 || firstEp < 1 {
			continue
		}
		episodes := []int{firstEp}
		effectiveEnd := loc[3]
		if loc[4] >= 0 && loc[5] >= 0 {
			endDigits := spaced[loc[4]:loc[5]]
			n2, err := strconv.Atoi(endDigits)
			if err == nil && endDigits[0] != '0' || (err == nil && len(endDigits) <= 2) {
				lastEp := 0
				switch {
				case n2 >= 100 && (n2 < 1888 || n2 > 2099) && n2/100 == season && n2%100 > firstEp:
					lastEp = n2 % 100
				case n2 > 0 && n2 < 100 && len(endDigits) <= 2 && n2 > firstEp:
					lastEp = n2
				}
				if lastEp > firstEp && lastEp-firstEp <= 200 {
					for ep := firstEp + 1; ep <= lastEp; ep++ {
						episodes = append(episodes, ep)
					}
					effectiveEnd = loc[1]
				}
			}
		}
		out = append(out, spanishChapterMatch{
			loc:      []int{loc[0], effectiveEnd},
			season:   season,
			episodes: episodes,
		})
	}
	return out
}

func spanishEpisodeLocationsWithRaw(raw, spaced string) [][]int {
	matches := spanishEpisodeMatches(raw, spaced)
	out := make([][]int, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.loc)
	}
	return out
}

func spanishEpisodeIndexWithRaw(raw, spaced string) []int {
	for _, m := range spanishEpisodeMatches(raw, spaced) {
		if m.loc[0] > 0 {
			return m.loc
		}
	}
	return nil
}

func parseEpisodesWithRaw(raw, spaced, upper string, info *core.ReleaseInfo) int {
	at := parseStandardEpisodes(spaced, upper, info)
	matchedPacked := false
	for _, m := range spanishEpisodeMatches(raw, spaced) {
		if m.loc[0] == 0 {
			continue // a title must precede the chapter marker
		}
		matchedPacked = true
		season, episodes := m.season, m.episodes
		isPack := len(episodes) > 1
		if info.FullSeason && len(info.Episodes) == 0 && slices.Contains(info.Seasons, season) {
			// A matching season declaration ("Temporada 1 [Cap.101_120]",
			// "Temporada 2 [Cap.205]") is refined by its explicit chapter(s).
			info.Seasons, info.Episodes, info.FullSeason = []int{season}, episodes, isPack
			continue
		}
		if at >= 0 && !(len(episodes) == 1 && EpisodeMatches(*info, season, episodes[0])) {
			// Contradictory markers must not become a matching episode or pack.
			info.Episodes, info.AbsoluteEpisodes, info.FullSeason = nil, nil, false
			return min(at, m.loc[0])
		}
		if at < 0 {
			info.Seasons, info.Episodes, info.FullSeason = []int{season}, episodes, isPack
			at = m.loc[0]
		}
	}
	// When an explicit single season was already declared ("Temporada 2 [Cap.05]",
	// "Temporada 1 [Cap.01_20]"), a 1-2 digit chapter marker refines that season.
	// Standalone "[Cap.20]" without a season remains rejected.
	if !matchedPacked && at >= 0 && info.FullSeason && len(info.Seasons) == 1 && len(info.Episodes) == 0 {
		if loc := reSpanishShortEpisode.FindStringSubmatchIndex(spaced[at:]); loc != nil {
			sub := submatches(spaced[at:], loc)
			firstEp, err := strconv.Atoi(sub[1])
			if err == nil && firstEp >= 1 {
				episodes := []int{firstEp}
				if sub[2] != "" {
					if lastEp, err := strconv.Atoi(sub[2]); err == nil && lastEp > firstEp && lastEp-firstEp <= 200 {
						for ep := firstEp + 1; ep <= lastEp; ep++ {
							episodes = append(episodes, ep)
						}
					}
				}
				info.Episodes = episodes
				info.FullSeason = len(episodes) > 1
			}
		}
	}
	return at
}

func parseStandardEpisodes(spaced, upper string, info *core.ReleaseInfo) int {
	if containsAny(upper, "СЕЗОН", "СЕРИ") {
		if at := parseRussianEpisodes(spaced, info); at >= 0 {
			return at
		}
	}

	if containsAny(upper, " PART ", " COUR ") {
		if loc := reSeasonCourDashEp.FindStringSubmatchIndex(spaced); loc != nil && releaseTagFollows(spaced[loc[1]:]) {
			m := submatches(spaced, loc)
			sTok := m[1]
			if sTok == "" {
				sTok = m[2]
			}
			season, _ := strconv.Atoi(sTok)
			cour, _ := strconv.Atoi(m[3])
			episode, _ := strconv.Atoi(m[4])
			if season >= 1 && season <= 50 && cour >= 1 && cour <= 10 && episode >= 1 {
				info.Seasons = []int{season}
				info.Cour = cour
				info.Episodes = []int{episode}
				return loc[0]
			}
		}
	}

	hasS := strings.Contains(upper, " S")
	if hasS {
		if seasons, episodes, at, ok := parseSEEpisode(spaced); ok {
			info.Seasons = seasons
			info.Episodes = episodes
			return at
		}
	}
	hasSeasonWord := containsAny(upper, "SEASON", "SERIES", "SAISON", "STAGIONE", "TEMPORADA")
	if hasSeasonWord {
		if season, episode, at, ok := parseSeasonEpisode(spaced); ok {
			info.Seasons = []int{season}
			info.Episodes = []int{episode}
			return at
		}
		// Anime: "Show Season 2 - 05 (1080p)" is an episode, not seasons 2-5.
		if season, episode, at, ok := parseSeasonDashEpisode(spaced); ok {
			info.Seasons = []int{season}
			info.Episodes = []int{episode}
			return at
		}
		if loc := reSeasonWord.FindStringSubmatchIndex(spaced); loc != nil {
			m := submatches(spaced, loc)
			info.Seasons = seasonRange(m)
			info.FullSeason = true
			return loc[0]
		}
	}
	if hasS {
		// Anime: "Show S2 - 05 (1080p)" is season 2's fifth episode.
		if loc := reSeasonShortDashEp.FindStringSubmatchIndex(spaced); loc != nil && releaseTagFollows(spaced[loc[1]:]) {
			m := submatches(spaced, loc)
			season, _ := strconv.Atoi(m[1])
			episode, _ := strconv.Atoi(m[2])
			if season >= 1 && episode >= 1 {
				info.Seasons, info.Episodes = []int{season}, []int{episode}
				return loc[0]
			}
		}
		if loc := reSeasonOnly.FindStringSubmatchIndex(spaced); loc != nil {
			m := submatches(spaced, loc)
			// Guard against matching the S in e.g. S01E02 (already handled) —
			// here no E followed, so it is a season pack.
			info.Seasons = seasonRange(m)
			info.FullSeason = true
			return loc[0]
		}
	}
	if strings.Contains(upper, "X") {
		if season, episodes, fullPack, at, ok := parseXEpisode(spaced); ok {
			info.Seasons = []int{season}
			info.Episodes = episodes
			info.FullSeason = fullPack
			return at
		}
	}
	if strings.Contains(upper, " E") {
		if episodes, fullPack, at, ok := parseBareEpisode(spaced); ok {
			if fullPack {
				info.FullSeason = true
			} else {
				info.Episodes = episodes
			}
			return at
		}
	}
	if strings.Contains(spaced, " - ") {
		if episode, at, ok := parseDashEpisode(spaced); ok {
			info.Episodes = []int{episode}
			return at
		}
	}
	return -1
}

// parseSEEpisode reads the SxxEyy pattern (reSE), including chained E##
// tokens (S01E02E03) and a range tail (S01E02-E04).
func parseSEEpisode(spaced string) (seasons, episodes []int, at int, ok bool) {
	loc := reSE.FindStringSubmatchIndex(spaced)
	if loc == nil {
		return nil, nil, -1, false
	}
	m := submatches(spaced, loc)
	s, _ := strconv.Atoi(m[1])
	seasons = []int{s}
	first, _ := strconv.Atoi(m[2])
	eps := []int{first}
	// Additional E## tokens chained (S01E02E03).
	for _, em := range reEpRange.FindAllStringSubmatch(m[3], -1) {
		n, _ := strconv.Atoi(em[1])
		eps = append(eps, n)
	}
	// Range tail: S01E02-E04 or S01E02-04 (captured last group beyond chained).
	if m[4] != "" && len(eps) == 1 {
		last, _ := strconv.Atoi(m[4])
		// Only expand when it really looks like a range end and is
		// a small step forward.
		if last > first && last-first <= 30 && rangeTailPresent(spaced, m[0]) {
			for n := first + 1; n <= last; n++ {
				eps = append(eps, n)
			}
		} else if last != first {
			eps = append(eps, last)
		}
	}
	sort.Ints(eps)
	return seasons, dedupeInts(eps), loc[0], true
}

// parseSeasonEpisode reads the "Season N Episode M" pattern
// (reSeasonAndEp).
func parseSeasonEpisode(spaced string) (season, episode, at int, ok bool) {
	loc := reSeasonAndEp.FindStringSubmatchIndex(spaced)
	if loc == nil {
		return 0, 0, -1, false
	}
	m := submatches(spaced, loc)
	s, _ := strconv.Atoi(m[1])
	e, _ := strconv.Atoi(m[2])
	if s < 1 || s > 50 || e < 1 {
		return 0, 0, -1, false
	}
	return s, e, loc[0], true
}

// parseXEpisode reads the NxM pattern (reXEpisode), including multi-episode
// and Russian partial-pack ranges ("[02x01-12 из 16]").
func parseXEpisode(spaced string) (season int, episodes []int, fullPack bool, at int, ok bool) {
	loc := reXEpisode.FindStringSubmatchIndex(spaced)
	if loc == nil {
		return 0, nil, false, -1, false
	}
	m := submatches(spaced, loc)
	s, _ := strconv.Atoi(m[1])
	first, _ := strconv.Atoi(m[2])
	// 1x02 is ambiguous with e.g. codec-ish tokens; season<=50 guard.
	if s < 1 || s > 50 || first < 1 {
		return 0, nil, false, -1, false
	}
	last := first
	if m[3] != "" {
		if n, err := strconv.Atoi(m[3]); err == nil && n >= first && n-first <= 200 {
			last = n
		}
	}
	if m[4] != "" {
		if total, err := strconv.Atoi(m[4]); err == nil && last > total {
			return 0, nil, false, -1, false
		}
	}
	eps := make([]int, 0, last-first+1)
	for n := first; n <= last; n++ {
		eps = append(eps, n)
	}
	// Multi-file tracker packs ("[02x01-12 из 16]", "1x01-12") need file
	// selection; plain two-part single files ("1x01-02") stay non-packs.
	isPack := last > first && (m[4] != "" || last-first >= 2)
	return s, eps, isPack, loc[0], true
}

// parseBareEpisode reads a bare episode pattern (reBareEpisode):
// "Episode N", "Ep N" or "ENN" without a season. Multi-episode pack ranges
// ("Episode 1-129") mark FullSeason when no specific episode follows.
func parseBareEpisode(spaced string) (episodes []int, fullPack bool, at int, ok bool) {
	locs := reBareEpisode.FindAllStringSubmatchIndex(spaced, -1)
	if len(locs) == 0 {
		return nil, false, -1, false
	}
	_, yearAt := parseYearAt(spaced)
	sawPack := false
	packAt := -1
	for _, loc := range locs {
		m := submatches(spaced, loc)
		// "Star Wars Episode 1 1999" has the release year after "Episode 1";
		// miniseries and season-directory files ("Band.of.Brothers.E01",
		// "Episode 05 - Name.mkv") do not.
		if m[1] != "" && m[4] == "" && yearAt >= 0 && loc[0] <= yearAt {
			continue
		}
		tok := m[1]
		if tok == "" {
			tok = m[2]
		}
		if tok == "" {
			tok = m[3]
		}
		e, err := strconv.Atoi(tok)
		if err != nil || e < 1 || e > 999 {
			continue
		}
		if m[4] != "" {
			last, err := strconv.Atoi(m[4])
			if err == nil && last > e {
				if last-e > 4 {
					if !sawPack {
						sawPack = true
						packAt = loc[0]
					}
					continue
				}
				var eps []int
				for n := e; n <= last; n++ {
					eps = append(eps, n)
				}
				return eps, false, loc[0], true
			}
		}
		return []int{e}, false, loc[0], true
	}
	if sawPack {
		return nil, true, packAt, true
	}
	return nil, false, -1, false
}

// parseDashEpisode reads the dash episode pattern (reDashEpisode):
// "Title - 02 1080p ...".
func parseDashEpisode(spaced string) (episode, at int, ok bool) {
	loc := reDashEpisode.FindStringSubmatchIndex(spaced)
	if loc == nil {
		return 0, -1, false
	}
	m := submatches(spaced, loc)
	e, err := strconv.Atoi(m[1])
	if err != nil || e < 1 || e > 500 {
		return 0, -1, false
	}
	return e, loc[0], true
}

// parseSeasonDashEpisode reads "Season N - EE" followed by the end of the
// name or a release tag (reSeasonDashEp).
func parseSeasonDashEpisode(spaced string) (season, episode, at int, ok bool) {
	loc := reSeasonDashEp.FindStringSubmatchIndex(spaced)
	if loc == nil || !releaseTagFollows(spaced[loc[1]:]) {
		return 0, 0, -1, false
	}
	m := submatches(spaced, loc)
	s, _ := strconv.Atoi(m[1])
	e, _ := strconv.Atoi(m[2])
	if s < 1 || s > 50 || e < 1 {
		return 0, 0, -1, false
	}
	return s, e, loc[0], true
}

// parseAbsoluteEpisode reads an anime episode number counted from the
// series start: "[Group] Show - 1071 (1080p)", "Show - 05v2 [1080p]",
// "Show - 12 END". stem keeps the name's brackets, which mark the number's
// end; at is the dash's offset in spaced. Batch ranges ("01-12") and
// years ("Godzilla - 1954 (1080p)") are not episodes.
func parseAbsoluteEpisode(stem, spaced string) (episode, at int) {
	if reSeasonCourDashEp.MatchString(spaced) {
		return 0, -1
	}
	m := reAbsoluteBracket.FindStringSubmatch(stem)
	if m == nil {
		if m = reAbsoluteTag.FindStringSubmatch(spaced); m == nil || !releaseTagFollows(spaced[strings.Index(spaced, m[0])+len(m[0]):]) {
			return 0, -1
		}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 || n > 9999 || (len(m[1]) == 4 && n >= 1888 && n <= 2100) {
		return 0, -1
	}
	for _, loc := range reDashNumber.FindAllStringSubmatchIndex(spaced, -1) {
		if v, _ := strconv.Atoi(spaced[loc[2]:loc[3]]); v == n {
			return n, loc[0]
		}
	}
	return 0, -1
}

var reAbsoluteAnnotation = regexp.MustCompile(`^\s*\(\s*(\d{1,4})\s*\)`)

// seasonAbsoluteAnnotation recognizes only a numeric parenthesis immediately
// after a single S/E marker. Years retain their existing meaning.
func seasonAbsoluteAnnotation(name string) (number, start, end int) {
	marker := reSE.FindStringSubmatchIndex(name)
	if marker == nil || marker[6] != marker[7] || marker[8] >= 0 {
		return 0, 0, 0
	}
	annotation := reAbsoluteAnnotation.FindStringSubmatchIndex(name[marker[1]:])
	if annotation == nil {
		return 0, 0, 0
	}
	n, _ := strconv.Atoi(name[marker[1]+annotation[2] : marker[1]+annotation[3]])
	if n <= 0 || n >= 1888 && n <= 2100 {
		return 0, 0, 0
	}
	return n, marker[1], marker[1] + annotation[1]
}

// releaseTagFollows reports whether rest (the text after an episode
// number) is empty or starts with a release tag: a resolution, source,
// codec, an anime CRC or END.
func releaseTagFollows(rest string) bool {
	tok, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if tok == "" {
		return true
	}
	u := strings.ToUpper(tok)
	if reResolution.MatchString(u) || (len(u) == 8 && strings.Trim(u, "0123456789ABCDEF") == "") {
		return true
	}
	switch u {
	case "END", "FINAL", "BD", "BLURAY", "WEB", "WEB-DL", "WEBRIP", "HDTV", "DVD", "X264", "X265", "HEVC", "H264", "H265", "AV1", "AAC", "FLAC", "UNCENSORED", "VOSTFR", "MULTI", "DUAL":
		return true
	}
	return false
}

// seasonRange expands submatches carrying a season and an optional range
// end (group 2) into the season list, capped at a 30-season span.
func seasonRange(m []string) []int {
	s, _ := strconv.Atoi(m[1])
	seasons := []int{s}
	if m[2] != "" {
		end, _ := strconv.Atoi(m[2])
		for n := s + 1; n <= end && n-s <= 30; n++ {
			seasons = append(seasons, n)
		}
	}
	return seasons
}

// submatches turns FindStringSubmatchIndex output into strings ("" for a
// group that did not participate).
func submatches(s string, loc []int) []string {
	out := make([]string, len(loc)/2)
	for i := range out {
		if loc[2*i] >= 0 {
			out[i] = s[loc[2*i]:loc[2*i+1]]
		}
	}
	return out
}

func rangeTailPresent(spaced, match string) bool {
	return strings.Contains(strings.ToUpper(match), "-")
}

func dedupeInts(in []int) []int {
	out := in[:0:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

var reResolution = regexp.MustCompile(`(?i)\b\d{3,4}[pi]\b`)

func parseYear(spaced string, info *core.ReleaseInfo) int {
	y, _ := parseYearAt(spaced)
	return y
}

// parseYearAt returns the release year and its offset in spaced, or (0, -1).
func parseYearAt(spaced string) (int, int) {
	return parseYearAtWithStem("", spaced)
}

func parseYearAtWithStem(stem, spaced string) (int, int) {
	if loc := reSpanishYear.FindStringSubmatchIndex(spaced); loc != nil {
		y, _ := strconv.Atoi(spaced[loc[2]:loc[3]])
		return y, loc[2]
	}
	locs := reYear.FindAllStringIndex(spaced, -1)
	if len(locs) == 0 {
		return 0, -1
	}
	// NewPCT-style releases place the year in a trailing "[2019]" or "(2019)"
	// after "[BluRay 1080p][Castellano]" brackets. When the title itself starts
	// with or contains an unbracketed year-like number ("1917", "Blade Runner 2049",
	// "2001 Odisea del espacio") and no bracketed/parenthesized year preceded
	// the technical brackets, that trailing bracketed year is the release year.
	if stem != "" && len(locs) > 1 {
		if firstBracket := strings.IndexByte(stem, '['); firstBracket > 0 && !reBracketedYear.MatchString(stem[:firstBracket]) {
			if m := reTrailingBracketYear.FindStringSubmatch(stem); m != nil {
				last := locs[len(locs)-1]
				if spaced[last[0]:last[1]] == m[1] {
					y, _ := strconv.Atoi(m[1])
					return y, last[0]
				}
			}
		}
	}
	// A year-like title ("1917", "Blade Runner 2049", "Wonder Woman 1984")
	// is followed by the release year: the last year before the episode
	// or resolution markers is the release year.
	limit := len(spaced)
	if loc := reSE.FindStringIndex(spaced); loc != nil {
		limit = loc[0]
	}
	if loc := reResolution.FindStringIndex(spaced); loc != nil && loc[0] < limit {
		limit = loc[0]
	}
	pick := locs[0]
	for _, l := range locs[1:] {
		// Release tags after a year ("Movie.1984.Remastered.2014") mean
		// the metadata has begun: that year was the release year, and
		// later ones are remaster or group years.
		if l[0] < limit && releaseMarkerIn(spaced[pick[1]:l[0]]) {
			break
		}
		// The end of a span ("1994-2004") is not a release year.
		rangeEnd := l[0] >= 2 && spaced[l[0]-1] == '-' && spaced[l[0]-2] >= '0' && spaced[l[0]-2] <= '9'
		if l[0] < limit && !rangeEnd {
			pick = l
		}
	}
	y, _ := strconv.Atoi(spaced[pick[0]:pick[1]])
	return y, pick[0]
}

// releaseMarkerIn reports whether seg carries a release tag (a boundary
// keyword or an edition label) rather than only title words.
func releaseMarkerIn(seg string) bool {
	for _, kw := range boundaryKeywords {
		if tokenIndex(seg, kw) >= 0 {
			return true
		}
	}
	return ParseEdition(seg) != ""
}

// namesPack reports whether the words after "COMPLETE" describe a pack
// ("Complete Series", "Complete S01-S05").
func namesPack(rest string) bool {
	rest = strings.TrimLeft(rest, " -")
	next, _, _ := strings.Cut(rest, " ")
	switch strings.ToUpper(next) {
	case "SERIES", "SEASON", "SEASONS", "PACK", "COLLECTION", "BOXSET":
		return true
	}
	loc := reSeasonOnly.FindStringIndex(rest)
	return loc != nil && loc[0] == 0
}

// languageTagAfter reports whether the ASCII tag occurs in spaced after
// offset floor, preceded by the start or a space and followed by the end, a
// space or a hyphen.
func languageTagAfter(spaced, tag string, floor int) bool {
	for i := max(floor+1, 0); i+len(tag) <= len(spaced); i++ {
		if i > 0 && spaced[i-1] != ' ' {
			continue
		}
		end := i + len(tag)
		if end < len(spaced) && spaced[end] != ' ' && spaced[end] != '-' {
			continue
		}
		if strings.EqualFold(spaced[i:end], tag) {
			return true
		}
	}
	return false
}

// subtitlePositions marks the offsets in spaced (after floor) of language
// tags that declare subtitles rather than audio: a tag right before a SUB,
// SUBS or SUBTITLES token ("ENG.SUBS"), and each tag in the run of tags
// right after one ("Sub.ITA.ENG").
func subtitlePositions(spaced string, floor int) map[int]bool {
	type token struct {
		at   int
		text string
	}
	var toks []token
	for i := max(floor+1, 0); i < len(spaced); {
		if spaced[i] == ' ' {
			i++
			continue
		}
		j := i
		for j < len(spaced) && spaced[j] != ' ' {
			j++
		}
		toks = append(toks, token{i, spaced[i:j]})
		i = j
	}
	isSub := func(s string) bool {
		switch strings.ToUpper(s) {
		case "SUB", "SUBS", "SUBTITLE", "SUBTITLES", "SUBBED":
			return true
		}
		return false
	}
	isTag := func(s string) bool {
		head, _, _ := strings.Cut(s, "-") // "FRE-GRP": the group follows the last tag
		if _, known := isoLanguages[strings.ToLower(head)]; known {
			return true
		}
		for _, l := range languages {
			if strings.EqualFold(s, l.token) || strings.EqualFold(head, l.token) {
				return true
			}
		}
		return false
	}
	out := map[int]bool{}
	for k, t := range toks {
		if !isSub(t.text) {
			continue
		}
		if k > 0 && isTag(toks[k-1].text) {
			out[toks[k-1].at] = true
		}
		for n := k + 1; n < len(toks) && isTag(toks[n].text); n++ {
			out[toks[n].at] = true
		}
	}
	return out
}

// languageTagRole reports whether tag occurs after floor as an audio
// declaration, and whether it occurs in a subtitle position (subs).
func languageTagRole(spaced, tag string, floor int, subs map[int]bool) (audio, sub bool) {
	for i := max(floor+1, 0); i+len(tag) <= len(spaced); i++ {
		if i > 0 && spaced[i-1] != ' ' {
			continue
		}
		end := i + len(tag)
		if end < len(spaced) && spaced[end] != ' ' && spaced[end] != '-' {
			continue
		}
		if !strings.EqualFold(spaced[i:end], tag) {
			continue
		}
		if subs[i] {
			sub = true
		} else {
			audio = true
		}
	}
	return audio, sub
}

// tokenIndex returns the offset in s of the first case-insensitive match of
// the ASCII keyword kw that is a whole token (bounded by the string ends,
// spaces or hyphens), or -1.
func tokenIndex(s, kw string) int {
	for i := 0; i+len(kw) <= len(s); i++ {
		if i > 0 && s[i-1] != ' ' && s[i-1] != '-' {
			continue
		}
		end := i + len(kw)
		if end < len(s) && s[end] != ' ' && s[end] != '-' {
			continue
		}
		if strings.EqualFold(s[i:end], kw) {
			return i
		}
	}
	return -1
}

func parseHeight(upper string) int {
	// An explicit resolution wins over a source hint: "1080p.UHD.BluRay"
	// is a 1080p encode of a UHD disc.
	switch {
	case strings.Contains(upper, "2160P") || strings.Contains(upper, "2160I"):
		return 2160
	case strings.Contains(upper, "1080P") || strings.Contains(upper, "1080I"):
		return 1080
	case strings.Contains(upper, "720P") || strings.Contains(upper, "720I"):
		return 720
	case strings.Contains(upper, "576P") || strings.Contains(upper, "576I"):
		return 576
	case strings.Contains(upper, "480P") || strings.Contains(upper, "480I"):
		return 480
	case strings.Contains(upper, "360P"):
		return 360
	case strings.Contains(upper, " 4K ") || strings.Contains(upper, " 4K-") || strings.Contains(upper, " UHD ") || strings.HasSuffix(strings.TrimSpace(upper), "UHD") || strings.Contains(upper, "UHD ") || containsAny(upper, "FULLUHD", "4KFULLUHD", "UHDREMUX", "UHDRIP", "UHD-RIP"):
		return 2160
	}
	return 0
}

// reRemux matches REMUX as a word of its own ("BluRay.REMUX", "BDRemux",
// "Remux-2160p"), never inside another word ("~ RemuxDoc", an uploader).
var reRemux = regexp.MustCompile(`(^|[ \-\[(])(BD|UHD|BR)?REMUX($|[ \-\])])`)

func parseSource(upper string) core.Source {
	switch {
	case reRemux.MatchString(upper):
		return core.SourceRemux
	case containsAny(upper, "BLURAY", "BLU-RAY", "BDRIP", "BRRIP", "BD-RIP", "MICROHD", "FULLBLURAY", "FULLUHD", "4KFULLUHD", "UHDRIP", "UHD-RIP", "HDDVD", "HD-DVD"):
		return core.SourceBluRay
	case containsAny(upper, "WEBRIP", "WEB-RIP", " WEB RIP ", "WEB-DLRIP", "WEBDLRIP", "HDRIP", "HD-RIP"):
		return core.SourceWEBRip
	case containsAny(upper, "WEB-DL", "WEBDL", " WEB DL", " WEB "):
		return core.SourceWEBDL
	case strings.Contains(upper, " HDTV") || strings.Contains(upper, "HDTV ") ||
		containsAny(upper, " PDTV ", " DSR ", " DSRIP ", " TVRIP ", " SATRIP ", " DTHRIP ", " SDTV "):
		return core.SourceHDTV
	case containsAny(upper, "DVDSCR", " SCR ", "SCREENER"):
		return core.SourceScreener
	case containsAny(upper, "DVDRIP", "DVD-RIP", " DVD ", " DVDR ", " DVD5 ", " DVD9 ", " DVD-5 ", " DVD-9 ", " VHSRIP ", " LDRIP ", " LASERDISC "):
		return core.SourceDVD
	case containsAny(upper, "HDCAM", " HD CAM", " CAM ", " CAM-", "CAMRIP", "HQCAM", "HDTS", "HD-TS", "TSRIP", "HDTC", "HD-TC", " TELESYNC", " TELECINE", " TS ", " TC ", "-TS ", "-TC "):
		return core.SourceCam
	case strings.HasSuffix(strings.TrimSpace(upper), "CAM"):
		return core.SourceCam
	}
	return core.SourceUnknown
}

func parseVideoCodec(upper string) string {
	switch {
	case containsAny(upper, "X265", "H265", "H 265", "HEVC"):
		return "hevc"
	case containsAny(upper, " AV1", "-AV1", " AV1 "):
		return "av1"
	case containsAny(upper, " VP9"):
		return "vp9"
	case containsAny(upper, "X264", "H264", "H 264", " AVC"):
		return "h264"
	case containsAny(upper, " VC-1", " VC1"):
		return "vc1"
	case containsAny(upper, "XVID", "DIVX"):
		return "xvid"
	case containsAny(upper, "MPEG2", "MPEG-2"):
		return "mpeg2"
	}
	return ""
}

func parseAudio(upper string) (string, bool) {
	atmos := strings.Contains(upper, "ATMOS")
	// Fix spaced variants: DTS-HD MA etc are contiguous enough.
	switch {
	case containsAny(upper, "TRUEHD", "TRUE-HD", "TRUE HD"):
		return "truehd", atmos
	case containsAny(upper, "DTS-HD", "DTSHD", "DTS HD", "DTS-HDMA", "DTSX", "DTS-X"):
		return "dtshd", atmos
	case containsAny(upper, "EAC3", "E-AC3", "E AC3", "DDP", "DD+", " DD PLUS"):
		return "eac3", atmos
	case strings.Contains(upper, " DTS") || strings.Contains(upper, "-DTS"):
		return "dts", atmos
	case containsAny(upper, " FLAC"):
		return "flac", atmos
	case containsAny(upper, " OPUS"):
		return "opus", atmos
	case containsAny(upper, "AC3", " DD5", " DD ", " DD-", "DOLBY DIGITAL"):
		return "ac3", atmos
	case containsAny(upper, " AAC"):
		return "aac", atmos
	case containsAny(upper, " MP3"):
		return "mp3", atmos
	}
	return "", atmos
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// boundaryKeywords mark the end of the title portion.
var boundaryKeywords = []string{
	"2160P", "1080P", "1080I", "720P", "576P", "480P", "360P", "4K", "UHD",
	"BLURAY", "BLU-RAY", "BD", "BDMUX", "BDRIP", "BD-RIP", "BRRIP", "MICROHD", "FULLBLURAY", "FULLUHD", "4KFULLUHD", "UHDRIP", "UHD-RIP",
	"HDDVD", "HD-DVD", "WEB-DLRIP", "WEBDLRIP", "WEBRIP", "WEB-RIP", "HDRIP", "HD-RIP", "WEB-DL", "WEBDL", "WEB", "HDTV",
	"PDTV", "DSR", "DSRIP", "TVRIP", "SATRIP", "DTHRIP", "SDTV",
	"DVDRIP", "DVDR", "DVD5", "DVD9", "DVDSCR", "DVD-SCREENER", "BR-SCREENER", "TS-SCREENER", "BDSCR", "WEBSCR", "VHSRIP", "LDRIP", "LASERDISC",
	"REMUX", "BDREMUX", "UHDREMUX", "BRREMUX",
	"X264", "X265", "H264", "H265", "HEVC", "AVC", "AV1", "VP9", "XVID", "DIVX", "MPEG2", "VC1", "VC-1",
	"CAM", "HDCAM", "HDTS", "TELESYNC", "TELECINE", "PROPER", "REPACK", "COMPLETE",
	"SEASON", "SEASONS", "MULTI", "FRENCH", "GERMAN", "SPANISH", "ITALIAN", "HDR", "HDR10",
	"INTERNAL", "LIMITED", "SUBBED", "DUBBED", "NTSC", "PAL",
}

// titleWordKeywords are boundary keywords that are also ordinary title
// words ("The French Dispatch", "Internal Affairs", "Charlottes Web",
// "Cam"); before the release year or episode marker they stay in the title.
var titleWordKeywords = map[string]bool{
	"BD": true, "BDMUX": true, "COMPLETE": true, "SEASON": true, "SEASONS": true, "MULTI": true, "FRENCH": true, "GERMAN": true,
	"SPANISH": true, "ITALIAN": true, "INTERNAL": true, "LIMITED": true,
	"SUBBED": true, "DUBBED": true, "CAM": true, "WEB": true, "PAL": true,
}

func extractTitle(spaced string, yearAt, markerAt, metaAt, floor int, info *core.ReleaseInfo) string {
	cut := len(spaced)
	consider := func(idx int) {
		if idx > 0 && idx < cut {
			cut = idx
		}
	}
	// Year boundary (the occurrence parseYearAt chose), episode/season marker,
	// and earliest post-title technical metadata block.
	consider(yearAt)
	consider(markerAt)
	consider(metaAt)
	for _, kw := range boundaryKeywords {
		// Keywords are whole tokens ("Multiverse" is not MULTI, "Scam" is
		// not CAM), and words before the release year or episode marker
		// belong to the title ("Charlottes Web 2006", "Internal Affairs
		// 1990", "The Spanish Princess S01E01").
		// Only words that also occur in titles get that protection:
		// resolutions, sources, codecs and PROPER/REPACK always end the
		// title ("Show.Name.720p.S01E01").
		if idx := tokenIndex(spaced, kw); idx > 0 && (idx > floor || !titleWordKeywords[kw] || kw == "COMPLETE" && namesPack(spaced[idx+len(kw):])) {
			consider(idx)
		}
	}
	title := spaced[:cut]
	title = strings.Trim(title, " -/")
	// Hyphens between words in titles are usually separators too, except
	// keep words joined: turn "Spider-Man" stays, "Title -" trimmed above.
	fields := strings.Fields(title)
	// Drop a leading bracket-group token, e.g. [HorribleSubs] Title ...
	if info.Group != "" && len(fields) > 1 {
		gf := strings.Fields(spacify(info.Group))
		if len(gf) > 0 && len(fields) > len(gf) && strings.EqualFold(strings.Join(fields[:len(gf)], " "), strings.Join(gf, " ")) {
			fields = fields[len(gf):]
		}
	}
	// Drop a stray leading "-" separator left by "[Group] Title - S01E01".
	for len(fields) > 0 && fields[0] == "-" {
		fields = fields[1:]
	}
	title = strings.Trim(strings.Join(fields, " "), " -/")
	return title
}

// SeriesYearMatches distinguishes same-named series and reboots. A year
// explicitly present in a TV release identifies the series, not the episode's
// air year. Releases omitting a year remain eligible.
func SeriesYearMatches(releaseYear, showYear int) bool {
	return releaseYear == 0 || showYear == 0 || releaseYear == showYear
}
