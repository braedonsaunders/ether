package newznab

import (
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Identity preset names accepted by the config. Empty means inherit.
const (
	IdentityEther     = "ether"
	IdentityProwlarr  = "prowlarr"
	IdentitySonarr    = "sonarr"
	IdentityRadarr    = "radarr"
	IdentityNZBHydra2 = "nzbhydra2"
	IdentityCustom    = "custom"
)

// queryProfile selects how a persona orders its Newznab query string.
type queryProfile int

const (
	// profileCurrent is the url.Values shape Ether has always sent.
	profileCurrent queryProfile = iota
	profileServarr
	profileProwlarr
)

// identityMedia selects the policy value for a request. Search requests
// always carry a media type; a grab without one falls back to ether.
type identityMedia int

const (
	mediaTV identityMedia = iota
	mediaMovie
	mediaUnknown
)

// identityPurpose selects the policy value for a request.
type identityPurpose int

const (
	purposeSearch identityPurpose = iota
	purposeGrab
)

// maxCustomHeaderBytes bounds the total size of a custom identity's headers.
const maxCustomHeaderBytes = 8 << 10

var rssAccept = map[string]string{"Accept": "application/rss+xml, text/rss+xml, application/xml, text/xml"}

// identityPresets is versioned persona data: refresh a client's literal
// user agent here when its upstream release changes. The (alpine x.y.z)
// token is the common LinuxServer container distribution.
var identityPresets = map[string]identityPreset{
	IdentityEther: {profile: profileCurrent},
	IdentityProwlarr: {
		userAgent: "Prowlarr/2.7.0.5723 (alpine 3.24.3)",
		headers:   rssAccept,
		profile:   profileProwlarr,
	},
	IdentitySonarr: {
		userAgent: "Sonarr/4.0.20.3014 (alpine 3.24.3)",
		headers:   rssAccept,
		profile:   profileServarr,
	},
	IdentityRadarr: {
		userAgent: "Radarr/6.4.4.10685 (alpine 3.24.3)",
		headers:   rssAccept,
		profile:   profileServarr,
	},
	IdentityNZBHydra2: {
		userAgent: "NZBHydra2 9.2.0",
		headers:   map[string]string{"Accept": "application/xml", "Content-Type": "application/xml"},
		profile:   profileServarr,
	},
	IdentityCustom: {profile: profileServarr},
}

type identityPreset struct {
	userAgent string
	headers   map[string]string
	profile   queryProfile
}

// resolvedIdentity is the identity a single request presents.
type resolvedIdentity struct {
	name      string
	userAgent string
	headers   map[string]string
	profile   queryProfile
}

// persona reports whether the identity presents a real client instead of
// Ether itself. Persona torrent fetches ask for a .torrent body.
func (i resolvedIdentity) persona() bool { return i.name != IdentityEther }

// torrent asks for a .torrent body the way a persona client does. The ether
// identity keeps its headers.
func (i resolvedIdentity) torrent() resolvedIdentity {
	if !i.persona() {
		return i
	}
	headers := make(map[string]string, len(i.headers)+1)
	for k, v := range i.headers {
		headers[k] = v
	}
	headers["Accept"] = "application/x-bittorrent"
	i.headers = headers
	return i
}

// IdentityPresets lists the values the owner UI may offer, display order.
func IdentityPresets() []string {
	return []string{IdentityEther, IdentityProwlarr, IdentitySonarr, IdentityRadarr, IdentityNZBHydra2, IdentityCustom}
}

// ValidIdentity reports whether name is a known preset.
func ValidIdentity(name string) bool {
	_, ok := identityPresets[name]
	return ok
}

// PresetUserAgent is the literal user agent a preset presents; custom has
// no fixed value and returns "".
func PresetUserAgent(name, version string) string {
	if name == IdentityCustom {
		return ""
	}
	return presetIdentity(name, version, "", nil).userAgent
}

// etherUserAgent is Ether's own identity, carrying the running build.
func etherUserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return "Ether/" + version
}

// presetIdentity builds the identity for a preset name; the version is
// substituted into the ether user agent only.
func presetIdentity(name, version, userAgent string, headers map[string]string) resolvedIdentity {
	p, ok := identityPresets[name]
	if !ok {
		p = identityPresets[IdentityEther]
		name = IdentityEther
	}
	id := resolvedIdentity{name: name, profile: p.profile, headers: p.headers}
	switch name {
	case IdentityEther:
		id.userAgent = etherUserAgent(version)
	case IdentityCustom:
		id.userAgent, id.headers = userAgent, headers
	default:
		id.userAgent = p.userAgent
	}
	return id
}

// resolveIdentity applies the precedence order: the indexer's override for
// the purpose, then the global purpose and media value, then ether. A grab
// value of "same" follows the same-media search value. fellBack is set when
// a grab had no media type and therefore presented ether.
func resolveIdentity(policy config.ClientIdentity, ix config.Indexer, purpose identityPurpose, media identityMedia, version string) (resolvedIdentity, bool) {
	if media == mediaUnknown {
		// A grab without a recorded media type presents ether and logs it:
		// guessing TV or movie could re-identify the wrong owner policy.
		return presetIdentity(IdentityEther, version, "", nil), purpose == purposeGrab
	}
	indexerName := ix.SearchIdentity
	if purpose == purposeGrab {
		indexerName = ix.GrabIdentity
	}
	name := indexerName
	if name == "" {
		if purpose == purposeGrab {
			switch media {
			case mediaTV:
				name = policy.GrabTV
			case mediaMovie:
				name = policy.GrabMovies
			}
			if name == "" || name == config.IdentitySame {
				name = searchValue(policy, media)
			}
		} else {
			name = searchValue(policy, media)
		}
	}
	if name == "" {
		return presetIdentity(IdentityEther, version, "", nil), false
	}
	userAgent, headers := policy.UserAgent, policy.Headers
	if indexerName == IdentityCustom {
		// The indexer's own custom pair wins over the global pair.
		userAgent, headers = ix.UserAgent, ix.Headers
	}
	return presetIdentity(name, version, userAgent, headers), false
}

func searchValue(policy config.ClientIdentity, media identityMedia) string {
	switch media {
	case mediaTV:
		return policy.SearchTV
	case mediaMovie:
		return policy.SearchMovies
	}
	return ""
}

// personaQueryOrder is the stable key order a persona client sends: the
// common Newznab parameters first, then identifiers, then the text query.
var personaQueryOrder = []string{"t", "cat", "extended", "apikey", "offset", "limit", "imdbid", "tmdbid", "tvdbid", "season", "ep", "q"}

// queryOrder returns the parameter order for a profile; nil keeps the
// canonical encoding Ether has always sent.
func queryOrder(p queryProfile) []string {
	switch p {
	case profileServarr, profileProwlarr:
		return personaQueryOrder
	}
	return nil
}

// encodeOrdered renders q with the listed keys first in that order, then
// the remaining keys sorted. Values are escaped the way url.Values.Encode
// escapes them, so only the order differs.
func encodeOrdered(q url.Values, order []string) string {
	if len(order) == 0 {
		return q.Encode()
	}
	var b strings.Builder
	written := make(map[string]bool, len(q))
	write := func(k string) {
		for _, v := range q[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
		written[k] = true
	}
	for _, k := range order {
		if _, ok := q[k]; ok {
			write(k)
		}
	}
	rest := make([]string, 0, len(q))
	for k := range q {
		if !written[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		write(k)
	}
	return b.String()
}

// writeIdentityHash adds a resolved identity's name, user agent, profile and
// sorted headers to a cache key, so a policy or custom-header change cannot
// reuse results fetched under another identity.
func writeIdentityHash(h hash.Hash, ident resolvedIdentity) {
	fmt.Fprintf(h, "%s\x00%s\x00%d", ident.name, ident.userAgent, ident.profile)
	keys := make([]string, 0, len(ident.headers))
	for k := range ident.headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, ident.headers[k])
	}
}

// categoryMedia maps a Newznab category to the media a feed item belongs to:
// 2000-2999 is movies, 5000-5999 is TV. Other categories (books, games) do
// not pick a side.
func categoryMedia(category int) core.MediaType {
	switch {
	case category >= 2000 && category <= 2999:
		return core.TypeMovie
	case category >= 5000 && category <= 5999:
		return core.TypeEpisode
	}
	return ""
}

// mediaType maps a policy side back to the release media type; the mixed
// feed leaves it to the item category.
func mediaType(m identityMedia) core.MediaType {
	switch m {
	case mediaTV:
		return core.TypeEpisode
	case mediaMovie:
		return core.TypeMovie
	}
	return ""
}

// mediaOf maps a search request's media type to the policy value. Episodes,
// seasons and shows are TV; anything else is unknown.
func mediaOf(t core.MediaType) identityMedia {
	switch t {
	case core.TypeMovie:
		return mediaMovie
	case core.TypeShow, core.TypeSeason, core.TypeEpisode:
		return mediaTV
	}
	return mediaUnknown
}

// EffectiveSearchIdentity is the preset an indexer's searches resolve to for
// one media side. movie selects the movie policy; otherwise TV.
func EffectiveSearchIdentity(policy config.ClientIdentity, ix config.Indexer, movie bool) string {
	return effectiveIdentityName(policy, ix, purposeSearch, mediaFor(movie))
}

// EffectiveGrabIdentity is the preset an indexer's grabs resolve to for one
// media side; unknown media falls back to ether.
func EffectiveGrabIdentity(policy config.ClientIdentity, ix config.Indexer, movie bool) string {
	return effectiveIdentityName(policy, ix, purposeGrab, mediaFor(movie))
}

func mediaFor(movie bool) identityMedia {
	if movie {
		return mediaMovie
	}
	return mediaTV
}

func effectiveIdentityName(policy config.ClientIdentity, ix config.Indexer, purpose identityPurpose, media identityMedia) string {
	id, _ := resolveIdentity(policy, ix, purpose, media, "")
	return id.name
}

// IdentitySummary is the one-line policy description the support pack shows:
// the global purpose/media values and each indexer's effective identity.
// Preset names only; custom header values and keys never appear.
func IdentitySummary(policy config.ClientIdentity, indexers []config.Indexer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "global: TV search %s, movie search %s, TV grab %s, movie grab %s",
		EffectiveSearchIdentity(policy, config.Indexer{}, false),
		EffectiveSearchIdentity(policy, config.Indexer{}, true),
		EffectiveGrabIdentity(policy, config.Indexer{}, false),
		EffectiveGrabIdentity(policy, config.Indexer{}, true))
	for _, ix := range indexers {
		search := "inherit"
		if ix.SearchIdentity != "" {
			search = ix.SearchIdentity
		}
		grab := "inherit"
		if ix.GrabIdentity != "" {
			grab = ix.GrabIdentity
		}
		name := ix.Name
		if name == "" {
			name = ix.ID
		}
		fmt.Fprintf(&b, "; %s search %s (TV %s, movies %s), grab %s (TV %s, movies %s)",
			name, search,
			EffectiveSearchIdentity(policy, ix, false), EffectiveSearchIdentity(policy, ix, true),
			grab,
			EffectiveGrabIdentity(policy, ix, false), EffectiveGrabIdentity(policy, ix, true))
	}
	return b.String()
}

// ValidateIdentityUserAgent checks a custom user agent. Empty is rejected:
// a custom identity without one would send the Go default.
func ValidateIdentityUserAgent(userAgent string) error {
	if strings.TrimSpace(userAgent) == "" {
		return fmt.Errorf("a custom identity needs a user agent")
	}
	return validHeaderValue(userAgent)
}

// ValidateIdentityHeaders checks a custom header set: RFC 7230 names, no
// control characters in values, no hop-by-hop or framing headers, and a
// bounded total size.
func ValidateIdentityHeaders(headers map[string]string) error {
	total := 0
	for k, v := range headers {
		if !validHeaderName(k) {
			return fmt.Errorf("header %q: invalid name", k)
		}
		if reservedHeader(k) {
			return fmt.Errorf("header %q: cannot be set by a custom identity", k)
		}
		if err := validHeaderValue(v); err != nil {
			return fmt.Errorf("header %q: %w", k, err)
		}
		total += len(k) + len(v)
	}
	if total > maxCustomHeaderBytes {
		return fmt.Errorf("custom identity headers exceed %d bytes", maxCustomHeaderBytes)
	}
	return nil
}

func validHeaderValue(v string) error {
	for _, r := range v {
		if r == '\r' || r == '\n' || r == 0 || (r < 0x20 && r != '\t') || r == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

// validHeaderName reports whether name is an RFC 7230 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

// reservedHeader rejects hop-by-hop and framing headers: a custom identity
// may not change how the request itself is framed or proxied.
func reservedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Host", "Connection", "Content-Length", "Transfer-Encoding",
		"Proxy-Authorization", "Proxy-Connection", "Te", "Trailer", "Upgrade", "Keep-Alive":
		return true
	}
	return false
}
