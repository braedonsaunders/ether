//go:build cgo && !android && !ios && (((darwin || linux) && (amd64 || arm64)) || (windows && amd64))

package usenet

import (
	"bufio"
	"bytes"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/yenc"
	"github.com/mnightingale/rapidyenc"
)

func TestRapidCGOBackend(t *testing.T) {
	t.Logf("RapidYenc C backend kernel: %s", rapidyenc.DecodeKernel())
	if rapidyenc.DecodeKernel() == "generic" {
		t.Skip("generic CPU retains SWAR")
	}
	// This guard rejection distinguishes the RapidYenc response path from the
	// portable line decoder, which accepts the last duplicate size field.
	wire := []byte("=ybegin line=128 size=10485760 size=1 name=x\r\n.\r\n")
	c := &Conn{r: bufio.NewReader(bytes.NewReader(wire))}
	br := &BodyReader{c: c, end: func(err error) error { return err }}
	_, err := decodeBody(br, &yenc.DecodeOptions{MaxSize: 1})
	if !errors.Is(err, yenc.ErrMalformed) {
		t.Fatalf("cgo must select the guarded RapidYenc path: got %v", err)
	}
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
}
