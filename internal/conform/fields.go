package conform

import (
	"regexp"
	"strconv"
	"strings"
)

// LoadBearingFields are Plex protocol-shape values clients branch on.
// Shared Compare reports mismatches; plex.Compare makes them blocking so
// Jellyfin/Emby/Silo gates are not affected. Catalog contents still differ
// between servers; those names stay listed here because a wrong type,
// count, or hub identity is a wire bug even when titles differ.
var LoadBearingFields = []string{
	"viewMode",
	"viewGroup",
	"leafCount",
	"viewedLeafCount",
	"childCount",
	"size",
	"totalSize",
	"type",
	"nocache",
	"allowSync",
	"librarySectionID",
	"hubIdentifier",
	"context",
	"style",
	"more",
	"generalDecisionCode",
	"directPlayDecisionCode",
	"transcodeDecisionCode",
	"mdeDecisionCode",
	"generalDecisionText",
	"directPlayDecisionText",
	"transcodeDecisionText",
	"mdeDecisionText",
}

// KeyShapeFields compare by identifier shape (path template or {id}),
// not the numeric IDs inside them. A shape mismatch is load-bearing.
var KeyShapeFields = []string{"key", "ratingKey"}

// VolatileFields are per-server values compared by type (and URL shape)
// only. A leading or trailing * is a wildcard. Load-bearing names win
// even when a pattern here would also match.
var VolatileFields = []string{
	"*title*", "*summary", "tagline", "*name", "machineIdentifier", "*token",
	"*timestamp", "*At", "username", "email", "*guid", "*ratingKey", "*key",
	"id", "*ID", "*Id", "*thumb", "*art", "banner", "*logo", "*theme", "*url",
	"uri", "offset", "*count", "*year", "duration", "*index", "*rating",
	"viewOffset", "*version", "platform*", "*slug", "alt", "*studio", "tag",
	"file", "uuid", "*bitrate", "width", "height", "address", "*port", "host",
	"composite", "filter", "ownerFeatures", "countryCode", "score", "*Image",
	"image", "value", "default", "location", "path", "*Color", "topLeft",
	"topRight", "bottomLeft", "bottomRight", "originallyAvailableAt",
	"contentRating", "role", "*Rating", "reason*", "*Text", "*Code", "*Size",
	"*Length", "*Offset", "*Count", "*Bitrates", "*Qualities", "*Resolutions",
	"*Sessions", "subscription*", "*features", "*Features", "*Address",
	"*Port", "minYear", "maxYear", "tagKey", "*ItemID", "playQueue*",
	"sessionKey", "throttled", "progress", "speed", "frameRate", "aspectRatio",
	"channels", "samplingRate", "bitDepth", "level", "refFrames", "codec",
	"*Codec", "profile", "*Profile", "container", "videoResolution",
	"videoFrameRate", "audioChannels", "audioChannelLayout", "chroma*",
	"color*", "language*", "languageTag", "languageCode", "displayTitle",
	"extendedDisplayTitle", "scanType", "DOVI*", "has64bitOffsets",
	"optimizedForStreaming", "selected", "forced", "hidden", "canDownload*",
	"*Lock", "loudness*", "peak", "gain", "lra", "indexes", "hearingImpaired",
	"original", "external", "userID", "collectionSort", "enumValues",
	"viewCount", "skipCount", "myPlex*", "pushNotifications",
	"transcoderActive*", "livetv", "readOnlyLibraries", "audienceRatingImage",
	"ratingImage", "primaryExtraKey", "chapterSource", "subtype", "extraType",
	"parentStudio", "grandparentStudio",
}

var (
	loadBearingSet = newSet(LoadBearingFields)
	keyShapeSet    = newSet(KeyShapeFields)
	volatileRE     = compileFieldPatterns(VolatileFields)
)

func newSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

func compileFieldPatterns(names []string) *regexp.Regexp {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		p := regexp.QuoteMeta(n)
		p = strings.ReplaceAll(p, `\*`, `.*`)
		parts = append(parts, p)
	}
	return regexp.MustCompile(`(?i)^(?:` + strings.Join(parts, "|") + `)$`)
}

// LoadBearing reports whether a schema path's leaf is a load-bearing field
// or a key/ratingKey shape. Image/Rating/Setting @type and Part @key are
// catalog decoration, not protocol branches.
func LoadBearing(path string) bool {
	leaf := leafName(path)
	if keyShapeSet[leaf] {
		return !strings.Contains(path, "/Part")
	}
	if !loadBearingSet[leaf] {
		return false
	}
	if leaf == "type" && decorativeType(path) {
		return false
	}
	return true
}

func decorativeType(path string) bool {
	return strings.Contains(path, "/Image") || strings.Contains(path, "/Rating") || strings.Contains(path, "/Setting")
}

func loadBearingLeaf(leaf string) bool {
	return loadBearingSet[leaf]
}

func keyShapeLeaf(leaf string) bool {
	return keyShapeSet[leaf]
}

func volatile(path string) bool {
	return volatileRE.MatchString(leafName(path))
}

// IdentifierShape reduces a key or ratingKey to the shape clients match on.
func IdentifierShape(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return urlShape(s)
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return "{id}"
	}
	return s
}
