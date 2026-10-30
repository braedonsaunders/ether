package handles

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func TestCodecWireAndRawHash(t *testing.T) {
	raw := bytes.Repeat([]byte{0x95}, 32)
	for _, mode := range []HashInput{HashWire, HashRaw} {
		c, err := NewCodec(bytes.NewReader(raw), "prefix-", mode)
		if err != nil {
			t.Fatal(err)
		}
		token, hash, err := c.Issue()
		if err != nil {
			t.Fatal(err)
		}
		if token != "prefix-"+base64.RawURLEncoding.EncodeToString(raw) {
			t.Fatal("wire mismatch")
		}
		input := raw
		if mode == HashWire {
			input = []byte(token)
		}
		sum := sha256.Sum256(input)
		if hash != hex.EncodeToString(sum[:]) {
			t.Fatal("digest mismatch")
		}
		if got, ok := c.Hash(token); !ok || got != hash {
			t.Fatal("lookup mismatch")
		}
		for _, bad := range []string{"", token + "=", "wrong-" + token, token[:len(token)-1], token + "!"} {
			if _, ok := c.Hash(bad); ok {
				t.Fatalf("accepted malformed %q", bad)
			}
		}
	}
}
func TestCodecEntropyAndConfiguration(t *testing.T) {
	if _, err := NewCodec(nil, "", HashWire); err == nil {
		t.Fatal("nil entropy accepted")
	}
	if _, err := NewCodec(bytes.NewReader(nil), "", 0); err == nil {
		t.Fatal("unknown hash mode accepted")
	}
	c, _ := NewCodec(bytes.NewReader([]byte{1}), "", HashWire)
	if token, hash, err := c.Issue(); !errors.Is(err, io.ErrUnexpectedEOF) || token != "" || hash != "" {
		t.Fatalf("entropy failure token=%q hash=%q err=%v", token, hash, err)
	}
}
func FuzzCodecHashCanonical(f *testing.F) {
	f.Add("invalid")
	f.Add(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	f.Fuzz(func(t *testing.T, s string) {
		c, _ := NewCodec(bytes.NewReader(nil), "", HashWire)
		hash, ok := c.Hash(s)
		if ok {
			b, e := base64.RawURLEncoding.Strict().DecodeString(s)
			if e != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != s || len(hash) != 64 {
				t.Fatal("invalid acceptance")
			}
		}
	})
}
