package nzb

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Kind classifies a posted file by its name.
type Kind int

const (
	KindUnknown  Kind = iota
	KindVideo         // a playable container (.mkv, .mp4, ...)
	KindRAR           // a RAR volume (.rar, .r00, .partNN.rar)
	KindSevenZip      // a 7z volume (.7z, .7z.001)
	KindZIP           // a ZIP volume (.zip, .zip.001, .z01)
	KindSplit         // a plain split volume (.001, .mkv.001)
	KindPar2
	KindNFO
	KindSFV
	KindSubtitle
	KindImage
	KindOther // known but irrelevant (.txt, .nzb, .srr, ...)
	KindISO   // a disc image (.iso), played by its main feature
)

var kindNames = [...]string{"unknown", "video", "rar", "7z", "zip", "split", "par2", "nfo", "sfv", "subtitle", "image", "other", "iso"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// IsArchive reports whether the kind is a volume of a multi-file set.
func (k Kind) IsArchive() bool {
	return k == KindRAR || k == KindSevenZip || k == KindZIP || k == KindSplit
}

// FileInfo is what a file name says about a posted file.
type FileInfo struct {
	Kind Kind
	// Ext is the lower-cased extension of the content: "mkv" for
	// "movie.mkv" and for "movie.mkv.001"; "rar" / "7z" for archives.
	Ext string
	// Volume is the 0-based position of an archive volume in its set
	// (.rar=0, .r00=1, .part01.rar=0, .7z.001=0, .001=0). -1 otherwise.
	Volume int
	// SetName is the lower-cased name shared by every volume of a set, so
	// volumes group by (Kind, SetName). For other files it is the
	// lower-cased name without extension.
	SetName string
	// NewStyle is true for .partNN.rar naming, false for .rar/.rNN.
	NewStyle bool
	// Sample and Extra flag short clips that are not the feature.
	Sample bool
	Extra  bool
}

var videoExts = map[string]bool{
	"mkv": true, "mp4": true, "m4v": true, "avi": true, "ts": true, "m2ts": true,
	"mts": true, "wmv": true, "mov": true, "mpg": true, "mpeg": true, "webm": true,
	"vob": true, "ogm": true, "divx": true, "flv": true, "3gp": true, "asf": true,
}

var subtitleExts = map[string]bool{"srt": true, "sub": true, "idx": true, "ass": true, "ssa": true, "vtt": true, "sup": true}
var imageExts = map[string]bool{"jpg": true, "jpeg": true, "png": true, "gif": true, "bmp": true, "webp": true}
var otherExts = map[string]bool{
	"txt": true, "nzb": true, "srr": true, "img": true, "url": true, "exe": true,
	"md5": true, "sha1": true, "db": true, "log": true, "cue": true, "nfo2": true,
	"lnk": true, "scr": true, "bat": true, "cmd": true, "msi": true, "ps1": true,
	"vbs": true, "hta": true, "pif": true, "epub": true, "mobi": true, "azw3": true,
	"cbz": true, "cbr": true, "pdf": true, "m4b": true, "mp3": true, "flac": true,
	"ogg": true, "opus": true, "wav": true, "m4a": true,
}

// IsVideoExt reports whether ext (without dot, any case) is a video container.
func IsVideoExt(ext string) bool { return videoExts[strings.ToLower(ext)] }

var (
	rePartRAR  = regexp.MustCompile(`(?i)^(.*)\.part(\d{1,4})\.rar$`)
	reOldRAR   = regexp.MustCompile(`(?i)^(.*)\.([r-y])(\d{2})$`)
	reOldRAR3  = regexp.MustCompile(`(?i)^(.*)\.r(\d{3})$`)
	rePlainRAR = regexp.MustCompile(`(?i)^(.*)\.rar$`)
	re7zVol    = regexp.MustCompile(`(?i)^(.*)\.7z\.(\d{1,4})$`)
	re7z       = regexp.MustCompile(`(?i)^(.*)\.7z$`)
	reZipVol   = regexp.MustCompile(`(?i)^(.*)\.zip\.(\d{1,4})$`)
	reZSplit   = regexp.MustCompile(`(?i)^(.*)\.z(\d{2,4})$`)
	reZip      = regexp.MustCompile(`(?i)^(.*)\.zip$`)
	reSplit    = regexp.MustCompile(`(?i)^(.*)\.(\d{3,4})$`)
	rePar2     = regexp.MustCompile(`(?i)^(.*?)(\.vol\d+[+-]\d+)?\.par2$`)
	reSample   = regexp.MustCompile(`(?i)(^|[^a-z0-9])(sample|proof)([^a-z0-9]|$)`)
	reExtra    = regexp.MustCompile(`(?i)(^|[^a-z0-9])(trailer|featurette|extras?|behind[ ._-]the[ ._-]scenes|deleted[ ._-]scenes?|bonus|interview|making[ ._-]of)([^a-z0-9]|$)`)
)

// Classify interprets a file name (a base name or a path inside an archive).
func Classify(name string) FileInfo {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
	if base == "." || base == "/" {
		base = ""
	}
	info := FileInfo{Volume: -1}
	lower := strings.ToLower(base)
	// Sample/extra detection looks at the whole path: "Sample/x.mkv".
	full := strings.ToLower(name)
	info.Sample = reSample.MatchString(full)
	info.Extra = !info.Sample && reExtra.MatchString(full)

	switch {
	case rePar2.MatchString(lower):
		m := rePar2.FindStringSubmatch(lower)
		info.Kind, info.Ext, info.SetName = KindPar2, "par2", m[1]
		return info
	case rePartRAR.MatchString(lower):
		m := rePartRAR.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[2])
		info.Kind, info.Ext, info.SetName, info.NewStyle = KindRAR, "rar", m[1], true
		info.Volume = n - 1 // part01 is the first; fixed up per set if numbering starts at 0
		return info
	case rePlainRAR.MatchString(lower):
		m := rePlainRAR.FindStringSubmatch(lower)
		info.Kind, info.Ext, info.SetName, info.Volume = KindRAR, "rar", m[1], 0
		return info
	case reOldRAR.MatchString(lower):
		m := reOldRAR.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[3])
		letter := int(m[2][0] - 'r')
		info.Kind, info.Ext, info.SetName = KindRAR, "rar", m[1]
		info.Volume = 1 + letter*100 + n
		return info
	case reOldRAR3.MatchString(lower):
		m := reOldRAR3.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[2])
		info.Kind, info.Ext, info.SetName = KindRAR, "rar", m[1]
		info.Volume = 1 + n
		return info
	case re7zVol.MatchString(lower):
		m := re7zVol.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[2])
		info.Kind, info.Ext, info.SetName, info.Volume = KindSevenZip, "7z", m[1], n-1
		return info
	case re7z.MatchString(lower):
		m := re7z.FindStringSubmatch(lower)
		info.Kind, info.Ext, info.SetName, info.Volume = KindSevenZip, "7z", m[1], 0
		return info
	case reZipVol.MatchString(lower):
		m := reZipVol.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[2])
		info.Kind, info.Ext, info.SetName, info.Volume = KindZIP, "zip", m[1], n-1
		return info
	case reZSplit.MatchString(lower):
		m := reZSplit.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[2])
		info.Kind, info.Ext, info.SetName, info.Volume = KindZIP, "zip", m[1], n-1
		info.NewStyle = true
		return info
	case reZip.MatchString(lower):
		m := reZip.FindStringSubmatch(lower)
		info.Kind, info.Ext, info.SetName, info.Volume = KindZIP, "zip", m[1], 0
		return info
	case reSplit.MatchString(lower):
		m := reSplit.FindStringSubmatch(lower)
		n, _ := strconv.Atoi(m[2])
		info.Kind, info.SetName, info.Volume = KindSplit, m[1], n-1
		info.Ext = strings.TrimPrefix(path.Ext(m[1]), ".")
		if _, err := strconv.Atoi(info.Ext); err == nil {
			info.Ext = "" // "movie.2024.001": the year is not an extension
		}
		return info
	}

	ext := strings.TrimPrefix(path.Ext(lower), ".")
	info.Ext = ext
	info.SetName = strings.TrimSuffix(lower, path.Ext(lower))
	switch {
	case ext == "":
		info.Kind = KindUnknown
	case videoExts[ext]:
		info.Kind = KindVideo
	case ext == "iso":
		info.Kind = KindISO
	case ext == "nfo":
		info.Kind = KindNFO
	case ext == "sfv":
		info.Kind = KindSFV
	case subtitleExts[ext]:
		info.Kind = KindSubtitle
	case imageExts[ext]:
		info.Kind = KindImage
	case otherExts[ext]:
		info.Kind = KindOther
	default:
		info.Kind = KindUnknown
	}
	return info
}

var (
	reQuoted         = regexp.MustCompile(`"([^"]*)"`)
	reBracketName    = regexp.MustCompile(`\[\s*(?:\d+/)?([^\[\]"]+\.[A-Za-z0-9]{2,5}(?:\.\d{2,4})?)\s*\]`)
	rePrivateMarker  = regexp.MustCompile(`(?i)\[PRIVATE\]-`)
	rePrivateCounter = regexp.MustCompile(`^\d+(?:/\d+)?$`)
	reLeadingNumPath = regexp.MustCompile(`^\d+/(.+\.[A-Za-z0-9]{2,5}(?:\.\d{2,4})?)$`)
	reFileCounter    = regexp.MustCompile(`[\[(]\s*(\d{1,5})\s*(?:/|of)\s*(\d{1,5})\s*[\])]`)
	reSegCounter     = regexp.MustCompile(`\(\s*(\d{1,6})\s*/\s*(\d{1,6})\s*\)\s*(?:\d+\s*)?$`)
	reTrailingYEnc   = regexp.MustCompile(`(?i)(?:(?:^|\s+)(?:-\s*)?yenc(?:\s+\d+)?(?:\s*[\[(]\s*\d+\s*/\s*\d+\s*[\])])?(?:\s+\d+)?|\s*(?:-\s*)?[\[(]\s*\d+\s*/\s*\d+\s*[\])](?:\s*(?:yenc|\d+))*)\s*$`)
	reSizeNoise      = regexp.MustCompile(`(?i)(?:\s*-\s*|\s+)[\[(]?\d+(?:[.,]\d+)?\s*(?:[kmgt]i?b|bytes?)\b[\])]?(?:\s*-\s*)?$`)
	reReplyPrefix    = regexp.MustCompile(`(?i)^(?:(?:re|fwd|fw)\s*:\s*)+`)
	reLeadingCounter = regexp.MustCompile(`(?i)^(?:[\[(]\s*\d{1,5}\s*(?:/|of)\s*\d{1,5}\s*[\])]|(?:file\s+)?\d{1,5}\s*(?:/|of)\s*\d{1,5})(?:\s*[-:]\s*|\s+)`)
	reMidCounter     = regexp.MustCompile(`(?i)(?:[\[(]\s*\d{1,5}\s*/\s*\d{1,5}\s*[\])](?:\s*[-:]\s*|\s+)|\[\s*\d{1,5}\s+of\s+\d{1,5}\s*\]\s*[-:]\s*)`)
	reUnquotedExt    = regexp.MustCompile(`(?i)\.(?:part\d{1,4}\.rar|vol\d+[+-]\d+\.par2|7z\.\d{1,4}|zip\.\d{1,4}|[A-Za-z0-9]{2,5}\.\d{3,4}|r\d{2,3}|[r-y]\d{2}|z\d{2,4}|\d{3,4}|[A-Za-z0-9]{2,5})(?:\s|$)`)
)

// NameFromSubject extracts the file name from a Usenet subject line, e.g.
//
//	`"Movie.2024.part01.rar" yEnc (1/50)`                -> Movie.2024.part01.rar
//	`[01/42] - "Movie.2024.mkv" yEnc (1/1000)`          -> Movie.2024.mkv
//	`[PRiVATE]-[WtFnZb]-[Movie.2024.mkv]-[1/5] - "" yEnc` -> Movie.2024.mkv
//	`Concert Recording 2019 Full Show.mkv (1/120)`      -> Concert Recording 2019 Full Show.mkv
//	`Movie.2024.r00 (1/50)`                             -> Movie.2024.r00
func NameFromSubject(subject string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		return ""
	}
	// 1. Quoted names: prefer one with a recognized file extension, then any
	// quoted string containing a dot, keeping the first non-empty quote as a
	// fallback for extensionless obfuscated names.
	var firstQuoted, dottedQuoted string
	for _, m := range reQuoted.FindAllStringSubmatch(s, -1) {
		q := strings.TrimSpace(m[1])
		if q == "" {
			continue
		}
		if firstQuoted == "" {
			firstQuoted = q
		}
		if strings.Contains(q, ".") {
			cleaned := cleanName(q)
			if Classify(cleaned).Kind != KindUnknown {
				return cleaned
			}
			if dottedQuoted == "" {
				dottedQuoted = cleaned
			}
		}
	}
	if dottedQuoted != "" {
		return dottedQuoted
	}
	// 2. Bracketed [PRiVATE]-[Group]-... frames (including nested group brackets).
	if name := parsePrivateFrame(s); name != "" {
		return name
	}
	// 3. Unquoted subjects vs. bracketed names: strip trailing yEnc/counter/size
	// noise first so unquoted filenames with spaces and embedded bracket tags
	// (e.g. `Show [H.264].mkv yEnc (1/50)`) aren't mistaken for bracketed names.
	stripped := stripUnquotedTrailing(s)
	unquoted, unquotedKnown := extractUnquotedName(stripped)
	if unquotedKnown && (firstQuoted == "" || !strings.Contains(unquoted, `"`)) {
		return unquoted
	}
	if name := extractBracketName(s); name != "" {
		return name
	}
	if firstQuoted != "" {
		return cleanName(firstQuoted)
	}
	if unquoted != "" {
		return unquoted
	}
	// Fall back to the last whitespace-separated token after prefix stripping.
	if f := strings.Fields(stripUnquotedPrefix(stripped)); len(f) > 0 {
		return cleanName(f[len(f)-1])
	}
	return cleanName(s)
}

func stripUnquotedTrailing(s string) string {
	cur := strings.TrimSpace(s)
	for range 4 {
		next := reTrailingYEnc.ReplaceAllString(cur, "")
		next = reSizeNoise.ReplaceAllString(next, "")
		next = strings.TrimSpace(next)
		if next == cur {
			break
		}
		cur = next
	}
	return cur
}

func stripUnquotedPrefix(s string) string {
	cur := strings.TrimSpace(s)
	cur = strings.TrimSpace(reReplyPrefix.ReplaceAllString(cur, ""))
	for range 3 {
		next := strings.TrimSpace(reLeadingCounter.ReplaceAllString(cur, ""))
		if next == "" || next == cur {
			break
		}
		cur = next
	}
	if locs := reMidCounter.FindAllStringIndex(cur, -1); len(locs) > 0 {
		after := strings.TrimSpace(cur[locs[len(locs)-1][1]:])
		if after != "" && !strings.HasPrefix(after, ".") {
			cur = after
		}
	}
	return cur
}

func extractUnquotedName(stripped string) (string, bool) {
	cand := stripUnquotedPrefix(stripped)
	if cand == "" {
		return "", false
	}
	locs := reUnquotedExt.FindAllStringIndex(cand, -1)
	if len(locs) == 0 {
		return "", false
	}
	var fallback string
	for i := len(locs) - 1; i >= 0; i-- {
		piece := strings.TrimSpace(cand[:locs[i][1]])
		if piece == "" || strings.HasPrefix(piece, ".") {
			continue
		}
		cleaned := cleanName(piece)
		if cleaned == "" || strings.HasPrefix(cleaned, ".") {
			continue
		}
		if fallback == "" {
			fallback = cleaned
		}
		if Classify(cleaned).Kind != KindUnknown {
			return cleaned, true
		}
	}
	return fallback, false
}

func extractBracketName(s string) string {
	ms := reBracketName.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return ""
	}
	var fallback string
	for i := len(ms) - 1; i >= 0; i-- {
		cand := cleanName(ms[i][1])
		if cand == "" {
			continue
		}
		if fallback == "" {
			fallback = cand
		}
		if Classify(cand).Kind != KindUnknown {
			return cand
		}
	}
	return fallback
}

// parsePrivateFrame extracts the filename section from `[PRiVATE]-[Group]-...`
// headers, preserving nested bracket tags inside the filename section (such as
// `[PRiVATE]-[WtFnZb]-[1/5]-[Show_[Group].mkv]`) where a flat `[^\[\]]+` regex fails.
func parsePrivateFrame(s string) string {
	loc := rePrivateMarker.FindStringIndex(s)
	if loc == nil {
		return ""
	}
	rest := s[loc[1]:]
	var sections []string
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, "- \t")
		if len(rest) == 0 || rest[0] != '[' {
			break
		}
		depth := 0
		end := -1
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		sections = append(sections, strings.TrimSpace(rest[1:end]))
		rest = rest[end+1:]
	}
	var fallback string
	for i, sec := range sections {
		if sec == "" || rePrivateCounter.MatchString(sec) {
			continue
		}
		if m := reLeadingNumPath.FindStringSubmatch(sec); m != nil {
			sec = m[1]
		}
		// The first section after [PRiVATE] is the group tag (e.g. [WtFnZb])
		// unless it is the only non-counter section and carries an extension.
		if i == 0 && len(sections) > 1 && Classify(sec).Kind == KindUnknown {
			continue
		}
		if strings.Contains(sec, ".") {
			cleaned := cleanName(sec)
			if Classify(cleaned).Kind != KindUnknown {
				return cleaned
			}
			if fallback == "" {
				fallback = cleaned
			}
		}
	}
	return fallback
}

func cleanName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Trim(n, `"'`)
	n = strings.ReplaceAll(n, `\`, "/")
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimSpace(n)
}

// subjectFileCounter finds the "[03/50]" file counter: the first counter
// that is not the trailing yEnc segment counter.
func subjectFileCounter(subject string) (int, int) {
	segLoc := reSegCounter.FindStringIndex(subject)
	for _, loc := range reFileCounter.FindAllStringSubmatchIndex(subject, -1) {
		if segLoc != nil && loc[0] >= segLoc[0] {
			break
		}
		i, _ := strconv.Atoi(subject[loc[2]:loc[3]])
		n, _ := strconv.Atoi(subject[loc[4]:loc[5]])
		if n > 0 && i <= n {
			return i, n
		}
	}
	return 0, 0
}

func subjectSegmentCount(subject string) int {
	m := reSegCounter.FindStringSubmatch(subject)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[2])
	return n
}

var (
	reHex       = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reAlnumRnd  = regexp.MustCompile(`^[0-9A-Za-z]{16,}$`)
	reUUIDish   = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)
	reBackupObf = regexp.MustCompile(`(?i)^backup_\d+s\d{1,2}-\d{1,2}$`)
)

// IsObfuscated reports whether a name carries no information about the
// content: no extension, a hash/UUID-like base, a long random token, or the
// "abc.xyz" / "b00bs." / "Backup_NNNNNS01-02" placeholders some posters use.
func IsObfuscated(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return true
	}
	info := Classify(n)
	if info.Kind == KindUnknown && info.Ext == "" {
		return true
	}
	baseName := path.Base(strings.ReplaceAll(n, `\`, "/"))
	lower := strings.ToLower(baseName)
	if strings.HasPrefix(lower, "abc.xyz") || strings.HasPrefix(lower, "b00bs.") {
		return true
	}
	stem := info.SetName
	if info.Kind == KindSplit && info.Ext != "" {
		stem = strings.TrimSuffix(stem, "."+info.Ext)
	}
	// SetName is a lower-cased prefix of the base name; recover the case.
	base := stem
	if len(lower) == len(baseName) && len(stem) <= len(baseName) {
		base = baseName[:len(stem)]
	}
	if base == "" {
		return true
	}
	if reHex.MatchString(base) || reUUIDish.MatchString(base) || reBackupObf.MatchString(base) || looksDottedHex(base) {
		return true
	}
	if reAlnumRnd.MatchString(base) && looksRandom(base) {
		return true
	}
	return false
}

// looksDottedHex recognises obfuscated hex stems split with dots, hyphens or
// underscores (e.g. "a1b2.c3d4.e5f6.7890.abcd.ef01" or
// "[Group]-b2.bef89a622e4a23f07b0d3757ad5e8a.a0").
func looksDottedHex(s string) bool {
	hexCount := 0
	allHexOrSep := true
	runLen, runHex := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if isHex {
			hexCount++
			runLen++
			runHex++
		} else if c == '.' {
			runLen++
		} else {
			if c != '-' && c != '_' {
				allHexOrSep = false
			}
			runLen, runHex = 0, 0
		}
		if runLen >= 30 && runHex >= 24 {
			return true
		}
	}
	return allHexOrSep && hexCount >= 16
}

// looksRandom: a separator-free token mixing digits with both letter cases,
// or digits with letters in long runs, typical of generated names.
func looksRandom(s string) bool {
	var upper, lower, digit, transitions int
	prev := 0
	for _, r := range s {
		cls := 0
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
			cls = 1
		case r >= 'a' && r <= 'z':
			lower++
			cls = 2
		case r >= '0' && r <= '9':
			digit++
			cls = 3
		}
		if prev != 0 && cls != prev {
			transitions++
		}
		prev = cls
	}
	kinds := 0
	if upper > 0 {
		kinds++
	}
	if lower > 0 {
		kinds++
	}
	if digit > 0 {
		kinds++
	}
	if kinds == 3 {
		return true
	}
	return kinds == 2 && digit > 0 && transitions >= 4
}
