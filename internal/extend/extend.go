// Package extend is the descriptor registry the owner console renders from.
//
// Built-ins and plugins describe the same things in the same shape: ranking
// steps, rule fields, collection and Live TV source kinds, notification
// channel kinds, integration cards, pages, navigation, commands, settings
// schemas, slots and agent tools. The console never keeps its own copy of
// these lists; it asks the registry. A plugin's contribution is therefore
// indistinguishable from a built-in one except for its Owner and badge.
//
// The registry is in memory. Built-ins register at startup; the plugin host
// replaces a plugin's descriptors atomically when it is installed, updated,
// disabled or removed. Nothing here is persisted.
package extend

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Kind names one family of descriptors. Adding a Kind is a contract change.
type Kind string

const (
	KindRankingStep      Kind = "rankingStep"
	KindRuleField        Kind = "ruleField"
	KindCollectionSource Kind = "collectionSource"
	KindRowSource        Kind = "rowSource"
	KindLiveTVSource     Kind = "liveTVSource"
	KindChannelKind      Kind = "channelKind"
	KindEventKind        Kind = "eventKind"
	KindSubtitleProvider Kind = "subtitleProvider"
	KindMarkerSource     Kind = "markerSource"
	KindSourceKind       Kind = "sourceKind"
	KindIntegration      Kind = "integration"
	KindPage             Kind = "page"
	KindNavItem          Kind = "navItem"
	KindCommand          Kind = "command"
	KindSettingsSchema   Kind = "settingsSchema"
	KindSlot             Kind = "slot"
	KindWidget           Kind = "widget"
	KindTool             Kind = "tool"
)

var knownKinds = map[Kind]bool{
	KindRankingStep: true, KindRuleField: true, KindCollectionSource: true, KindRowSource: true,
	KindLiveTVSource: true, KindChannelKind: true, KindEventKind: true, KindSubtitleProvider: true,
	KindMarkerSource: true, KindSourceKind: true, KindIntegration: true, KindPage: true,
	KindNavItem: true, KindCommand: true, KindSettingsSchema: true, KindSlot: true,
	KindWidget: true, KindTool: true,
}

// Known reports whether k is a registered descriptor kind.
func Known(k Kind) bool { return knownKinds[k] }

// Builtin is the Owner of every descriptor Ether itself registers.
const Builtin = "builtin"

// Text is user-visible copy. Built-ins carry an English source string that the
// console translates through its catalogue (declare it with T so the i18n
// extractor finds it). Plugins carry a key into their own strings; the
// registry handler resolves it for the requested locale into Resolved.
type Text struct {
	Source   string `json:"source,omitempty"`
	Plugin   string `json:"plugin,omitempty"`
	Key      string `json:"key,omitempty"`
	Resolved string `json:"resolved,omitempty"`
}

// T declares built-in console copy. The argument must be a string literal:
// web/scripts/i18n.mjs extracts extend.T("...") calls from Go sources into
// the console catalogue, exactly like t("...") calls in the console.
func T(source string) Text { return Text{Source: source} }

// PluginText refers to key in plugin's strings.
func PluginText(plugin, key string) Text { return Text{Plugin: plugin, Key: key} }

// Empty reports whether the text carries nothing to show.
func (t Text) Empty() bool { return t.Source == "" && t.Key == "" && t.Resolved == "" }

// Descriptor is one thing the console can render or pick.
type Descriptor struct {
	Kind  Kind   `json:"kind"`
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Label Text   `json:"label"`
	Hint  Text   `json:"hint,omitempty"`
	// Icon is a lucide icon name for built-ins or an asset URL for plugins.
	Icon  string `json:"icon,omitempty"`
	Group string `json:"group,omitempty"`
	Order int    `json:"order,omitempty"`
	// Meta is kind-specific. Each kind's Go type lives in meta_<kind>.go and
	// is mirrored in web/src/lib/extend.ts.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// WithMeta returns d with Meta set to the JSON encoding of v.
func (d Descriptor) WithMeta(v any) Descriptor {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("extend: encode meta for %s/%s: %v", d.Kind, d.ID, err))
	}
	d.Meta = raw
	return d
}

// MetaOf decodes d.Meta into T. An empty Meta yields T's zero value.
func MetaOf[T any](d Descriptor) (T, error) {
	var out T
	if len(d.Meta) == 0 {
		return out, nil
	}
	err := json.Unmarshal(d.Meta, &out)
	return out, err
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)

// PluginID namespaces a plugin's local id: "plugin:<plugin>/<id>".
func PluginID(plugin, id string) string { return "plugin:" + plugin + "/" + id }

// Validate checks the fields every descriptor needs.
func (d Descriptor) Validate() error {
	switch {
	case !Known(d.Kind):
		return fmt.Errorf("unknown descriptor kind %q", d.Kind)
	case !idPattern.MatchString(d.ID):
		return fmt.Errorf("%s: invalid id %q", d.Kind, d.ID)
	case d.Owner == "":
		return fmt.Errorf("%s/%s: owner is required", d.Kind, d.ID)
	case d.Label.Empty():
		return fmt.Errorf("%s/%s: label is required", d.Kind, d.ID)
	case d.Owner != Builtin && !strings.HasPrefix(d.ID, "plugin:"+d.Owner+"/"):
		return fmt.Errorf("%s/%s: plugin descriptors must be namespaced %q", d.Kind, d.ID, PluginID(d.Owner, "…"))
	case d.Owner == Builtin && strings.HasPrefix(d.ID, "plugin:"):
		return fmt.Errorf("%s/%s: built-in ids cannot use the plugin namespace", d.Kind, d.ID)
	}
	if d.Owner == Builtin && (d.Label.Plugin != "" || d.Label.Key != "") {
		return fmt.Errorf("%s/%s: built-in copy must use extend.T", d.Kind, d.ID)
	}
	return nil
}

// ErrDuplicate is returned when a (kind, id) pair is already registered.
var ErrDuplicate = errors.New("descriptor already registered")

type key struct {
	kind Kind
	id   string
}

// Registry holds every descriptor. It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	items   map[key]Descriptor
	version uint64
	subs    map[int]func()
	nextSub int
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{items: map[key]Descriptor{}, subs: map[int]func(){}}
}

// Register adds built-in or plugin descriptors. It is all or nothing: on any
// error nothing is added.
func (r *Registry) Register(ds ...Descriptor) error {
	r.mu.Lock()
	seen := map[key]bool{}
	for _, d := range ds {
		if err := d.Validate(); err != nil {
			r.mu.Unlock()
			return err
		}
		k := key{d.Kind, d.ID}
		if _, ok := r.items[k]; ok || seen[k] {
			r.mu.Unlock()
			return fmt.Errorf("%w: %s/%s", ErrDuplicate, d.Kind, d.ID)
		}
		seen[k] = true
	}
	for _, d := range ds {
		r.items[key{d.Kind, d.ID}] = d
	}
	r.version++
	subs := r.subscribers()
	r.mu.Unlock()
	notify(subs)
	return nil
}

// MustRegister is Register for built-ins whose descriptors are static: a
// failure is a programming error caught by tests.
func (r *Registry) MustRegister(ds ...Descriptor) {
	if err := r.Register(ds...); err != nil {
		panic("extend: " + err.Error())
	}
}

// ReplaceOwner atomically swaps every descriptor owned by owner for ds. The
// plugin host calls it on install, update, enable and disable. Built-ins
// cannot be replaced.
func (r *Registry) ReplaceOwner(owner string, ds []Descriptor) error {
	if owner == "" || owner == Builtin {
		return fmt.Errorf("cannot replace descriptors of owner %q", owner)
	}
	r.mu.Lock()
	seen := map[key]bool{}
	for _, d := range ds {
		if d.Owner != owner {
			r.mu.Unlock()
			return fmt.Errorf("%s/%s: owner %q does not match %q", d.Kind, d.ID, d.Owner, owner)
		}
		if err := d.Validate(); err != nil {
			r.mu.Unlock()
			return err
		}
		k := key{d.Kind, d.ID}
		if existing, ok := r.items[k]; (ok && existing.Owner != owner) || seen[k] {
			r.mu.Unlock()
			return fmt.Errorf("%w: %s/%s", ErrDuplicate, d.Kind, d.ID)
		}
		seen[k] = true
	}
	for k, d := range r.items {
		if d.Owner == owner {
			delete(r.items, k)
		}
	}
	for _, d := range ds {
		r.items[key{d.Kind, d.ID}] = d
	}
	r.version++
	subs := r.subscribers()
	r.mu.Unlock()
	notify(subs)
	return nil
}

// RemoveOwner drops every descriptor owned by a plugin.
func (r *Registry) RemoveOwner(owner string) {
	_ = r.ReplaceOwner(owner, nil)
}

// Get returns one descriptor.
func (r *Registry) Get(kind Kind, id string) (Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.items[key{kind, id}]
	return d, ok
}

// List returns descriptors of the given kinds (all kinds when none are
// given), ordered by kind, group, order, then id.
func (r *Registry) List(kinds ...Kind) []Descriptor {
	want := map[Kind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	r.mu.RLock()
	out := make([]Descriptor, 0, len(r.items))
	for _, d := range r.items {
		if len(want) == 0 || want[d.Kind] {
			out = append(out, d)
		}
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.ID < b.ID
	})
	return out
}

// Owned returns every descriptor owned by owner.
func (r *Registry) Owned(owner string) []Descriptor {
	var out []Descriptor
	for _, d := range r.List() {
		if d.Owner == owner {
			out = append(out, d)
		}
	}
	return out
}

// Version increases on every change. The HTTP handler uses it as an ETag.
func (r *Registry) Version() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.version
}

// Subscribe calls fn after every change. fn must not block.
func (r *Registry) Subscribe(fn func()) (unsubscribe func()) {
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = fn
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}
}

func (r *Registry) subscribers() []func() {
	out := make([]func(), 0, len(r.subs))
	for _, fn := range r.subs {
		out = append(out, fn)
	}
	return out
}

func notify(subs []func()) {
	for _, fn := range subs {
		fn()
	}
}
