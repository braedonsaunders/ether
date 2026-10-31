package resolver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// The Starr apps whose TRaSH Guides data Ether imports.
const (
	AppRadarr = "radarr"
	AppSonarr = "sonarr"
)

// QualityImport is a TRaSH quality profile translated into Ether's terms. It
// keeps the profile's format references (formatItems) so the referenced
// custom formats can be resolved before the profile is saved.
type QualityImport struct {
	Profile   core.QualityProfile `json:"profile"`
	FormatIDs map[string]string   `json:"formatIds"`
	// TrashID is the profile's TRaSH ID, which its default format groups
	// are keyed by.
	TrashID string `json:"trashId,omitempty"`
	// ScoreSet names the trash_scores entry the profile's formats are scored
	// with ("sqp-1-2160p"); empty means each format's default score.
	ScoreSet string `json:"scoreSet,omitempty"`
	// App is the Starr app the quality names identify, or "" when they
	// read the same in Radarr and Sonarr.
	App string `json:"app,omitempty"`
	// Skipped lists enabled qualities Ether has no equivalent for.
	Skipped []string `json:"skipped,omitempty"`
	// Warnings explain profile settings that do not carry over.
	Warnings []string `json:"warnings,omitempty"`
}

// ImportQualityProfile translates native TRaSH quality groups into exact
// source/resolution gates. Download upgrade/cutoff fields have no streaming
// equivalent; source ordering and minimum format score do. An enabled quality
// Ether cannot match (Raw-HD, SDTV) is skipped and reported, unless nothing
// playable would remain.
func ImportQualityProfile(data []byte) (QualityImport, error) {
	var native struct {
		Name                  string            `json:"name"`
		TrashID               string            `json:"trash_id"`
		ScoreSet              string            `json:"trash_score_set"`
		UpgradeAllowed        bool              `json:"upgradeAllowed"`
		Cutoff                json.RawMessage   `json:"cutoff"`
		MinFormatScore        int               `json:"minFormatScore"`
		CutoffFormatScore     int               `json:"cutoffFormatScore"`
		MinUpgradeFormatScore int               `json:"minUpgradeFormatScore"`
		Language              json.RawMessage   `json:"language"`
		Items                 []json.RawMessage `json:"items"`
		FormatItems           json.RawMessage   `json:"formatItems"`
	}
	if len(data) > 8<<20 {
		return QualityImport{}, fmt.Errorf("quality profile: too large")
	}
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		return QualityImport{}, fmt.Errorf("quality profile: expected one profile object, not a list")
	}
	if err := json.Unmarshal(data, &native); err != nil {
		return QualityImport{}, fmt.Errorf("quality profile: not valid profile JSON: %w", err)
	}
	native.Name = strings.TrimSpace(native.Name)
	if native.Name == "" || len(native.Items) == 0 {
		return QualityImport{}, fmt.Errorf("quality profile: name and items are required")
	}
	var formatIDs map[string]string
	if f := bytes.TrimSpace(native.FormatItems); len(f) > 0 && !bytes.Equal(f, []byte("null")) {
		if f[0] == '[' {
			// Radarr and Sonarr's own API exports list formats by local id.
			return QualityImport{}, fmt.Errorf("quality profile: this looks like a Radarr or Sonarr API export; use a TRaSH Guides profile instead")
		}
		if err := json.Unmarshal(f, &formatIDs); err != nil {
			return QualityImport{}, fmt.Errorf("quality profile: formatItems must map format names to trash IDs")
		}
	}
	if len(formatIDs) > 1000 {
		return QualityImport{}, fmt.Errorf("quality profile: too many custom formats")
	}
	imp := QualityImport{FormatIDs: formatIDs, TrashID: strings.TrimSpace(native.TrashID), ScoreSet: strings.TrimSpace(native.ScoreSet)}
	p := core.QualityProfile{
		Name:                 native.Name,
		PreferredCodecs:      []string{"h264", "hevc"},
		PreferredAudio:       []string{"eac3", "ac3", "aac"},
		MinSizeMBPerMin:      5, // the same fake filter as a new profile; scaled down for SD
		MinCustomFormatScore: &native.MinFormatScore,
	}
	seenQuality := map[string]bool{}
	seenSource := map[core.Source]bool{}
	skipped := map[string]bool{}
	add := func(name string) (core.Source, bool) {
		q, source, height, ok := parseImportedQuality(name)
		if !ok {
			if !skipped[name] {
				skipped[name] = true
				imp.Skipped = append(imp.Skipped, name)
			}
			return "", false
		}
		if strings.HasPrefix(name, "Remux-") {
			imp.App = AppRadarr
		} else if strings.HasSuffix(name, " Remux") {
			imp.App = AppSonarr
		}
		if !seenQuality[q] {
			seenQuality[q] = true
			p.AllowedQualities = append(p.AllowedQualities, q)
		}
		p.MaxHeight = max(p.MaxHeight, height)
		if p.MinHeight == 0 || height < p.MinHeight {
			p.MinHeight = height
		}
		return source, true
	}
	// Starr ranks the top item highest; qualities grouped together rank
	// equally, so a group's sources follow Ether's own order.
	for _, raw := range native.Items {
		var item struct {
			Name    string            `json:"name"`
			Allowed bool              `json:"allowed"`
			Items   []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &item) != nil || !item.Allowed {
			continue
		}
		names := []string{item.Name}
		if len(item.Items) > 0 {
			names = names[:0]
			for _, child := range item.Items {
				var name string
				if json.Unmarshal(child, &name) != nil {
					var obj struct {
						Name string `json:"name"`
					}
					if json.Unmarshal(child, &obj) != nil {
						continue
					}
					name = obj.Name
				}
				names = append(names, name)
			}
		}
		var group []core.Source
		for _, name := range names {
			if source, ok := add(name); ok && !seenSource[source] && !slices.Contains(group, source) {
				group = append(group, source)
			}
		}
		slices.SortStableFunc(group, func(a, b core.Source) int { return sourceRank(a) - sourceRank(b) })
		for _, s := range group {
			seenSource[s] = true
			p.PreferredSources = append(p.PreferredSources, s)
		}
	}
	if len(p.AllowedQualities) == 0 {
		if len(imp.Skipped) > 0 {
			return QualityImport{}, fmt.Errorf("quality profile: none of its enabled qualities can be streamed (%s)", strings.Join(imp.Skipped, ", "))
		}
		return QualityImport{}, fmt.Errorf("quality profile: no playable qualities enabled")
	}
	if p.MaxHeight >= 2160 {
		// UHD profiles exist for HDR. Both still need an app that supports
		// them, and Dolby Vision without an HDR10 base stays off.
		p.AllowHDR, p.AllowDolbyVision = true, true
		p.PreferredCodecs = []string{"hevc", "h264"}
	}
	if len(imp.Skipped) > 0 {
		imp.Warnings = append(imp.Warnings, fmt.Sprintf("Left out %s: Ether has no equivalent for %s.",
			strings.Join(imp.Skipped, ", "), plural(len(imp.Skipped), "this quality", "these qualities")))
	}
	if native.UpgradeAllowed || len(bytes.TrimSpace(native.Cutoff)) > 0 || native.CutoffFormatScore != 0 || native.MinUpgradeFormatScore != 0 {
		imp.Warnings = append(imp.Warnings, "Upgrade and cutoff settings are left out: Ether picks the best release each time a title plays, so there is nothing to upgrade.")
	}
	var language string
	if json.Unmarshal(native.Language, &language) == nil {
		imp.App = cmpOr(imp.App, AppRadarr) // only Radarr profiles carry a language
		switch name := strings.ToLower(strings.TrimSpace(language)); name {
		case "", "any", "unknown":
		case "original":
			imp.Warnings = append(imp.Warnings, "Radarr limits this profile to each title's original language. Ether doesn't filter by language; it passes over dubs that don't match the original language, and the profile's language formats still score releases.")
		default:
			if knownLanguageName(name) {
				p.PreferredLanguages = []string{name}
				imp.Warnings = append(imp.Warnings, fmt.Sprintf("The %s language requirement became a preference: %s releases rank first, but others can still play.", language, language))
			} else {
				imp.Warnings = append(imp.Warnings, fmt.Sprintf("The %s language requirement is left out: Ether doesn't recognise that language.", language))
			}
		}
	}
	imp.Profile = p
	return imp, nil
}

// parseImportedQuality maps a Starr quality name ("WEBDL-1080p", Radarr's
// "Remux-2160p", Sonarr's "Bluray-1080p Remux", "DVD") to Ether's
// source-resolution gate. ok is false for qualities with no equivalent.
func parseImportedQuality(name string) (q string, source core.Source, height int, ok bool) {
	sourceName, resolution, _ := strings.Cut(strings.TrimSpace(name), "-")
	// Sonarr names remuxes "Bluray-1080p Remux" (Radarr: "Remux-1080p").
	if r, ok := strings.CutSuffix(strings.ToLower(resolution), " remux"); ok {
		sourceName, resolution = "remux", r
	}
	switch strings.ToLower(sourceName) {
	case "webdl":
		source = core.SourceWEBDL
	case "webrip":
		source = core.SourceWEBRip
	case "bluray":
		source = core.SourceBluRay
	case "remux":
		source = core.SourceRemux
	case "hdtv":
		source = core.SourceHDTV
	case "dvd":
		source = core.SourceDVD
	default:
		return "", "", 0, false
	}
	height = 480
	if resolution != "" {
		var err error
		height, err = strconv.Atoi(strings.TrimSuffix(strings.ToLower(resolution), "p"))
		if err != nil || height <= 0 || height > 4320 {
			return "", "", 0, false
		}
	}
	return fmt.Sprintf("%s-%d", source, height), source, height, true
}

// sourceRank orders equally ranked sources best first.
func sourceRank(s core.Source) int {
	return slices.Index([]core.Source{core.SourceRemux, core.SourceBluRay, core.SourceWEBDL, core.SourceWEBRip, core.SourceHDTV, core.SourceDVD}, s)
}

func releaseQuality(info core.ReleaseInfo) string {
	height := info.Height
	if height == 0 && info.Source == core.SourceDVD {
		height = 480 // a DVD rip names no resolution; Starr's DVD quality is 480
	}
	return fmt.Sprintf("%s-%d", info.Source, height)
}

func knownLanguageName(name string) bool {
	for _, n := range starrLanguages {
		if n == name {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func cmpOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// Import format statuses.
const (
	FormatPresent     = "present"     // already in the owner's custom formats
	FormatAdd         = "add"         // added by the import
	FormatUnsupported = "unsupported" // uses a rule Ether can't evaluate
	FormatMissing     = "missing"     // not found in TRaSH Guides
)

// ImportFormat is one custom format an imported profile references.
type ImportFormat struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Score  int    `json:"score"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// QualityImportPlan previews an import: the profile as it will be saved, the
// custom formats it references and what carries over.
type QualityImportPlan struct {
	App      string              `json:"app"`
	Profile  core.QualityProfile `json:"profile"`
	ScoreSet string              `json:"scoreSet,omitempty"`
	Formats  []ImportFormat      `json:"formats"`
	Skipped  []string            `json:"skippedQualities"`
	Warnings []string            `json:"warnings"`
	// add holds the definitions the import merges into the owner's formats.
	add []core.CustomFormat
}

var (
	// ErrProfileExists rejects an import whose name another profile uses.
	ErrProfileExists = errors.New("a profile with that name already exists")
	// ErrImportRejected wraps configuration validation failures on import.
	ErrImportRejected = errors.New("the import would make the configuration invalid")
)

// formatID is the id the scorer keys a format by.
func formatID(f core.CustomFormat) string {
	if f.ID != "" {
		return f.ID
	}
	return f.Name
}

// formatScore is a format's score in a TRaSH score set, else its default.
func formatScore(f core.CustomFormat, set string) int {
	if v, ok := f.Scores[set]; ok && set != "" {
		return v
	}
	return f.Scores["default"]
}

// PlanQualityImport resolves imp's custom formats against the owner's
// current ones (cfg) and the TRaSH definitions found (by trash ID), and
// names the profile so it doesn't clash. Definitions the scorer cannot
// compile are reported as unsupported and left out, so an import can never
// make the scoring configuration invalid.
func PlanQualityImport(imp QualityImport, app string, found map[string]core.CustomFormat, cfg config.Config) QualityImportPlan {
	plan := QualityImportPlan{App: app, Profile: imp.Profile, ScoreSet: imp.ScoreSet, Skipped: imp.Skipped,
		Formats: []ImportFormat{}, Warnings: append([]string{}, imp.Warnings...)}
	if plan.Skipped == nil {
		plan.Skipped = []string{}
	}
	existing := map[string]core.CustomFormat{}
	for _, f := range cfg.Resolver.CustomFormats {
		existing[formatID(f)] = f
	}
	scores := map[string]int{}
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(imp.FormatIDs)) {
		id := strings.TrimSpace(imp.FormatIDs[name])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		f := ImportFormat{ID: id, Name: name}
		def, fetched := found[id]
		if have, ok := existing[id]; ok {
			f.Status = FormatPresent
			if !fetched {
				def = have
			}
			f.Name = cmpOr(have.Name, name)
		} else if fetched {
			def.ID = id
			if app == AppSonarr {
				def.App = AppSonarr
			}
			if _, err := NewFormatScorer([]core.CustomFormat{def}, nil); err != nil {
				f.Status, f.Reason = FormatUnsupported, unsupportedReason(def, err)
			} else {
				f.Status = FormatAdd
				plan.add = append(plan.add, def)
			}
			f.Name = cmpOr(def.Name, name)
		} else {
			f.Status, f.Reason = FormatMissing, "Not found in TRaSH Guides"
		}
		if f.Status == FormatPresent || f.Status == FormatAdd || fetched {
			f.Score = formatScore(def, imp.ScoreSet)
		}
		if f.Status == FormatPresent || f.Status == FormatAdd {
			scores[id] = f.Score
		}
		plan.Formats = append(plan.Formats, f)
	}
	if len(existing)+len(plan.add) > 1000 {
		plan.Warnings = append(plan.Warnings, "Custom scoring is limited to 1,000 formats; remove some from a quality profile's More options before importing this profile.")
	}
	slices.SortStableFunc(plan.Formats, func(a, b ImportFormat) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	var lost int
	for _, f := range plan.Formats {
		if f.Status == FormatUnsupported || f.Status == FormatMissing {
			lost++
		}
	}
	if lost > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d custom %s can't be imported and won't score releases for this profile.", lost, plural(lost, "format", "formats")))
	}
	if other := len(existing) - countPresent(plan.Formats); other > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d custom %s already saved %s part of this profile and won't change its ranking.", other, plural(other, "format", "formats"), plural(other, "isn't", "aren't")))
	}
	if len(scores) > 0 {
		plan.Profile.CustomFormatScores = scores
	}
	plan.Profile.Name = uniqueProfileName(plan.Profile.Name, cfg.Quality.Profiles)
	return plan
}

func countPresent(fs []ImportFormat) int {
	n := 0
	for _, f := range fs {
		if f.Status == FormatPresent {
			n++
		}
	}
	return n
}

// unsupportedReason explains, for the owner, why the scorer rejected def.
func unsupportedReason(def core.CustomFormat, err error) string {
	for _, s := range def.Specifications {
		switch s.Implementation {
		case "IndexerFlagSpecification":
			return "Uses indexer flags, which Ether doesn't read"
		case "ReleaseTypeSpecification":
			return "Uses Sonarr's release type, which Ether doesn't match"
		}
	}
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, "\": "); ok {
		msg = after
	}
	if msg == "" {
		return "Uses a rule Ether can't evaluate"
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// uniqueProfileName suffixes name ("Name (2)") until no profile uses it.
func uniqueProfileName(name string, profiles []core.QualityProfile) string {
	taken := func(n string) bool {
		return slices.ContainsFunc(profiles, func(p core.QualityProfile) bool { return p.Name == n })
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s (%d)", name, i); !taken(n) {
			return n
		}
	}
}

// Apply adds the planned profile to c under name (the planned name when
// empty) and merges the formats it needs into c's custom formats, keeping
// every existing definition. The new profile scores only its own formats,
// as in Radarr and Sonarr, and every other profile scores the added formats
// 0: importing a profile never changes how the others rank releases. Apply
// only mutates c, so it can be retried against fresher settings.
func (p QualityImportPlan) Apply(c *config.Config, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		name = p.Profile.Name
	}
	if len(name) > 200 {
		return fmt.Errorf("%w: the profile name is too long", ErrImportRejected)
	}
	if slices.ContainsFunc(c.Quality.Profiles, func(q core.QualityProfile) bool { return q.Name == name }) {
		return ErrProfileExists
	}
	have := map[string]bool{}
	for _, f := range c.Resolver.CustomFormats {
		have[formatID(f)] = true
	}
	prof := p.Profile
	prof.Name = name
	prof.AllowedQualities = slices.Clone(prof.AllowedQualities)
	prof.PreferredSources = slices.Clone(prof.PreferredSources)
	prof.PreferredCodecs = slices.Clone(prof.PreferredCodecs)
	prof.PreferredAudio = slices.Clone(prof.PreferredAudio)
	prof.PreferredLanguages = slices.Clone(prof.PreferredLanguages)
	if prof.MinCustomFormatScore != nil {
		v := *prof.MinCustomFormatScore
		prof.MinCustomFormatScore = &v
	}
	scores := maps.Clone(prof.CustomFormatScores)
	if scores == nil {
		scores = map[string]int{}
	}
	for id := range have {
		if _, ok := scores[id]; !ok {
			scores[id] = 0
		}
	}
	var added []string
	for _, f := range p.add {
		if have[f.ID] {
			continue
		}
		have[f.ID] = true
		added = append(added, f.ID)
		c.Resolver.CustomFormats = append(c.Resolver.CustomFormats, f)
	}
	for i := range c.Quality.Profiles {
		other := &c.Quality.Profiles[i]
		for _, id := range added {
			if _, ok := other.CustomFormatScores[id]; ok {
				continue
			}
			if other.CustomFormatScores == nil {
				other.CustomFormatScores = map[string]int{}
			}
			other.CustomFormatScores[id] = 0
		}
	}
	if len(scores) > 0 {
		prof.CustomFormatScores = scores
	} else {
		prof.CustomFormatScores = nil
	}
	c.Quality.Profiles = append(c.Quality.Profiles, prof)
	return nil
}

// Added counts the custom formats the import adds.
func (p QualityImportPlan) Added() int { return len(p.add) }
