package handles

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func FuzzHandleCodec(f *testing.F) {
	f.Add("grant_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	f.Add("grant_invalid")
	f.Fuzz(func(t *testing.T, token string) {
		for _, mode := range []HashInput{HashRaw, HashWire} {
			c, _ := NewCodec(bytes.NewReader(make([]byte, 32)), "grant_", mode)
			digest, ok := c.Hash(token)
			if ok {
				if len(digest) != 64 {
					t.Fatal("digest length")
				}
				if _, ok := c.Hash(token + "="); ok {
					t.Fatal("noncanonical accepted")
				}
			}
		}
	})
}
