package clientreg

import (
	"slices"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

// Device classes group clients for quality rules.
const (
	ClassTV       = "tv"
	ClassMobile   = "mobile"
	ClassComputer = "computer"
)

// ClassInfo names one device class. Aliases are other words an owner may
// search for (the device picker matches them).
type ClassInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// Classes lists the device classes in display order.
func Classes() []ClassInfo {
	return []ClassInfo{
		{ID: ClassTV, Name: "TVs & streaming boxes", Aliases: []string{"TV", "Streaming box", "Smart TV", "Apple TV", "Android TV", "Google TV", "Shield", "Fire TV", "Roku", "Chromecast", "Console"}},
		{ID: ClassMobile, Name: "Phones & tablets", Aliases: []string{"Phone", "Tablet", "Mobile", "iPhone", "iPad", "Android phone", "Android tablet", "Findroid", "Swiftfin", "Streamyfin"}},
		{ID: ClassComputer, Name: "Computers & browsers", Aliases: []string{"Computer", "Browser", "Desktop", "Laptop", "Mac", "Windows", "Linux", "HTPC", "Kodi", "Web"}},
	}
}

// Class is the device class of a client: the registry's class record (apps
// that span device kinds, or report no platform), else the playback
// record's class, else one read from the platform, else the policy
// record's; "" when the client cannot be placed.
func Class(id Identity) string {
	if c := first("class", id).Class; c != "" {
		return c
	}
	if c := first("playback", id).Class; c != "" {
		return c
	}
	switch strings.ToLower(strings.TrimSpace(id.Platform)) {
	case "tvos", "roku", "webos", "tizen", "xbox", "playstation":
		return ClassTV
	case "ios", "ipados":
		return ClassMobile
	case "macos", "windows", "linux", "desktop":
		return ClassComputer
	}
	if core.IsFireTV(id.Product, id.Device) {
		return ClassTV
	}
	return first("policy", id).Class
}

// DeviceTypeInfo is one registry device type a rule can name.
type DeviceTypeInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Group   string   `json:"group"`
	Aliases []string `json:"aliases,omitempty"`
}

type deviceSignature struct {
	id, name, policy, class, parent string
	aliases                         []string
	// words name the device type in a client's identity ("playstation"),
	// so types sharing a policy record ("console") can be told apart.
	words []string
}

var deviceSignatures = sync.OnceValue(func() []deviceSignature {
	out := make([]deviceSignature, 0, len(loaded.Devices))
	for _, d := range loaded.Devices {
		words := []string{strings.ToLower(d.Name)}
		for _, a := range d.Aliases {
			words = append(words, strings.ToLower(a))
		}
		out = append(out, deviceSignature{id: d.TypeID(), name: d.Name, parent: d.Parent, policy: first("policy", d.Identity).ID, class: Class(d.Identity), aliases: d.Aliases, words: words})
	}
	return out
})

// DeviceTypes lists the registry device types (the catalogue devices),
// each with its class. The id is the registry device name.
func DeviceTypes() []DeviceTypeInfo {
	sigs := deviceSignatures()
	out := make([]DeviceTypeInfo, 0, len(sigs))
	for _, s := range sigs {
		out = append(out, DeviceTypeInfo{ID: s.id, Name: s.name, Group: s.class, Aliases: slices.Clone(s.aliases)})
	}
	return out
}

// DeviceAliases are the search words for a registry device type ("iPad"
// for iOS); nil when the type has none.
func DeviceAliases(name string) []string {
	for _, s := range deviceSignatures() {
		if s.id == name || s.name == name {
			return slices.Clone(s.aliases)
		}
	}
	return nil
}

// DeviceType is the registry device type a client is, by its policy
// record and class, or "" when none fits. Fire TV is recognised by its
// model codes, which no catalogue identity carries.
func DeviceType(id Identity) string {
	if core.IsFireTV(id.Product, id.Device) {
		return "Fire TV"
	}
	policy, class := first("policy", id).ID, Class(id)
	// Several types can share a policy record (Xbox and PlayStation are
	// both "console"): prefer the one the identity names. A type with the
	// generic record (Mac) is only ever recognised by name.
	identity := strings.ToLower(id.Product + " " + id.Platform + " " + id.Device)
	found := ""
	for _, s := range deviceSignatures() {
		if s.policy != policy || (class != "" && s.class != "" && s.class != class) {
			continue
		}
		if slices.ContainsFunc(s.words, func(w string) bool { return strings.Contains(identity, w) }) {
			return s.id
		}
		if found == "" && policy != "generic" {
			found = s.id
		}
	}
	return found
}

// DeviceTypeMatches reports whether a client of device type have fits a
// rule naming device type want: the same type, or a type that narrows it
// (an "Apple TV" rule covers "Apple TV HD").
func DeviceTypeMatches(have, want string) bool {
	if have == "" || want == "" {
		return false
	}
	for seen := 0; have != "" && seen < 8; seen++ {
		if have == want {
			return true
		}
		parent := ""
		for _, s := range deviceSignatures() {
			if s.id == have {
				parent = s.parent
				break
			}
		}
		have = parent
	}
	return false
}

// AppID is the opaque id a quality rule uses for a registry app:
// "<protocol>:<name>".
func AppID(a App) string { return a.Protocol + ":" + a.Name }

// AppFor is the registry app a client is, by protocol and product, or ""
// when the registry does not list it.
func AppFor(protocol string, id Identity) string {
	product := strings.TrimSpace(id.Product)
	if product == "" {
		return ""
	}
	for _, a := range loaded.Apps {
		if protocol != "" && !strings.EqualFold(a.Protocol, protocol) {
			continue
		}
		for _, p := range a.AllProducts() {
			if strings.EqualFold(p, product) {
				return AppID(a)
			}
		}
	}
	return ""
}
