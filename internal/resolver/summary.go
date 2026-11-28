package resolver

import (
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// SummaryLibrary describes one library for symbolic rule evaluation in
// GET /api/quality/summary.
type SummaryLibrary struct {
	ID             int64
	Name           string
	MediaType      core.MediaType // core.TypeMovie, core.TypeShow, or "" (mixed/plugin)
	Plugin         string
	ChosenByPlugin bool
}

// SummaryDevice is a parsed device filter for GET /api/quality/summary.
type SummaryDevice struct {
	Field string // "" | "group" | "type" | "app" | "client" | "contains"
	Value string
	// Optional resolved facts when Field is "client" (from seen devices).
	ClientGroup string
	ClientType  string
	ClientApp   string
	ClientCaps  core.ClientCaps
}

// QualitySummary is the response body of GET /api/quality/summary.
type QualitySummary struct {
	Rows []QualitySummaryRow `json:"rows"`
}

// QualitySummaryRow is one library's row in the summary table.
type QualitySummaryRow struct {
	LibraryID      int64                     `json:"libraryId"`
	Library        string                    `json:"library"`
	Plugin         string                    `json:"plugin"`
	ChosenByPlugin bool                      `json:"chosenByPlugin"`
	Cells          []QualitySummaryKindCells `json:"cells"`
	Exceptions     []QualitySummaryException `json:"exceptions"`
}

// QualitySummaryKindCells pairs the At home and Away cells for one kind
// ("" when a library doesn't split, or "movie" / "episode").
type QualitySummaryKindCells struct {
	Kind string             `json:"kind"`
	Home QualitySummaryCell `json:"home"`
	Away QualitySummaryCell `json:"away"`
}

// QualitySummaryCell is the symbolic evaluation result for one library × where × kind.
type QualitySummaryCell struct {
	Profile        string        `json:"profile"`
	Source         ProfileSource `json:"source"`
	ProfileRuleID  string        `json:"profileRuleId,omitempty"`
	MaxHeight      int           `json:"maxHeight"`
	MaxBitrateKbps int           `json:"maxBitrateKbps"`
	Stereo         bool          `json:"stereo"`
	LimitRuleIDs   []string      `json:"limitRuleIds"`
	PartialRuleIDs []string      `json:"partialRuleIds"`
	// DeviceMaxHeight and DeviceMaxBitrateKbps are the filtered device's
	// own ceiling (after a rule lifts an Android phone's 1080p), set only
	// when the summary is filtered to one device or app; 0 = none. The
	// play gets the lower of these and MaxHeight / MaxBitrateKbps.
	DeviceMaxHeight      int `json:"deviceMaxHeight,omitempty"`
	DeviceMaxBitrateKbps int `json:"deviceMaxBitrateKbps,omitempty"`
}

// QualitySummaryException lists a viewer- or device-scoped rule that can match
// a library when the summary is unfiltered.
type QualitySummaryException struct {
	RuleID         string `json:"ruleId"`
	Profile        string `json:"profile"`
	MaxHeight      int    `json:"maxHeight"`
	MaxBitrateKbps int    `json:"maxBitrateKbps"`
	Stereo         bool   `json:"stereo"`
}

// ParseSummaryDevice parses the `device` query parameter of
// GET /api/quality/summary, splitting on its FIRST colon (e.g.
// "group:tv", "client:abc", "app:plex:Plex for iOS").
func ParseSummaryDevice(raw string) (SummaryDevice, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SummaryDevice{}, true
	}
	field, value, ok := strings.Cut(raw, ":")
	field = strings.TrimSpace(field)
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return SummaryDevice{}, false
	}
	switch field {
	case "group", "type", "app", "client", "contains":
		return SummaryDevice{Field: field, Value: value}, true
	default:
		return SummaryDevice{}, false
	}
}

// SummarizeQuality symbolically evaluates q across libs (in the caller's sort
// order) for the optional viewer and device filters. No titles are queried.
func SummarizeQuality(q config.Quality, libs []SummaryLibrary, viewerID int64, device SummaryDevice) QualitySummary {
	libByID := make(map[int64]SummaryLibrary, len(libs))
	for _, lib := range libs {
		libByID[lib.ID] = lib
	}
	filtered := viewerID > 0 || device.Field != ""
	devMatcher := newSummaryDeviceMatcher(device)

	rows := make([]QualitySummaryRow, 0, len(libs))
	for _, lib := range libs {
		row := QualitySummaryRow{
			LibraryID:      lib.ID,
			Library:        lib.Name,
			Plugin:         lib.Plugin,
			ChosenByPlugin: lib.ChosenByPlugin,
			Cells:          []QualitySummaryKindCells{},
			Exceptions:     []QualitySummaryException{},
		}

		// Determine which kinds this library needs cells for.
		kinds := summaryKindsForLibrary(q.Rules, lib, libByID, filtered, viewerID, devMatcher)
		for _, kind := range kinds {
			home := evalSummaryCell(q, lib, libByID, config.WhereHome, kind, filtered, viewerID, devMatcher)
			away := evalSummaryCell(q, lib, libByID, config.WhereAway, kind, filtered, viewerID, devMatcher)
			row.Cells = append(row.Cells, QualitySummaryKindCells{Kind: kind, Home: home, Away: away})
		}

		// When unfiltered, list viewer- or device-scoped rules that can match this library.
		if !filtered {
			for _, r := range q.Rules {
				if len(r.Viewers) == 0 && len(r.Devices) == 0 {
					continue
				}
				if !ruleKindCompatibleWithLibrary(r.Kind, lib) {
					continue
				}
				if !ruleCanMatchLibrary(r, lib, libByID) {
					continue
				}
				row.Exceptions = append(row.Exceptions, QualitySummaryException{
					RuleID:         r.ID,
					Profile:        r.Profile,
					MaxHeight:      r.MaxHeight,
					MaxBitrateKbps: r.MaxBitrateKbps,
					Stereo:         r.StereoAudio,
				})
			}
		}
		rows = append(rows, row)
	}
	return QualitySummary{Rows: rows}
}

func summaryKindsForLibrary(rules []config.QualityRule, lib SummaryLibrary, libByID map[int64]SummaryLibrary, filtered bool, viewerID int64, dev summaryDeviceMatcher) []string {
	switch lib.MediaType {
	case core.TypeMovie:
		return []string{config.KindMovie}
	case core.TypeShow:
		return []string{config.KindEpisode}
	}
	if lib.ChosenByPlugin {
		return []string{""}
	}
	// Mixed library (e.g. collection with both movies and episodes): split into
	// movie and episode only if a rule using Kind can apply to this library.
	for _, r := range rules {
		if r.Kind == "" {
			continue
		}
		if !filtered && (len(r.Viewers) > 0 || len(r.Devices) > 0) {
			continue
		}
		if filtered && !summaryRuleMatchesViewerAndDevice(r, viewerID, dev) {
			continue
		}
		if ruleLibraryRelation(r, lib, libByID) != libRelNone {
			return []string{config.KindMovie, config.KindEpisode}
		}
	}
	return []string{""}
}

type libRelation int

const (
	libRelNone libRelation = iota
	libRelDirect
	libRelPartial
)

func ruleLibraryRelation(r config.QualityRule, lib SummaryLibrary, libByID map[int64]SummaryLibrary) libRelation {
	if len(r.Libraries) == 0 {
		return libRelDirect
	}
	inList := slices.Contains(r.Libraries, lib.ID)
	if r.ExceptLibraries {
		if inList {
			return libRelNone
		}
		return libRelDirect
	}
	if inList {
		return libRelDirect
	}
	// Another library is named: check whether any named library can overlap lib.
	if lib.Plugin != "" {
		return libRelNone
	}
	for _, otherID := range r.Libraries {
		other, ok := libByID[otherID]
		if !ok || other.Plugin != "" {
			continue
		}
		if librariesCanOverlap(lib, other) {
			return libRelPartial
		}
	}
	return libRelNone
}

func librariesCanOverlap(a, b SummaryLibrary) bool {
	if a.Plugin != "" || b.Plugin != "" {
		return a.ID == b.ID
	}
	if a.MediaType != "" && b.MediaType != "" && a.MediaType != b.MediaType {
		return false
	}
	return true
}

func ruleKindCompatibleWithLibrary(kind string, lib SummaryLibrary) bool {
	if kind == "" {
		return true
	}
	switch lib.MediaType {
	case core.TypeMovie:
		return kind == config.KindMovie
	case core.TypeShow:
		return kind == config.KindEpisode
	default:
		return true
	}
}

func ruleCanMatchLibrary(r config.QualityRule, lib SummaryLibrary, libByID map[int64]SummaryLibrary) bool {
	if len(r.Libraries) == 0 {
		return true
	}
	inList := slices.Contains(r.Libraries, lib.ID)
	if r.ExceptLibraries {
		return !inList
	}
	return inList
}

func evalSummaryCell(q config.Quality, lib SummaryLibrary, libByID map[int64]SummaryLibrary, where, kind string, filtered bool, viewerID int64, dev summaryDeviceMatcher) QualitySummaryCell {
	cell := QualitySummaryCell{
		LimitRuleIDs:   []string{},
		PartialRuleIDs: []string{},
	}
	var limits ruleLimits
	for _, r := range q.Rules {
		if !filtered && (len(r.Viewers) > 0 || len(r.Devices) > 0) {
			continue
		}
		if r.Where != "" && r.Where != where {
			continue
		}
		if r.Kind != "" && (kind == "" || r.Kind != kind) {
			continue
		}
		if filtered && !summaryRuleMatchesViewerAndDevice(r, viewerID, dev) {
			continue
		}
		rel := ruleLibraryRelation(r, lib, libByID)
		switch rel {
		case libRelNone:
			continue
		case libRelPartial:
			if r.ID != "" {
				cell.PartialRuleIDs = append(cell.PartialRuleIDs, r.ID)
			}
			continue
		case libRelDirect:
			if r.HasLimits() {
				limits.add(r.Limits())
				if r.ID != "" {
					cell.LimitRuleIDs = append(cell.LimitRuleIDs, r.ID)
				}
			}
			if !lib.ChosenByPlugin && cell.Profile == "" && r.Profile != "" {
				if p, ok := core.FindProfile(q.Profiles, r.Profile); ok {
					cell.Profile = p.Name
					cell.Source = ProfileFromRule
					cell.ProfileRuleID = r.ID
				}
			}
		}
	}
	if lib.ChosenByPlugin {
		cell.Source = ProfileFromPlugin
	} else if cell.Profile == "" {
		choice := fallbackProfile(q, where == config.WhereAway)
		cell.Profile = choice.profile.Name
		cell.Source = choice.source
		source, fallback := fallbackLimits(q, choice)
		if fallback.HasLimits() {
			limits.add(fallback)
			cell.LimitRuleIDs = append(cell.LimitRuleIDs, string(source))
		}
	}
	cell.MaxHeight = limits.maxHeight
	cell.MaxBitrateKbps = limits.maxBitrateKbps
	cell.Stereo = limits.stereo
	return cell
}

func summaryRuleMatchesViewerAndDevice(r config.QualityRule, viewerID int64, dev summaryDeviceMatcher) bool {
	if len(r.Viewers) > 0 {
		in := viewerID > 0 && slices.Contains(r.Viewers, viewerID)
		if in == r.ExceptViewers {
			return false
		}
	}
	if len(r.Devices) > 0 {
		in := slices.ContainsFunc(r.Devices, dev.matches)
		if in == r.ExceptDevices {
			return false
		}
	}
	return true
}

type summaryDeviceMatcher struct {
	dev     SummaryDevice
	group   string
	devType string
	app     string
	hasCaps bool
	caps    core.ClientCaps
}

func newSummaryDeviceMatcher(dev SummaryDevice) summaryDeviceMatcher {
	m := summaryDeviceMatcher{dev: dev}
	switch dev.Field {
	case "group":
		m.group = dev.Value
	case "type":
		m.devType = dev.Value
		for _, dt := range clientreg.DeviceTypes() {
			if dt.ID == dev.Value {
				m.group = dt.Group
				break
			}
		}
		for _, d := range clientreg.Devices() {
			if d.TypeID() == dev.Value {
				m.hasCaps = true
				m.caps = core.ClientCaps{Product: d.Identity.Product, Platform: d.Identity.Platform, Device: d.Identity.Device, Model: d.Identity.Model}
				break
			}
		}
	case "app":
		m.app = dev.Value
		for _, a := range clientreg.Apps() {
			if clientreg.AppID(a) == dev.Value {
				id := a.Identity
				if id.Product == "" {
					id.Product = a.Product
				}
				if id.Product == "" {
					id.Product = a.Name
				}
				m.group = clientreg.Class(id)
				m.devType = clientreg.DeviceType(id)
				m.hasCaps = true
				m.caps = core.ClientCaps{
					Protocol: core.Protocol(a.Protocol),
					Product:  id.Product,
					Platform: id.Platform,
					Device:   id.Device,
				}
				break
			}
		}
	case "client":
		m.group = dev.ClientGroup
		m.devType = dev.ClientType
		m.app = dev.ClientApp
		m.caps = dev.ClientCaps
		m.caps.ClientID = dev.Value
		m.hasCaps = true
	}
	return m
}

func (m summaryDeviceMatcher) matches(d config.DeviceMatch) bool {
	if m.dev.Field == "" {
		return false
	}
	switch field, v := d.Field(); field {
	case "group":
		return m.group != "" && m.group == v
	case "type":
		return clientreg.DeviceTypeMatches(m.devType, v)
	case "app":
		return m.app != "" && m.app == v
	case "client":
		return m.dev.Field == "client" && m.dev.Value == v
	case "contains":
		if m.dev.Field == "contains" {
			return strings.EqualFold(strings.TrimSpace(m.dev.Value), strings.TrimSpace(v))
		}
		if m.hasCaps {
			return containsMatches(v, m.caps)
		}
	}
	return false
}
