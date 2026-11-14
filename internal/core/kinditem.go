package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Plugin kind item envelope and source query types (spec §2.2,
// §3.2, §4.5, §7). These types are the frozen cross-slice contract:
// plugins declare kinds with them, pluginitems stores them, and the
// protocol frontends project them. Keep edits additive.

// ItemRef is a plugin item's stable identity:
// "pk:" + pluginID + ":" + kindKey + ":" + externalID, where the
// external id never contains ':' or '/'. Ether assigns it; plugins
// only ever see their own external ids.
type ItemRef string

// NewItemRef formats the canonical ItemRef for (pluginID, kindKey, externalID).
func NewItemRef(pluginID, kindKey, externalID string) ItemRef {
	return ItemRef("pk:" + pluginID + ":" + kindKey + ":" + externalID)
}

// ValidExternalID reports whether id is a legal plugin external id
// (spec §2.2: ^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$, no ':' or '/').
func ValidExternalID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alnum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && c != '.' && c != '_' && c != '~' && c != '-' {
			return false
		}
	}
	return true
}

// Parse splits r into (pluginID, kindKey, externalID) and reports
// whether r is a well-formed ItemRef (spec §2.2).
func (r ItemRef) Parse() (pluginID, kindKey, externalID string, ok bool) {
	s, hasPrefix := strings.CutPrefix(string(r), "pk:")
	if !hasPrefix {
		return "", "", "", false
	}
	p1 := strings.IndexByte(s, ':')
	if p1 <= 0 {
		return "", "", "", false
	}
	pluginID = s[:p1]
	rest := s[p1+1:]
	p2 := strings.IndexByte(rest, ':')
	if p2 <= 0 {
		return "", "", "", false
	}
	kindKey = rest[:p2]
	externalID = rest[p2+1:]
	if strings.ContainsRune(pluginID, '/') || strings.ContainsRune(kindKey, '/') || !ValidExternalID(externalID) {
		return "", "", "", false
	}
	return pluginID, kindKey, externalID, true
}

// ParseItemRef validates s and returns it as an ItemRef.
func ParseItemRef(s string) (ItemRef, error) {
	ref := ItemRef(s)
	if _, _, _, ok := ref.Parse(); !ok {
		return "", fmt.Errorf("itemRef %q: invalid format", s)
	}
	return ref, nil
}

// Valid reports whether r is a well-formed ItemRef.
func (r ItemRef) Valid() bool {
	_, _, _, ok := r.Parse()
	return ok
}

// Plugin returns the pluginID component of r, or "" if r is invalid.
func (r ItemRef) Plugin() string {
	p, _, _, _ := r.Parse()
	return p
}

// Kind returns the kindKey component of r, or "" if r is invalid.
func (r ItemRef) Kind() string {
	_, k, _, _ := r.Parse()
	return k
}

// External returns the externalID component of r, or "" if r is invalid.
func (r ItemRef) External() string {
	_, _, e, _ := r.Parse()
	return e
}

func (r ItemRef) String() string { return string(r) }

// ItemUnavailable explains why an item cannot currently be played.
type ItemUnavailable struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// KindItem is the standardised plugin item envelope (spec §3.2).
type KindItem struct {
	Unavailable *ItemUnavailable           `json:"unavailable,omitempty"`
	Ref         ItemRef                    `json:"-"`  // assigned by Ether, never sent to the plugin
	Key         RatingKey                  `json:"-"`  // allocated PluginKey(seq), populated on store reads
	ID          string                     `json:"id"` // externalID
	Level       string                     `json:"level"`
	ParentID    string                     `json:"parentId,omitempty"`
	Index       int                        `json:"index,omitempty"` // position within parent / container leaf index
	Title       string                     `json:"title"`
	SortTitle   string                     `json:"sortTitle,omitempty"` // Ether derives it when empty
	Summary     string                     `json:"summary,omitempty"`
	Year        int                        `json:"year,omitempty"`
	Released    int64                      `json:"released,omitempty"` // unix seconds
	Rating      float64                    `json:"rating,omitempty"`
	Playable    bool                       `json:"playable,omitempty"`
	ChildCount  int                        `json:"childCount,omitempty"`
	LeafCount   int                        `json:"leafCount,omitempty"`
	Art         []KindArtRef               `json:"art,omitempty"`
	Fields      map[string]json.RawMessage `json:"fields,omitempty"` // validated against the kind declaration
	Hints       json.RawMessage            `json:"hints,omitempty"`  // opaque; stored and echoed to the resolver verbatim
	ETag        string                     `json:"etag,omitempty"`
}

// KindArtRef is one artwork reference on a KindItem.
type KindArtRef struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

// KindPage is one page of kind items.
type KindPage struct {
	Items      []KindItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"` // opaque; absent = last page
	Total      *int       `json:"total,omitempty"`      // provider claim; Ether never trusts it for its own totals
}

// KindBrowseRequest is the request body sent to a catalog_provider
// browse or search route (spec §3.1, §3.2).
type KindBrowseRequest struct {
	ParentID string              `json:"parentId,omitempty"`
	Level    string              `json:"level,omitempty"`
	Cursor   string              `json:"cursor,omitempty"`
	Limit    int                 `json:"limit,omitempty"`
	Sort     string              `json:"sort,omitempty"`
	Facets   map[string][]string `json:"facets,omitempty"`
	Query    string              `json:"query,omitempty"`
}

// KindChangePage is one delta page returned by POST /ether/v1/kinds/{kind}/changes
// during pull sync (spec §3.5).
type KindChangePage struct {
	Upserts    []KindItem `json:"upserts,omitempty"`
	Deletes    []string   `json:"deletes,omitempty"` // externalIDs
	NextCursor string     `json:"nextCursor,omitempty"`
	Complete   bool       `json:"complete"`
}

// KindMatchRequest is the request body sent to POST /ether/v1/kinds/{kind}/match (spec §7).
type KindMatchRequest struct {
	Item       KindItem          `json:"item"`
	Candidates []SourceCandidate `json:"candidates"`
	Settings   map[string]any    `json:"settings,omitempty"`
}

// KindMatchScore is one candidate score returned by a matcher (spec §7).
type KindMatchScore struct {
	CandidateRef string   `json:"candidateRef"`
	Score        float64  `json:"score"`
	Reasons      []string `json:"reasons,omitempty"`
}

// KindMatchResponse is the response from POST /ether/v1/kinds/{kind}/match (spec §7).
type KindMatchResponse struct {
	Matches []KindMatchScore `json:"matches"`
}

// SourceQuery asks Ether to find sources for a kind resolver
// (spec §4.5).
type SourceQuery struct {
	Kind  string `json:"kind"` // indexer_search|debrid_check|debrid_acquire
	Query string `json:"query,omitempty"`
	// Category accepts comma-separated Newznab IDs or standard family names.
	Category   string   `json:"category,omitempty"`
	InfoHashes []string `json:"infoHashes,omitempty"`
	InfoHash   string   `json:"infoHash,omitempty"`
	Magnet     string   `json:"magnet,omitempty"`
}

// SourceCandidate is one Ether-found release for a resolver to rank,
// carrying the release-hint fields plus the facts matching needs.
type SourceCandidate struct {
	InfoHash string `json:"infoHash,omitempty"`
	Magnet   string `json:"magnet,omitempty"`
	NZBURL   string `json:"nzbUrl,omitempty"`
	// FileIndex selects a zero-based entry in the torrent provider file list; nil uses video selection.
	FileIndex *int   `json:"fileIndex,omitempty"`
	Title     string `json:"title,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Seeders   int    `json:"seeders,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
}

// PluginLibraryKey is the library key a plugin kind owns
// (spec §1.1): "pk-" + slug(pluginID) + "-" + slug(kindKey), matching
// internal/libraries.UpsertPluginLibrary. The key is an opaque unique
// slug; ownership is always read back from the stored plugin/kind
// columns, never parsed out of the key.
func PluginLibraryKey(pluginID, kindKey string) string {
	return "pk-" + libraryKeySlug(pluginID) + "-" + libraryKeySlug(kindKey)
}

func libraryKeySlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}

// PlayableSource is one candidate way to acquire bytes for a KindItem or one of
// its parts (spec §4.1). Multiple sources are tried in order.
type PlayableSource struct {
	Path        string            `json:"path,omitempty"`
	ID          string            `json:"id"`
	Label       string            `json:"label,omitempty"`
	Pattern     string            `json:"pattern"` // "range" | "download" | "direct" | "container" | "release" | "file"
	MIME        string            `json:"mime"`
	Size        int64             `json:"size,omitempty"`
	DurationSec float64           `json:"durationSec,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	BlobRef     string            `json:"blobRef,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	MaxBytes    int64             `json:"maxBytes,omitempty"`
	ExpiresAt   int64             `json:"expiresAt,omitempty"`
	Container   *ContainerRef     `json:"container,omitempty"`
	Release     *ReleaseHint      `json:"release,omitempty"`
}

// ContainerRef describes an archive member inside one or more volume streams
// (spec §4.1, §5.3).
type ContainerRef struct {
	Format         string      `json:"format,omitempty"` // "" (sniff) | "rar" | "7z" | "zip"
	Volumes        []VolumeRef `json:"volumes"`
	Member         string      `json:"member,omitempty"`
	MemberIndex    *int        `json:"memberIndex,omitempty"`
	PasswordSecret string      `json:"passwordSecret,omitempty"`
}

// VolumeRef locates one archive volume by URL or blobcache reference (spec §4.1).
type VolumeRef struct {
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	BlobRef string            `json:"blobRef,omitempty"`
	Size    int64             `json:"size"`
}

// ReleaseHint hands an NZB, magnet, infohash, or pre-built Candidate to Ether's
// existing Usenet / debrid / torrent acquisition pipeline (spec §4.1, §4.3).
type ReleaseHint struct {
	// FileIndex overrides candidate.fileIndex; out-of-range indices fail without fallback.
	FileIndex *int             `json:"fileIndex,omitempty"`
	NZBURL    string           `json:"nzbUrl,omitempty"`
	Magnet    string           `json:"magnet,omitempty"`
	InfoHash  string           `json:"infoHash,omitempty"`
	Candidate *SourceCandidate `json:"candidate,omitempty"`
	// File selects an exact torrent path or unique basename, including non-video files.
	File           string `json:"file,omitempty"`
	FileHint       string `json:"fileHint,omitempty"`
	PasswordSecret string `json:"passwordSecret,omitempty"`
}

// KindResolveRequest is the payload sent to a plugin's kind_resolve handler
// (spec §4.1).
type KindResolveRequest struct {
	LibraryKey string         `json:"libraryKey"`
	Item       KindItem       `json:"item"`
	PartIndex  *int           `json:"partIndex,omitempty"`
	Client     map[string]any `json:"client,omitempty"`
}

// KindResolveResponse is the response returned by a plugin's kind_resolve
// handler (spec §4.1).
type KindResolveResponse struct {
	Sources []PlayableSource `json:"sources"`
	TTLSec  int              `json:"ttlSec,omitempty"`
}
