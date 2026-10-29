// Package ids provides stable, stateless protocol identifiers.
package ids

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Kind separates identifiers for different entity classes.
type Kind uint64

const (
	Item Kind = iota + 1
	User
	Person
	View
	Genre
	Studio
	Session
	Playlist
	Channel
	Programme
)

var ErrInvalid = errors.New("invalid identifier")

func validKind(k Kind) bool { return k >= Item && k <= Programme }

// GUID encodes a signed identifier in a kind-specific 128-bit namespace.
func GUID(kind Kind, id int64) (string, error) {
	if !validKind(kind) {
		return "", ErrInvalid
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(kind))
	binary.BigEndian.PutUint64(b[8:], uint64(id))
	return hex.EncodeToString(b[:]), nil
}

// ParseGUID accepts canonical dashed or undashed GUIDs and verifies the namespace.
func ParseGUID(kind Kind, value string) (int64, error) {
	if !validKind(kind) {
		return 0, ErrInvalid
	}
	if len(value) == 36 {
		if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
			return 0, ErrInvalid
		}
		value = strings.ReplaceAll(value, "-", "")
	}
	if len(value) != 32 {
		return 0, ErrInvalid
	}
	b, err := hex.DecodeString(value)
	if err != nil || binary.BigEndian.Uint64(b[:8]) != uint64(kind) {
		return 0, ErrInvalid
	}
	return int64(binary.BigEndian.Uint64(b[8:])), nil
}

func RatingKeyGUID(key core.RatingKey) string { s, _ := GUID(Item, int64(key)); return s }

// IsSeries reports whether a decoded item key represents a series.
func IsSeries(key core.RatingKey) bool { return key.Kind() == core.KindShow }
func ParseRatingKeyGUID(value string) (core.RatingKey, error) {
	n, err := ParseGUID(Item, value)
	return core.RatingKey(n), err
}
func Decimal(id int64) string { return strconv.FormatInt(id, 10) }
func ParseDecimal(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || Decimal(n) != value {
		return 0, ErrInvalid
	}
	return n, nil
}

// Codecs exposes stateless codecs to a frontend without retaining state.
type CompactAllocator interface {
	Compact(context.Context, string, string, string) (int32, error)
	Resolve(context.Context, string, string, int32) (string, error)
}
type Codecs struct{ Allocator CompactAllocator }

func (c Codecs) Compact(ctx context.Context, surface, kind, ref string) (int32, error) {
	if c.Allocator == nil {
		return 0, ErrInvalid
	}
	return c.Allocator.Compact(ctx, surface, kind, ref)
}
func (c Codecs) Resolve(ctx context.Context, surface, kind string, id int32) (string, error) {
	if c.Allocator == nil {
		return "", ErrInvalid
	}
	return c.Allocator.Resolve(ctx, surface, kind, id)
}

func (Codecs) GUID(kind Kind, id int64) (string, error)         { return GUID(kind, id) }
func (Codecs) ParseGUID(kind Kind, value string) (int64, error) { return ParseGUID(kind, value) }
func (Codecs) Decimal(id int64) string                          { return Decimal(id) }
func (Codecs) ParseDecimal(value string) (int64, error)         { return ParseDecimal(value) }
func (Codecs) RatingKeyGUID(key core.RatingKey) string          { return RatingKeyGUID(key) }
func (Codecs) ParseRatingKeyGUID(value string) (core.RatingKey, error) {
	return ParseRatingKeyGUID(value)
}

// StremioID is an external TMDB or IMDb identifier with an optional episode.
// IMDb identifiers cannot be converted to RatingKey without a catalog lookup.
type StremioID struct {
	Source          string
	ID              string
	Season, Episode int
	HasEpisode      bool
}

// ParseStremio accepts tmdb:<id> or tt<digits>, optionally followed by
// :<season>:<episode>. TMDB type (movie or series) is supplied by the caller.
func ParseStremio(value string) (StremioID, error) {
	var result StremioID
	parts := strings.Split(value, ":")
	if len(parts) > 0 && parts[0] == "tmdb" {
		if len(parts) != 2 && len(parts) != 4 {
			return result, ErrInvalid
		}
		result.Source = "tmdb"
		result.ID = parts[1]
		parts = parts[2:]
		n, err := ParseDecimal(result.ID)
		if err != nil || n <= 0 {
			return StremioID{}, ErrInvalid
		}
	} else {
		if len(parts) != 1 && len(parts) != 3 {
			return result, ErrInvalid
		}
		result.Source = "imdb"
		result.ID = parts[0]
		parts = parts[1:]
		if len(result.ID) < 3 || !strings.HasPrefix(result.ID, "tt") {
			return StremioID{}, ErrInvalid
		}
		for _, c := range result.ID[2:] {
			if c < '0' || c > '9' {
				return StremioID{}, ErrInvalid
			}
		}
	}
	if len(parts) == 2 {
		season, e1 := ParseDecimal(parts[0])
		episode, e2 := ParseDecimal(parts[1])
		if e1 != nil || e2 != nil || season < 0 || season > 999 || episode < 0 || episode > 9999 {
			return StremioID{}, ErrInvalid
		}
		result.Season = int(season)
		result.Episode = int(episode)
		result.HasEpisode = true
	}
	return result, nil
}

// FormatStremio encodes an external identifier and optional episode in the
// same format accepted by ParseStremio. IMDb identifiers retain leading zeros.
func FormatStremio(id StremioID) (string, error) {
	var value string
	switch id.Source {
	case "tmdb":
		value = "tmdb:" + id.ID
	case "imdb":
		value = id.ID
	default:
		return "", ErrInvalid
	}
	if id.HasEpisode {
		value += ":" + strconv.Itoa(id.Season) + ":" + strconv.Itoa(id.Episode)
	}
	parsed, err := ParseStremio(value)
	if err != nil || parsed != id {
		return "", ErrInvalid
	}
	return value, nil
}

func (Codecs) ParseStremio(value string) (StremioID, error) { return ParseStremio(value) }
func (Codecs) FormatStremio(id StremioID) (string, error)   { return FormatStremio(id) }

// StremioTMDBKey maps a TMDB Stremio identifier to its neutral catalogue key.
// IMDb identifiers require the host's policy-checked external ID lookup.
func StremioTMDBKey(mediaType string, id StremioID) (core.RatingKey, error) {
	if id.Source != "tmdb" {
		return 0, ErrInvalid
	}
	n, err := ParseDecimal(id.ID)
	if err != nil || n <= 0 {
		return 0, ErrInvalid
	}
	switch mediaType {
	case "movie":
		if id.HasEpisode || n >= 1_000_000_000_000_000 {
			return 0, ErrInvalid
		}
		return core.MovieKey(n), nil
	case "series":
		if n >= 100_000_000 {
			return 0, ErrInvalid
		}
		if id.HasEpisode {
			if id.Season < 0 || id.Season >= 1000 || id.Episode < 0 || id.Episode >= 10000 {
				return 0, ErrInvalid
			}
			return core.EpisodeKey(n, id.Season, id.Episode), nil
		}
		return core.ShowKey(n), nil
	}
	return 0, ErrInvalid
}

// StringGUID maps opaque channel/programme IDs into distinct namespaces.
// It is deterministic but not reversible; resolution requires the host's
// viewer-filtered LiveTV lookup.
func StringGUID(kind Kind, value string) (string, error) {
	if (kind != Channel && kind != Programme) || value == "" {
		return "", ErrInvalid
	}
	h := sha256.Sum256([]byte(value))
	var out [16]byte
	binary.BigEndian.PutUint64(out[:8], uint64(kind))
	copy(out[8:], h[:8])
	return hex.EncodeToString(out[:]), nil
}
func (Codecs) StringGUID(kind Kind, value string) (string, error) { return StringGUID(kind, value) }
