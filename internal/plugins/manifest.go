package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ParseManifest decodes manifest JSON. Unknown fields are ignored (SPEC
// §5.1.1). Call Validate before trusting the result.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("plugins: manifest: %w", err)
	}
	return m, nil
}

// UnmarshalJSON completes a Stremio-backed plugin's roles from its
// catalogs and resources wherever a manifest is read: packages, stored
// records and drafts alike.
func (m *Manifest) UnmarshalJSON(b []byte) error {
	type plain Manifest
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*m = Manifest(p)
	if m.Ether != nil && m.Ether.Stremio {
		m.Ether = m.withStremioRoles()
	}
	return nil
}

var (
	idRe       = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9][a-z0-9-]*)+$`)
	keyRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	hostRe     = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	bannedRe   = regexp.MustCompile(`(?i)\b(simply|easily|just)\b`)
)

// LintCopy checks one user-visible string against the copy rules: at most
// max characters (0 means no limit), no em or en dashes, no exclamation
// marks, no "simply", "just" or "easily", and sentence case. It returns a
// plain reason, or "" when the copy passes. Shared with the kit (WS11).
func LintCopy(s string, max int) string {
	s = strings.TrimSpace(s)
	switch {
	case max > 0 && utf8.RuneCountInString(s) > max:
		return fmt.Sprintf("is longer than %d characters", max)
	case strings.ContainsAny(s, "—–"):
		return "must not contain em or en dashes"
	case strings.Contains(s, "!"):
		return "must not contain exclamation marks"
	case bannedRe.MatchString(s):
		return "must not use the words simply, just or easily"
	}
	for _, r := range s {
		if unicode.IsLetter(r) {
			if unicode.IsLower(r) {
				return "must use sentence case"
			}
			break
		}
	}
	// Reject Title Case: more than two thirds of words capitalised, so one
	// proper noun ("Screenings this Friday") still passes.
	words := strings.Fields(s)
	if len(words) >= 3 {
		caps := 0
		for _, w := range words {
			r, _ := utf8.DecodeRuneInString(w)
			if unicode.IsUpper(r) {
				caps++
			}
		}
		if caps*3 > len(words)*2 {
			return "must use sentence case"
		}
	}
	return ""
}

// capabilityKinds is the SPEC §5.1.3 table. A nil set means any kind.
var capabilityKinds = map[Capability]map[Kind]bool{
	CapStreamSource:    {KindSource: true},
	CapOnAirContent:    {KindExtension: true},
	CapLiveTVPack:      {KindExtension: true},
	CapDebridProvider:  {KindSource: true},
	CapListSource:      {KindIntegration: true, KindExtension: true},
	CapPluginDirectory: {KindIntegration: true, KindExtension: true, KindSource: true},
	CapMetadata:        {KindIntegration: true, KindExtension: true, KindSource: true},
	CapLiveTV:          {KindLiveTVSource: true},
	CapSubtitles:       {KindIntegration: true, KindExtension: true},
	CapMarkers:         {KindIntegration: true, KindExtension: true},
	CapNotify:          {KindNotificationChannel: true},
	CapRankingStep:     {KindExtension: true, KindIntegration: true},
	CapRuleCondition:   {KindExtension: true, KindIntegration: true},
	CapEvents:          nil, CapTools: nil, CapUI: nil,
}

var knownKinds = map[Kind]bool{
	KindSource: true, KindIntegration: true, KindNotificationChannel: true,
	KindLiveTVSource: true, KindExtension: true,
}

// IsStremioOnly reports whether m is a plain Stremio add-on (no ether block).
func (m Manifest) IsStremioOnly() bool { return m.Ether == nil }

// SpeaksStremio reports whether Ether reads this plugin's catalogues,
// metadata, subtitles, channels and directories over Stremio routes: a
// plain add-on, or a native plugin built from one in Studio.
func (m Manifest) SpeaksStremio() bool { return m.Ether == nil || m.Ether.Stremio }

// HasCapability reports whether the manifest declares c.
func (m Manifest) HasCapability(c Capability) bool {
	for _, have := range m.EffectiveEther().Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// Validate checks every field rule in SPEC §5.1.1 and §5.1.3 and the copy
// rules for summary and contribution labels. It returns a
// *ValidationError listing all problems, or nil.
func (m Manifest) Validate() error {
	var f []FieldError
	add := func(field, msg string) { f = append(f, FieldError{field, msg}) }

	switch {
	case !(idRe.MatchString(m.ID) || (m.Ether == nil && regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)+$`).MatchString(m.ID))):
		add("id", "must be a reverse-DNS id in lower case, such as community.example.plugin")
	case len(m.ID) > 120:
		add("id", "is longer than 120 characters")
	}
	if _, ok := ParseSemVer(m.Version); !ok {
		add("version", "must be a version such as 1.2.0")
	}
	if strings.TrimSpace(m.Name) == "" {
		add("name", "is required")
	} else if m.Ether != nil && utf8.RuneCountInString(m.Name) > 60 {
		add("name", "is longer than 60 characters")
	}
	if m.Ether != nil && strings.TrimSpace(m.Description) != "" {
		if msg := LintCopy(m.Description, 300); msg != "" {
			add("description", msg)
		}
	}
	if m.Ether == nil {
		if len(f) > 0 {
			return &ValidationError{f}
		}
		return nil
	}
	e := m.Ether
	if e.Protocol != Protocol {
		add("ether.protocol", fmt.Sprintf("this version of Ether speaks plugin protocol %d", Protocol))
	}
	if !knownKinds[e.Kind] {
		add("ether.kind", "must be source, integration, notification_channel, livetv_source or extension")
	}
	if e.Kind == "sign_in" {
		add("ether.kind", "sign_in is reserved for a later release")
	}
	seenCap := map[Capability]bool{}
	for i, c := range e.Capabilities {
		field := fmt.Sprintf("ether.capabilities[%d]", i)
		switch {
		case ReservedCapabilities[c]:
			add(field, string(c)+" is reserved for a later release")
		case capabilityKinds[c] == nil && !isAnyKindCap(c):
			add(field, "unknown capability "+strconv.Quote(string(c)))
		case seenCap[c]:
			add(field, "is listed twice")
		default:
			// An add-on-backed plugin's roles come from its Stremio
			// resources, which may combine (a catalogue that also streams).
			if allowed := capabilityKinds[c]; allowed != nil && !allowed[e.Kind] && !e.Stremio {
				add(field, string(c)+" is not allowed for kind "+string(e.Kind))
			}
		}
		seenCap[c] = true
	}
	if !strings.HasPrefix(e.Summary, "$t:") {
		if strings.TrimSpace(e.Summary) == "" {
			add("ether.summary", "is required")
		} else if msg := LintCopy(e.Summary, 80); msg != "" {
			add("ether.summary", msg)
		}
	}
	if e.Icon != "" && !strings.HasSuffix(strings.ToLower(e.Icon), ".svg") && !strings.HasPrefix(e.Icon, "http") {
		add("ether.icon", "must be an SVG file")
	}
	if len(e.Contributes.Looks) > 0 && !m.HasCapability(CapLiveTVPack) {
		add("ether.capabilities", "livetv_pack is required for style contributions")
	}
	if m.HasCapability(CapLiveTVPack) && e.Runtime.Kind != "data" {
		add("ether.runtime.kind", "style packs require the data runtime")
	}
	switch e.Runtime.Kind {
	case "data":
		if e.Kind != KindExtension || len(e.Capabilities) != 1 || !m.HasCapability(CapLiveTVPack) || len(e.Contributes.Looks) != 1 {
			add("ether.contributes.looks", "a data package must contribute exactly one style")
		}
		if e.Runtime.Entry != "" || e.Runtime.Module != "" {
			add("ether.runtime", "data packages run no code")
		}
		c := e.Contributes
		c.Looks = nil
		raw, _ := json.Marshal(c)
		if string(raw) != "{}" {
			add("ether.contributes", "data packages may contribute only styles")
		}
		permissions, _ := json.Marshal(e.Permissions)
		empty, _ := json.Marshal(Permissions{})
		if string(permissions) != string(empty) || len(e.Schedule) > 0 {
			add("ether.permissions", "data packages require no permissions or schedules")
		}
		for _, look := range e.Contributes.Looks {
			if look.ID == "" || look.File == "" || unsafePath(look.File) {
				add("ether.contributes.looks", "style files must stay inside the package")
			}
		}

	case "hosted":
		if e.Runtime.Entry == "" && e.Runtime.Module == "" {
			add("ether.runtime", "a hosted plugin needs an entry or module file")
		}
		if e.Runtime.Entry != "" && e.Runtime.Module != "" {
			add("ether.runtime", "give either entry or module, not both")
		}
		for _, p := range []string{e.Runtime.Entry, e.Runtime.Module} {
			if unsafePath(p) {
				add("ether.runtime", "file paths must stay inside the package")
			}
		}
	case "remote":
		if e.Runtime.Entry != "" || e.Runtime.Module != "" {
			add("ether.runtime", "a remote plugin has no entry or module file")
		}
	default:
		add("ether.runtime.kind", "must be hosted, remote or data")
	}
	if e.Cluster != "" && e.Cluster != "leader" && e.Cluster != "every_node" {
		add("ether.cluster", "must be leader or every_node")
	}
	if e.Health.Interval != "" {
		d, err := time.ParseDuration(e.Health.Interval)
		if err != nil || d < 15*time.Second {
			add("ether.health.interval", "must be a duration of at least 15s")
		}
	}
	for i, s := range e.Schedule {
		field := fmt.Sprintf("ether.schedule[%d]", i)
		d, err := time.ParseDuration(s.Every)
		if err != nil || d < 15*time.Minute {
			add(field+".every", "must be a duration of at least 15m")
		}
		if !strings.HasPrefix(s.Call, "/") {
			add(field+".call", "must be a plugin route starting with /")
		}
	}
	validatePermissions(e.Permissions, add)
	validateSettings(e.Settings, add)
	usesT := strings.HasPrefix(e.Summary, "$t:") || contributesUseT(e.Contributes)
	if usesT {
		if _, ok := e.Strings["en"]; !ok {
			add("ether.strings", "an en strings file is required when $t: keys are used")
		}
	}
	validateContributes(m, add)
	validateOnAir(m, add)
	if len(f) > 0 {
		return &ValidationError{f}
	}
	return nil
}

func isAnyKindCap(c Capability) bool {
	return c == CapEvents || c == CapTools || c == CapUI
}

func unsafePath(p string) bool {
	return p != "" && (strings.HasPrefix(p, "/") || strings.Contains(p, ".."))
}

func validatePermissions(p Permissions, add func(string, string)) {
	for i, h := range p.Network {
		if !hostRe.MatchString(strings.ToLower(h)) || strings.Contains(h, "://") {
			add(fmt.Sprintf("ether.permissions.network[%d]", i), "must be a host name, with a wildcard only as *.example.com")
		}
	}
	level := func(field, v string, allowed ...string) {
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		add(field, "must be one of "+strings.Join(allowed, ", "))
	}
	level("ether.permissions.catalog", normLevel(p.Catalog), "none", "read")
	level("ether.permissions.collections", normLevel(p.Collections), "none", "write")
	level("ether.permissions.folders", normLevel(p.Folders), "none", "read")
	level("ether.permissions.viewers.identity", normLevel(p.Viewers.Identity), "none", "granted_viewers", "all")
	level("ether.permissions.viewers.watch_history", normLevel(p.Viewers.WatchHistory), "none", "granted_viewers", "all")
	level("ether.permissions.viewers.write_watch_state", normLevel(p.Viewers.WriteWatchState), "none", "granted_viewers")
	for i, s := range p.Secrets {
		if !keyRe.MatchString(s) {
			add(fmt.Sprintf("ether.permissions.secrets[%d]", i), "must be a simple name")
		}
	}
}

func normLevel(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

// validateSettings checks the settings schema shape (SPEC §5.5). A $ref is
// validated again after resolution at install.
func validateSettings(raw json.RawMessage, add func(string, string)) {
	if len(raw) == 0 {
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		add("ether.settings", "must be a JSON object")
		return
	}
	if ref, ok := doc["$ref"].(string); ok {
		if unsafePath(ref) || ref == "" {
			add("ether.settings.$ref", "must name a file inside the package")
		}
		return
	}
	lintSettingsNode(doc, "ether.settings", add)
}

var settingKinds = map[string]bool{
	"group": true, "text": true, "secret": true, "duration": true,
	"readonly": true, "number": true, "bool": true, "select": true,
	"list": true, "bytes": true, "hidden": true, "multiSelect": true,
	"orderedList": true, "language": true, "viewer": true,
	"collection": true, "folder": true, "url": true, "title": true,
}

// LintSettingsSchema validates a resolved settings schema document
// (SPEC §5.5 copy rules). Shared with the kit.
func LintSettingsSchema(raw json.RawMessage) error {
	var f []FieldError
	validateSettings(raw, func(field, msg string) { f = append(f, FieldError{field, msg}) })
	if len(f) > 0 {
		return &ValidationError{f}
	}
	return nil
}

func lintSettingsNode(node any, path string, add func(string, string)) {
	switch n := node.(type) {
	case map[string]any:
		if k, ok := n["kind"].(string); ok {
			if k == "json" {
				add(path+".kind", "the json field kind is not allowed in plugin settings")
			} else if !settingKinds[k] {
				add(path+".kind", "unknown field kind "+strconv.Quote(k))
			}
		}
		if label, ok := n["label"].(string); ok && !strings.HasPrefix(label, "$t:") {
			if msg := LintCopy(label, 40); msg != "" {
				add(path+".label", msg)
			}
		}
		if hint, ok := n["hint"].(string); ok && !strings.HasPrefix(hint, "$t:") {
			if msg := LintCopy(hint, 120); msg != "" {
				add(path+".hint", msg)
			}
			if strings.Count(strings.TrimRight(hint, "."), ".") > 0 {
				add(path+".hint", "must be one sentence")
			}
		}
		for key, v := range n {
			lintSettingsNode(v, path+"."+key, add)
		}
	case []any:
		for i, v := range n {
			lintSettingsNode(v, fmt.Sprintf("%s[%d]", path, i), add)
		}
	}
}

func contributesUseT(c Contributes) bool {
	any := false
	check := func(s string) {
		if strings.HasPrefix(s, "$t:") {
			any = true
		}
	}
	for _, x := range c.ListSources {
		check(x.Label)
	}
	for _, x := range c.RankingSteps {
		check(x.Label)
	}
	for _, x := range c.RuleConditions {
		check(x.Label)
	}
	for _, x := range c.Tools {
		check(x.Title)
	}
	for _, x := range c.Pages {
		check(x.Label)
	}
	for _, x := range c.Slots {
		check(x.Label)
	}
	for _, x := range c.Commands {
		check(x.Label)
	}
	if c.Integration != nil {
		for _, s := range c.Integration.Adds {
			check(s)
		}
	}
	return any
}

func validateContributes(m Manifest, add func(string, string)) {
	e := m.Ether
	c := e.Contributes
	hosted := e.Runtime.Kind == "hosted"
	label := func(field, s string, max int) {
		if strings.HasPrefix(s, "$t:") {
			if !keyRe.MatchString(strings.TrimPrefix(s, "$t:")) {
				add(field, "must name a string key after $t:")
			}
			return
		}
		if strings.TrimSpace(s) == "" {
			add(field, "is required")
		} else if msg := LintCopy(s, max); msg != "" && !e.Stremio {
			// Labels an add-on brings (its catalogue names) are its own
			// words; Ether shows them as the add-on wrote them.
			add(field, msg)
		}
	}
	if m.HasCapability(CapStreamSource) {
		if c.Source == nil {
			add("ether.contributes.source", "is required for stream_source")
		} else if !map[string]bool{"debrid": true, "usenet": true, "indexer": true, "direct": true}[c.Source.Class] {
			add("ether.contributes.source.class", "must be debrid, usenet, indexer or direct")
		}
	}
	if m.HasCapability(CapDebridProvider) && (c.Source == nil || c.Source.Class != "debrid") {
		add("ether.contributes.source.class", "must be debrid for debrid_provider")
	}
	if m.HasCapability(CapLiveTV) {
		if c.Channels == nil || !c.Channels.Test {
			add("ether.contributes.channels", "is required for livetv_channels, with test set")
		}
	}
	if m.HasCapability(CapPluginDirectory) && len(c.Directories) == 0 {
		add("ether.contributes.directories", "is required for plugin_directory")
	}
	seenDirectories := map[string]bool{}
	for i, d := range c.Directories {
		base := fmt.Sprintf("ether.contributes.directories[%d]", i)
		if !keyRe.MatchString(d.Key) || seenDirectories[d.Key] {
			add(base+".key", "must be a unique simple key")
		}
		seenDirectories[d.Key] = true
		label(base+".label", d.Label, 80)
	}
	if m.HasCapability(CapMetadata) {
		if c.Metadata == nil || len(c.Metadata.Types) == 0 {
			add("ether.contributes.metadata.types", "is required for metadata")
		} else {
			for _, typ := range c.Metadata.Types {
				if typ != "movie" && typ != "show" {
					add("ether.contributes.metadata.types", "must contain movie or show")
				}
			}
		}
	}
	if m.HasCapability(CapSubtitles) && c.Subtitles == nil {
		add("ether.contributes.subtitles", "is required for subtitles")
	}
	if m.HasCapability(CapMarkers) {
		if c.Markers == nil || len(c.Markers.Kinds) == 0 {
			add("ether.contributes.markers.kinds", "is required for markers")
		} else {
			for _, k := range c.Markers.Kinds {
				if !map[string]bool{"intro": true, "credits": true, "recap": true, "preview": true}[k] {
					add("ether.contributes.markers.kinds", "unknown marker kind "+strconv.Quote(k))
				}
			}
		}
	}
	if m.HasCapability(CapNotify) {
		if c.Notification == nil || !c.Notification.SendTest {
			add("ether.contributes.notification", "is required for notify, with sendTest set")
		}
	}
	if m.HasCapability(CapEvents) {
		if c.Events == nil || len(c.Events.Subscribe) == 0 {
			add("ether.contributes.events.subscribe", "is required for events")
		} else {
			for _, n := range c.Events.Subscribe {
				if !KnownEvents[n] {
					add("ether.contributes.events.subscribe", "unknown event "+strconv.Quote(n))
				}
			}
		}
	}
	if m.HasCapability(CapListSource) && len(c.ListSources) == 0 {
		add("ether.contributes.listSources", "is required for list_source")
	}
	for i, ls := range c.ListSources {
		base := fmt.Sprintf("ether.contributes.listSources[%d]", i)
		if !keyRe.MatchString(ls.Key) {
			add(base+".key", "must be a simple key")
		}
		label(base+".label", ls.Label, 40)
		if ls.Catalog == "" {
			add(base+".catalog", "is required")
		}
		if ls.MaxItems < 0 || ls.MaxItems > 2000 {
			add(base+".maxItems", "must be at most 2000")
		}
		if ls.Refresh != "" {
			if d, err := time.ParseDuration(ls.Refresh); err != nil || d < time.Hour {
				add(base+".refresh", "must be a duration of at least 1h")
			}
		}
	}
	if m.HasCapability(CapRankingStep) {
		if !hosted {
			add("ether.capabilities", "ranking_step needs the hosted runtime")
		}
		if len(c.RankingSteps) == 0 {
			add("ether.contributes.rankingSteps", "is required for ranking_step")
		}
	}
	for i, rs := range c.RankingSteps {
		base := fmt.Sprintf("ether.contributes.rankingSteps[%d]", i)
		if !keyRe.MatchString(rs.Key) {
			add(base+".key", "must be a simple key")
		}
		label(base+".label", rs.Label, 40)
	}
	if m.HasCapability(CapRuleCondition) {
		if !hosted {
			add("ether.capabilities", "rule_condition needs the hosted runtime")
		}
		if len(c.RuleConditions) == 0 {
			add("ether.contributes.ruleConditions", "is required for rule_condition")
		}
	}
	for i, rc := range c.RuleConditions {
		base := fmt.Sprintf("ether.contributes.ruleConditions[%d]", i)
		if !keyRe.MatchString(rc.Key) {
			add(base+".key", "must be a simple key")
		}
		label(base+".label", rc.Label, 40)
	}
	if m.HasCapability(CapTools) && len(c.Tools) == 0 {
		add("ether.contributes.tools", "is required for tools")
	}
	for i, tl := range c.Tools {
		base := fmt.Sprintf("ether.contributes.tools[%d]", i)
		if !toolNameRe.MatchString(tl.Name) {
			add(base+".name", "must be snake_case")
		}
		label(base+".title", tl.Title, 60)
		if strings.TrimSpace(tl.Description) == "" {
			add(base+".description", "is required")
		}
		var schema map[string]any
		if len(tl.InputSchema) == 0 || json.Unmarshal(tl.InputSchema, &schema) != nil || schema["type"] != "object" {
			add(base+".inputSchema", "must be a JSON object schema")
		}
		if tl.ReadOnly && tl.Destructive {
			add(base, "a tool cannot be both read-only and destructive")
		}
	}
	for i, pg := range c.Pages {
		base := fmt.Sprintf("ether.contributes.pages[%d]", i)
		if !keyRe.MatchString(pg.Key) {
			add(base+".key", "must be a simple key")
		}
		label(base+".label", pg.Label, 40)
		if !strings.HasPrefix(pg.Route, "/") {
			add(base+".route", "must be a plugin route starting with /")
		}
	}
	for i, sl := range c.Slots {
		base := fmt.Sprintf("ether.contributes.slots[%d]", i)
		if !knownSlots[sl.Slot] {
			add(base+".slot", "unknown slot "+strconv.Quote(sl.Slot))
		}
		label(base+".label", sl.Label, 40)
		if !keyRe.MatchString(sl.Key) {
			add(base+".key", "must be a simple key")
		}
		if strings.HasPrefix(sl.Slot, "actionMenu.") {
			if !keyRe.MatchString(sl.Action) {
				add(base+".action", "must be a simple action name")
			}
			if sl.Route != "" {
				add(base+".route", "action slots use an action instead of a route")
			}
		} else {
			if !strings.HasPrefix(sl.Route, "/") {
				add(base+".route", "must be a plugin route starting with /")
			}
			if sl.Action != "" {
				add(base+".action", "view slots use a route instead of an action")
			}
		}
	}
	for i, cm := range c.Commands {
		base := fmt.Sprintf("ether.contributes.commands[%d]", i)
		if !keyRe.MatchString(cm.Key) {
			add(base+".key", "must be a simple key")
		}
		label(base+".label", cm.Label, 40)
		if cm.Action == "" {
			add(base+".action", "is required")
		}
	}
	if e.Kind == KindIntegration {
		if c.Integration == nil {
			add("ether.contributes.integration", "is required for the integration kind")
		} else {
			if c.Integration.Category == "" {
				add("ether.contributes.integration.category", "is required")
			}
			if len(c.Integration.Adds) < 1 || len(c.Integration.Adds) > 4 {
				add("ether.contributes.integration.adds", "must list one to four things the plugin adds")
			}
			for i, s := range c.Integration.Adds {
				label(fmt.Sprintf("ether.contributes.integration.adds[%d]", i), s, 80)
			}
			if !map[string]bool{"fields": true, "device_code": true, "none": true}[c.Integration.Connect] {
				add("ether.contributes.integration.connect", "must be fields, device_code or none")
			}
		}
	}
}

var knownSlots = map[string]bool{
	"page": true, "title.panel": true, "viewer.tab": true,
	"dashboard.panel": true, "actionMenu.title": true,
	"actionMenu.viewer": true, "actionMenu.collection": true,
	"actionMenu.release": true, "command": true,
}

// ParseSemVer parses a plain major.minor.patch version.
func ParseSemVer(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// CompareSemVer returns -1, 0 or 1. Unparseable versions compare equal to 0.
func CompareSemVer(a, b string) int {
	va, _ := ParseSemVer(a)
	vb, _ := ParseSemVer(b)
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			if va[i] < vb[i] {
				return -1
			}
			return 1
		}
	}
	// A release sorts after any prerelease of the same core version.
	pre := func(v string) string {
		v, _, _ = strings.Cut(v, "+")
		_, p, _ := strings.Cut(v, "-")
		return p
	}
	pa, pb := pre(a), pre(b)
	if pa == pb {
		return 0
	}
	if pa == "" {
		return 1
	}
	if pb == "" {
		return -1
	}
	as, bs := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		if x == y {
			continue
		}
		isNumeric := func(s string) bool {
			if s == "" {
				return false
			}
			for _, c := range s {
				if c < '0' || c > '9' {
					return false
				}
			}
			return true
		}
		xn, yn := isNumeric(x), isNumeric(y)
		if xn != yn {
			if xn {
				return -1
			}
			return 1
		}
		if xn {
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(x) < len(y) {
				return -1
			}
			if len(x) > len(y) {
				return 1
			}
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	if len(as) < len(bs) {
		return -1
	}
	if len(as) > len(bs) {
		return 1
	}
	return 0
}
