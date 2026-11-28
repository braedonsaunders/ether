// Package clientreg is the data-driven client capability engine.
// One ordered registry matches every client. Roles share that matcher:
// class (the device kind quality rules group by, for apps that span kinds
// or report no platform; it never changes capabilities), policy (Dolby Vision profiles, subtitles, family), playback (codecs and
// limits for clients that send no profile), patch (narrow hardware facts),
// quirk (a named flag, including a wire-protocol quirk) and segmented-audio
// (audio codecs a client's segmented player must not receive copied, even
// when its streaming target lists them).
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

// Identity is the client a match rule sees. Device is the hardware/model
// identifier (such as X-Plex-Device); DeviceName is the user-visible name
// (such as X-Plex-Device-Name or Jellyfin's Device/X-Emby-Device-Name).
type Identity struct {
	Product, Platform, Device, DeviceName, Model, Version string
}

// Result is one resolved registry record after presets, patches and overrides.
type Result struct {
	ID                  string
	Family              string
	Class               string
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
	// DolbyVisionOff is set when an owner override turned Dolby Vision off.
	DolbyVisionOff bool
	// AlwaysEncodeVideo is set when an owner override disables video copy on
	// transcoded/remuxed streams.
	AlwaysEncodeVideo bool
}

// App describes a client application in the registry catalogue.
// Capabilities are evaluated through Playback and Policy on Identity unless
// the entry lists conform-captured codecs that do not come from a preset.
type App struct {
	Protocol      string   `json:"protocol"`
	Name          string   `json:"name"`
	Product       string   `json:"product,omitempty"`
	Products      []string `json:"products,omitempty"`
	Identity      Identity `json:"identity,omitempty"`
	Devices       []string `json:"devices"`
	VideoCodecs   []string `json:"videoCodecs,omitempty"`
	Subtitles     []string `json:"subtitles,omitempty"`
	DirectPlay    string   `json:"directPlay"`
	Note          string   `json:"note,omitempty"`
	Certification string   `json:"certification"`
}

// AllProducts returns every product string associated with this app row.
func (a App) AllProducts() []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if !slices.ContainsFunc(out, func(existing string) bool { return strings.EqualFold(existing, s) }) {
			out = append(out, s)
		}
	}
	add(a.Product)
	add(a.Name)
	add(a.Identity.Product)
	for _, p := range a.Products {
		add(p)
	}
	return out
}

// Apps returns the registry's app catalogue in display order.
func Apps() []App {
	out := make([]App, len(loaded.Apps))
	for i, item := range loaded.Apps {
		item.Products = slices.Clone(item.Products)
		item.Devices = slices.Clone(item.Devices)
		item.VideoCodecs = slices.Clone(item.VideoCodecs)
		item.Subtitles = slices.Clone(item.Subtitles)
		out[i] = item
	}
	return out
}

// Device describes a representative catalogue identity, not a capability table.
// Evaluate it through Playback and Policy just like a runtime client. Actual
// clients can still declare capabilities or match more specific model rules.
type Device struct {
	// ID is the device type id quality rules store. Empty means Name; it is
	// set only where a row was renamed after rules already saved its id
	// (Apple TV became "Apple TV 4K" when Apple TV HD got its own row).
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name"`
	Identity Identity `json:"identity"`
	// Aliases are other names owners search for ("iPad" for iOS).
	Aliases []string `json:"aliases,omitempty"`
	// Parent is the device type id this one narrows: a rule naming the
	// parent ("Apple TV") also matches it ("Apple TV HD").
	Parent string `json:"parent,omitempty"`
}

// TypeID is the id quality rules use for this device type.
func (d Device) TypeID() string {
	if d.ID != "" {
		return d.ID
	}
	return d.Name
}

// Devices returns the registry's catalogue identities in display order.
func Devices() []Device { return slices.Clone(loaded.Devices) }

// hardwareModel names one hardware model code a client reports
// (X-Plex-Model, or X-Plex-Device on Android TV boxes).
type hardwareModel struct {
	Model string `json:"model"`
	Name  string `json:"name"`
}

// CanonicalModel is the client's hardware model code in the form the
// registry lists it. Plex for Apple TV sends only the numeric part of the
// Apple identifier ("5,3" for AppleTV5,3), which on its own says nothing
// about the vendor, so it is prefixed when the client is an Apple TV.
func CanonicalModel(id Identity) string {
	m := strings.TrimSpace(id.Model)
	if bareAppleModel(m) {
		ident := strings.ToLower(id.Product + " " + id.Platform + " " + id.Device)
		if strings.Contains(ident, "apple tv") || strings.Contains(ident, "appletv") || strings.Contains(ident, "tvos") {
			return "AppleTV" + m
		}
	}
	return m
}

// bareAppleModel reports a "<major>,<minor>" code with no vendor prefix.
func bareAppleModel(m string) bool {
	major, minor, ok := strings.Cut(m, ",")
	if !ok || major == "" || minor == "" {
		return false
	}
	for _, part := range []string{major, minor} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// ModelName is the product name of a reported hardware model code
// ("AppleTV5,3" is "Apple TV HD"), or "" when the code is not known.
// Codes are compared exactly, ignoring case and surrounding space.
func ModelName(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	for _, m := range loaded.Models {
		if strings.EqualFold(m.Model, code) {
			return m.Name
		}
	}
	return ""
}

type document struct {
	Apps        []App             `json:"apps"`
	Devices     []Device          `json:"devices"`
	Models      []hardwareModel   `json:"models"`
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
	// Class is the kind of device a policy or playback record describes:
	// "tv", "mobile" or "computer" (rule device groups).
	Class       string `json:"class"`
	PatchHeight *int   `json:"patchHeight"`
	PatchHDR10  *bool  `json:"patchHdr10"`
	Match       Match  `json:"match"`
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

// MatchesProduct reports whether m matches a client with the given product name.
func (m Match) MatchesProduct(product string) bool {
	return m.ok(Identity{Product: product})
}

// ProductRules returns all positive product-field match leaves across registry definitions.
func ProductRules() []Match {
	var out []Match
	var walk func(m Match)
	walk = func(m Match) {
		if strings.EqualFold(m.Field, "product") {
			op := strings.ToLower(m.Op)
			if !strings.HasPrefix(op, "not") {
				exists := slices.ContainsFunc(out, func(prev Match) bool {
					return strings.EqualFold(prev.Op, m.Op) && strings.EqualFold(prev.Value, m.Value)
				})
				if !exists {
					out = append(out, Match{Field: "product", Op: m.Op, Value: m.Value})
				}
			}
		}
		for _, child := range m.All {
			walk(child)
		}
		for _, child := range m.Any {
			walk(child)
		}
	}
	for i := range loaded.Definitions {
		walk(loaded.Definitions[i].Match)
	}
	return out
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
		return CanonicalModel(id)
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
		Class:               d.Class,
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
	return applyOverrides(base, recordIDs(base.ID, ""), id, overrides)
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
		if d.Video != nil {
			// Hardware limits can remove a codec but must not grant one
			// the app's playback defaults never supported.
			base.Video = slices.DeleteFunc(base.Video, func(codec string) bool {
				return !slices.Contains(d.Video, codec)
			})
		}
		if d.PatchHeight != nil {
			base.MaxHeight = *d.PatchHeight
		}
		if d.PatchHDR10 != nil {
			base.HDR10 = *d.PatchHDR10
		}
	}
	policyID := first("policy", id).ID
	return applyOverrides(base, recordIDs(base.ID, policyID), id, overrides)
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

// SegmentedAudioRefused reports whether codec must not be copied into
// segmented output (HLS or DASH) for id. Every matching segmented-audio
// record applies. Direct play of the original file is not affected.
func SegmentedAudioRefused(id Identity, codec string) bool {
	codec = strings.TrimSpace(codec)
	if codec == "" {
		return false
	}
	for i := range loaded.Definitions {
		d := &loaded.Definitions[i]
		if d.Role != "segmented-audio" || !d.Match.ok(id) {
			continue
		}
		if slices.ContainsFunc(d.Audio, func(c string) bool { return strings.EqualFold(c, codec) }) {
			return true
		}
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
	if o.Product != "" {
		if o.ID == "" && o.Device == "" && o.Platform == "" {
			if !strings.EqualFold(strings.TrimSpace(id.Product), strings.TrimSpace(o.Product)) {
				return false
			}
		} else if !strings.Contains(strings.ToLower(id.Product), strings.ToLower(o.Product)) {
			return false
		}
	}
	// Device and platform selectors intentionally support partial names: the
	// device editor accepts model fragments and an optional app-name filter.
	// Device matches either the device model/header (e.g. X-Plex-Device) or
	// the user-visible device name (e.g. X-Plex-Device-Name, Jellyfin Device).
	if o.Platform != "" && !strings.Contains(strings.ToLower(id.Platform), strings.ToLower(o.Platform)) {
		return false
	}
	if o.Device != "" {
		want := strings.ToLower(o.Device)
		if !strings.Contains(strings.ToLower(id.Device), want) &&
			!strings.Contains(strings.ToLower(id.DeviceName), want) {
			return false
		}
	}
	if o.Version != "" && semverCompare(id.Version, o.Version) < 0 {
		return false
	}
	return true
}

func recordIDs(playbackID, policyID string) []string {
	if policyID == "" || policyID == playbackID {
		return []string{playbackID}
	}
	return []string{playbackID, policyID}
}

func matchingOverrides(ids []string, id Identity, overrides []core.ClientCapabilityOverride) []int {
	var out []int
	for i, o := range overrides {
		if OverrideApplies(o, ids, id) {
			out = append(out, i)
		}
	}
	return out
}

// MatchingOverrides returns the indices of overrides that apply to id at
// runtime, in application order. Both runtime override application and the
// console's seen-devices indicator use this matcher so they cannot disagree.
func MatchingOverrides(id Identity, overrides []core.ClientCapabilityOverride) []int {
	if len(overrides) == 0 {
		return nil
	}
	return matchingOverrides(recordIDs(first("playback", id).ID, first("policy", id).ID), id, overrides)
}

func applyOverrides(base Result, ids []string, id Identity, overrides []core.ClientCapabilityOverride) Result {
	for _, i := range matchingOverrides(ids, id, overrides) {
		o := overrides[i]
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
			base.DolbyVisionOff = !*o.DolbyVision
		}
		if o.AlwaysEncodeVideo {
			base.AlwaysEncodeVideo = true
		}
		if o.Notes != "" {
			base.Notes = o.Notes
		}
	}
	return base
}
