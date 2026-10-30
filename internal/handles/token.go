// Package handles provides shared mechanics for scoped opaque handles. Domain
// adapters remain responsible for authorization, storage and lifetimes.
package handles

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// HashInput preserves legacy namespaces which hash either the wire token or
// the underlying random bytes. It is not an authorization policy.
type HashInput uint8

const (
	HashWire HashInput = iota + 1
	HashRaw
)

type Codec struct {
	random    io.Reader
	prefix    string
	hashInput HashInput
}

func NewCodec(random io.Reader, prefix string, input HashInput) (*Codec, error) {
	if random == nil || (input != HashWire && input != HashRaw) {
		return nil, errors.New("handles: invalid codec dependencies")
	}
	return &Codec{random: random, prefix: prefix, hashInput: input}, nil
}

// Issue returns an unguessable 256-bit token and its storage digest.
func (c *Codec) Issue() (string, string, error) {
	var raw [32]byte
	if _, err := io.ReadFull(c.random, raw[:]); err != nil {
		return "", "", err
	}
	token := c.prefix + base64.RawURLEncoding.EncodeToString(raw[:])
	hash, _ := c.Hash(token)
	return token, hash, nil
}

// Hash accepts only the canonical encoding and exact configured namespace.
func (c *Codec) Hash(token string) (string, bool) {
	if !strings.HasPrefix(token, c.prefix) {
		return "", false
	}
	encoded := strings.TrimPrefix(token, c.prefix)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return "", false
	}
	input := raw
	if c.hashInput == HashWire {
		input = []byte(token)
	}
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:]), true
}
