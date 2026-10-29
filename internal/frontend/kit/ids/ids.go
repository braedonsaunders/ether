// Package ids provides stable, stateless protocol identifiers.
package ids

import (
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
)

var ErrInvalid = errors.New("invalid identifier")

func validKind(k Kind) bool { return k >= Item && k <= Playlist }

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
type Codecs struct{}

func (Codecs) GUID(kind Kind, id int64) (string, error)         { return GUID(kind, id) }
func (Codecs) ParseGUID(kind Kind, value string) (int64, error) { return ParseGUID(kind, value) }
func (Codecs) Decimal(id int64) string                          { return Decimal(id) }
func (Codecs) ParseDecimal(value string) (int64, error)         { return ParseDecimal(value) }
