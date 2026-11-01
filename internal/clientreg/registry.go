// Package clientreg is the data-driven client capability engine.
// One ordered registry matches every client. Roles share that matcher:
// policy (Dolby Vision profiles, subtitles, family), playback (codecs and
// limits for clients that send no profile), patch (narrow hardware facts)
// and quirk (a named flag, including a wire-protocol quirk).
// Owner overrides apply to the matched policy or playback id. Runtime
// declarations are applied by callers after this package returns.
package clientreg

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

//go:embed registry.json
var registryJSON []byte

// Version is the embedded registry generation.
func Version() int { return loaded.Version }

// Identity is the client a match rule sees.
type Identity struct {
	Product, Platform, Device, Model, Version string
}

// Result is one resolved registry record after presets, patches and overrides.
type Result struct {
	ID                  string
	Family              string
	Notes               string
	Video               []string
	Audio               []string
	Containers          []string
	Subtitles           []string
	DolbyVisionProfiles []int
	MaxHeight           int
	MaxBitrateKbps      int
	HDR10               bool
	DolbyVision         bool
	// HeightLocked is set when an owner override chose the height.
	HeightLocked bool
}

type document struct {
	Version     int               `json:"version"`
	Presets     map[string]preset `json:"presets"`
	Definitions []definition      `json:"definitions"`
}

type preset struct {
	Family              string   `json:"family"`
	Notes               string   `json:"notes"`
	Video               []string `json:"video"`
	Audio               []string `json:"audio"`
	Containers          []string `json:"containers"`
	Subtitles           []string `json:"subtitles"`
	DolbyVisionProfiles []int    `json:"dolbyVisionProfiles"`
	MaxHeight           int      `json:"maxHeight"`
	HDR10               bool     `json:"hdr10"`
	DolbyVision         bool     `json:"dolbyVision"`
}

type definition struct {
	ID                  string   `json:"id"`
	Role                string   `json:"role"`
	Family              string   `json:"family"`
	Notes               string   `json:"notes"`
	Preset              string   `json:"preset"`
	Video               []string `json:"video"`
	Audio               []string `json:"audio"`
	Containers          []string `json:"containers"`
	Subtitles           []string `json:"subtitles"`
	DolbyVisionProfiles []int    `json:"dolbyVisionProfiles"`
	MaxHeight           int      `json:"maxHeight"`
	HDR10               *bool    `json:"hdr10"`
	DolbyVision         *bool    `json:"dolbyVision"`
	Quirks              []string `json:"quirks"`
	PatchHeight         *int     `json:"patchHeight"`
	PatchHDR10          *bool    `json:"patchHdr10"`
	Match               Match    `json:"match"`
}

// Match is one rule. A leaf compares Field. All, Any and None combine rules.
// An empty match matches every client and is the registry's fallback.
type Match struct {
	All   []Match `json:"all,omitempty"`
	Any   []Match `json:"any,omitempty"`
	None  []Match `json:"none,omitempty"`
	Field string  `json:"field,omitempty"`
	Op    string  `json:"op,omitempty"`
	Value string  `json:"value,omitempty"`
}

var loaded = mustLoad()

func mustLoad() document {
	var doc document
	if err := json.Unmarshal(registryJSON, &doc); err != nil {
		panic("clientreg: " + err.Error())
	}
	if doc.Version < 1 {
		panic("clientreg: registry version missing")
	}
	return doc
}

// Matches reports whether m matches id.
func Matches(m Match, id Identity) bool { return m.ok(id) }

func (m Match) ok(id Identity) bool {
	if m.Field != "" {
		return leaf(m, id)
	}
	if len(m.All) == 0 && len(m.Any) == 0 && len(m.None) == 0 {
		return true
	}
	for _, child := range m.All {
		if !child.ok(id) {
			return false
		}
	}
	if len(m.Any) > 0 {
		hit := false
		for _, child := range m.Any {
			if child.ok(id) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, child := range m.None {
		if child.ok(id) {
			return false
		}
	}
	return true
}

func leaf(m Match, id Identity) bool {
	have := fieldValue(id, m.Field)
	switch strings.ToLower(m.Op) {
	case "contains":
		return strings.Contains(strings.ToLower(have), strings.ToLower(m.Value))
	case "notcontains":
		return !strings.Contains(strings.ToLower(have), strings.ToLower(m.Value))
	case "equals":
		return strings.EqualFold(strings.TrimSpace(have), strings.TrimSpace(m.Value))
	case "prefix":
		return strings.HasPrefix(strings.ToLower(have), strings.ToLower(m.Value))
	case "semveratleast":
		return semverCompare(have, m.Value) >= 0
	case "semverbelow":
		return semverCompare(have, m.Value) < 0
	default:
		return false
	}
}

func fieldValue(id Identity, name string) string {
	switch strings.ToLower(name) {
	case "product":
		return id.Product
	case "platform":
		return id.Platform
	case "device":
		return id.Device
	case "model":
		return id.Model
	case "version":
		return id.Version
	case "identity":
		return id.Product + " " + id.Platform + " " + id.Device
	default:
		return ""
	}
}

func semverCompare(have, want string) int {
	a, b := semverParts(have), semverParts(want)
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func semverParts(s string) []int {
	var out []int
	cur := -1
	flush := func() {
		if cur >= 0 {
			out = append(out, cur)
			cur = -1
		}
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if cur < 0 {
				cur = 0
			}
			cur = cur*10 + int(r-'0')
		case r == '.':
			flush()
		default:
			flush()
			return out
		}
	}
	flush()
	return out
}

func (d definition) result() Result {
	var base preset
	if d.Preset != "" {
		base = loaded.Presets[d.Preset]
	}
	out := Result{
		ID:                  d.ID,
		Family:              base.Family,
		Notes:               base.Notes,
		Video:               slices.Clone(base.Video),
		Audio:               slices.Clone(base.Audio),
		Containers:          slices.Clone(base.Containers),
		Subtitles:           slices.Clone(base.Subtitles),
		DolbyVisionProfiles: slices.Clone(base.DolbyVisionProfiles),
		MaxHeight:           base.MaxHeight,
		HDR10:               base.HDR10,
		DolbyVision:         base.DolbyVision,
	}
	if d.Family != "" {
		out.Family = d.Family
	}
	if d.Notes != "" {
		out.Notes = d.Notes
	}
	if d.Video != nil {
		out.Video = slices.Clone(d.Video)
	}
	if d.Audio != nil {
		out.Audio = slices.Clone(d.Audio)
	}
	if d.Containers != nil {
		out.Containers = slices.Clone(d.Containers)
	}
	if d.Subtitles != nil {
		out.Subtitles = slices.Clone(d.Subtitles)
	}
	if d.DolbyVisionProfiles != nil {
		out.DolbyVisionProfiles = slices.Clone(d.DolbyVisionProfiles)
	}
	if d.MaxHeight != 0 {
		out.MaxHeight = d.MaxHeight
	}
	if d.HDR10 != nil {
		out.HDR10 = *d.HDR10
	}
	if d.DolbyVision != nil {
		out.DolbyVision = *d.DolbyVision
	}
	return out
}

func first(role string, id Identity) Result {
	for i := range loaded.Definitions {
		d := &loaded.Definitions[i]
		if d.Role != role || !d.Match.ok(id) {
			continue
		}
		return d.result()
	}
	return Result{ID: "generic", Family: "generic"}
}

// Policy is the family, subtitle set and Dolby Vision profiles for id,
// with matching owner overrides applied.
func Policy(id Identity, overrides []core.ClientCapabilityOverride) Result {
	base := first("policy", id)
	return applyOverrides(base, base.ID, "", id, overrides)
}

// Playback is the codec and limit set for a client that did not send its
// own profile, plus hardware patches and matching owner overrides.
// policyID is the policy record for the same client so a device override
// edits both layers.
func Playback(id Identity, overrides []core.ClientCapabilityOverride) Result {
	base := first("playback", id)
	for i := range loaded.Definitions {
		d := &loaded.Definitions[i]
		if d.Role != "patch" || !d.Match.ok(id) {
			continue
		}
		if d.PatchHeight != nil {
			base.MaxHeight = *d.PatchHeight
		}
		if d.PatchHDR10 != nil {
			base.HDR10 = *d.PatchHDR10
		}
	}
	policyID := first("policy", id).ID
	return applyOverrides(base, base.ID, policyID, id, overrides)
}

// HasQuirk reports whether a named quirk matches id.
func HasQuirk(id Identity, name string) bool {
	for i := range loaded.Definitions {
		d := &loaded.Definitions[i]
		if d.Role != "quirk" || !slices.Contains(d.Quirks, name) || !d.Match.ok(id) {
			continue
		}
		return true
	}
	return false
}

// OverrideApplies reports whether o edits the record ids for this client.
// An id matches the playback record or the policy record. Product, platform,
// device and version narrow it further. An override with no id and no
// matcher matches nothing.
func OverrideApplies(o core.ClientCapabilityOverride, recordIDs []string, id Identity) bool {
	if o.ID != "" && !slices.Contains(recordIDs, o.ID) {
		return false
	}
	if o.ID == "" && o.Product == "" && o.Platform == "" && o.Device == "" && o.Version == "" {
		return false
	}
	if o.Product != "" && !strings.Contains(strings.ToLower(id.Product), strings.ToLower(o.Product)) {
		return false
	}
	if o.Platform != "" && !strings.Contains(strings.ToLower(id.Platform), strings.ToLower(o.Platform)) {
		return false
	}
	if o.Device != "" && !strings.Contains(strings.ToLower(id.Device), strings.ToLower(o.Device)) {
		return false
	}
	if o.Version != "" && semverCompare(id.Version, o.Version) < 0 {
		return false
	}
	return true
}

func applyOverrides(base Result, playbackID, policyID string, id Identity, overrides []core.ClientCapabilityOverride) Result {
	ids := []string{playbackID}
	if policyID != "" && policyID != playbackID {
		ids = append(ids, policyID)
	}
	for _, o := range overrides {
		if !OverrideApplies(o, ids, id) {
			continue
		}
		if o.VideoCodecs != nil {
			base.Video = slices.Clone(o.VideoCodecs)
		}
		if o.AudioCodecs != nil {
			base.Audio = slices.Clone(o.AudioCodecs)
		}
		if o.Containers != nil {
			base.Containers = slices.Clone(o.Containers)
		}
		if o.SubtitleCodecs != nil {
			base.Subtitles = slices.Clone(o.SubtitleCodecs)
		}
		if o.DolbyVisionProfiles != nil {
			base.DolbyVisionProfiles = slices.Clone(o.DolbyVisionProfiles)
		}
		if o.MaxHeight > 0 {
			base.MaxHeight = o.MaxHeight
			base.HeightLocked = true
		}
		if o.MaxBitrateKbps > 0 {
			base.MaxBitrateKbps = o.MaxBitrateKbps
		}
		if o.HDR10 != nil {
			base.HDR10 = *o.HDR10
		}
		if o.DolbyVision != nil {
			base.DolbyVision = *o.DolbyVision
		}
		if o.Notes != "" {
			base.Notes = o.Notes
		}
	}
	return base
}

// Definitions returns the embedded records for the owner console.
func Definitions(role string) []Result {
	var out []Result
	seen := map[string]bool{}
	for i := range loaded.Definitions {
		d := &loaded.Definitions[i]
		if role != "" && d.Role != role {
			continue
		}
		if d.Role == "patch" || d.Role == "quirk" {
			continue
		}
		if seen[d.Role+"\x00"+d.ID] {
			continue
		}
		seen[d.Role+"\x00"+d.ID] = true
		out = append(out, d.result())
	}
	return out
}
