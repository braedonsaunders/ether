//go:build !cgo && goexperiment.simd && (amd64 || arm64) && !(windows && arm64)

package usenet

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/braedonsaunders/ether/internal/yenc"
	"github.com/mnightingale/rapidyenc"
)

func TestRapidWireBoundary(t *testing.T) {
	t.Logf("RapidYenc kernel: %s", rapidyenc.DecodeKernel())
	data := randData(91, 4096)
	article, err := yenc.EncodeBytes(data, yenc.EncodeOptions{Name: "wire.bin", Part: 2, Total: 3, Begin: 4097, FileSize: 12288})
	if err != nil {
		t.Fatal(err)
	}
	// EncodeBytes already emits canonical CRLF; add the real NNTP terminator
	// and a subsequent response that must stay in the connection buffer.
	wire := append(append(article, []byte("signature\r\n.\r\n")...), []byte("223 next\r\n")...)
	c := &Conn{r: bufio.NewReader(bytes.NewReader(wire))}
	br := &BodyReader{c: c, end: func(err error) error { return err }}
	p, err := decodeBody(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data, data) || p.Header.Begin != 4097 || p.Header.Part != 2 {
		t.Fatalf("part: %+v", p)
	}
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := c.r.ReadString('\n')
	if err != nil || next != "223 next\r\n" {
		t.Fatalf("next %q: %v", next, err)
	}
	if !br.done {
		t.Fatal("terminator not consumed")
	}
}

func TestRapidWireLimitsAndCRC(t *testing.T) {
	data := randData(92, 4096)
	article, _ := yenc.EncodeBytes(data, yenc.EncodeOptions{Name: "limits.bin"})
	for _, tc := range []struct {
		name    string
		article []byte
		opt     yenc.DecodeOptions
		want    error
	}{
		{"header without data", []byte("=ybegin line=128 size=1048576 name=x\r\n"), yenc.DecodeOptions{MaxSize: 1}, yenc.ErrTooLarge},
		{"limit", article, yenc.DecodeOptions{MaxSize: 4095}, yenc.ErrTooLarge},
		{"dishonest size", bytes.Replace(article, []byte("size=4096"), []byte("size=2048"), 1), yenc.DecodeOptions{}, yenc.ErrSizeMismatch},
		{"missing trailer", article[:bytes.Index(article, []byte("=yend"))], yenc.DecodeOptions{}, yenc.ErrTruncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "header without data" && rapidyenc.DecodeKernel() == "generic" {
				t.Skip("early hint validation belongs to the RapidYenc guard")
			}

			c := &Conn{r: bufio.NewReader(io.MultiReader(bytes.NewReader(tc.article), bytes.NewReader([]byte(".\r\n223 next\r\n"))))}
			br := &BodyReader{c: c, end: func(err error) error { return err }}
			_, err := decodeBody(br, &tc.opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if err := br.Close(); err != nil {
				t.Fatal(err)
			}
			if err := br.Close(); err != nil {
				t.Fatal(err)
			}
			next, _ := c.r.ReadString('\n')
			if next != "223 next\r\n" {
				t.Fatalf("next %q", next)
			}
		})
	}
}

func TestRapidWireReuseAndMetadata(t *testing.T) {
	data := randData(93, 4096)
	article, _ := yenc.EncodeBytes(data, yenc.EncodeOptions{Name: "large-file", Part: 2, Total: 1000, Begin: 4097, FileSize: 1 << 32})
	for _, buffer := range [][]byte{nil, make([]byte, 0, len(data))} {
		for i := 0; i < 8; i++ {
			c := &Conn{r: bufio.NewReader(io.MultiReader(bytes.NewReader(article), bytes.NewReader([]byte(".\r\n"))))}
			br := &BodyReader{c: c, end: func(err error) error { return err }}
			p, err := decodeBody(br, &yenc.DecodeOptions{MaxSize: 8192, Buffer: buffer})
			if err != nil || !bytes.Equal(p.Data, data) {
				t.Fatalf("iteration %d: %v", i, err)
			}
			if p.Header.Size != 1<<32 || p.Header.Begin != 4097 || p.Header.Line == 0 || !p.Trailer.HasPartCRC {
				t.Fatalf("metadata %+v", p)
			}
			if buffer != nil && &p.Data[0] != &buffer[:cap(buffer)][0] {
				t.Fatal("buffer not reused")
			}
			if err := br.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRapidWireSkipCRC(t *testing.T) {
	data := randData(94, 4096)
	article, _ := yenc.EncodeBytes(data, yenc.EncodeOptions{Name: "crc.bin"})
	end := bytes.Index(article, []byte("=yend"))
	article = append(article[:end], []byte("=yend size=4096 crc32=00000000\r\n.\r\n")...)
	for _, skip := range []bool{false, true} {
		c := &Conn{r: bufio.NewReader(bytes.NewReader(article))}
		br := &BodyReader{c: c, end: func(err error) error { return err }}
		p, err := decodeBody(br, &yenc.DecodeOptions{SkipCRC: skip})
		if skip && err != nil || !skip && !errors.Is(err, yenc.ErrCRCMismatch) {
			t.Fatalf("skip=%v: %v", skip, err)
		}
		if p == nil || !bytes.Equal(p.Data, data) {
			t.Fatal("lost payload on CRC error")
		}
	}
}

func TestRapidWireLFAndDotStuffing(t *testing.T) {
	// Literal dots are legal encoded bytes when properly NNTP-stuffed.
	data := bytes.Repeat([]byte{4}, 4096) // byte +42 == '.'
	article, _ := yenc.EncodeBytes(data, yenc.EncodeOptions{Name: "dots.bin"})
	// Encoder normally escapes a leading dot. Construct a valid raw line with
	// dot stuffing explicitly instead, with a matching decoded checksum.
	article = bytes.Replace(article, []byte("=n"), []byte(".."), 1)
	for _, lf := range []bool{false, true} {
		wire := append(append([]byte(nil), article...), []byte(".\r\n223 next\r\n")...)
		if lf {
			wire = bytes.ReplaceAll(wire, []byte("\r\n"), []byte("\n"))
		}
		c := &Conn{r: bufio.NewReader(bytes.NewReader(wire))}
		br := &BodyReader{c: c, end: func(err error) error { return err }}
		p, err := decodeBody(br, nil)
		if err != nil || !bytes.Equal(p.Data, data) {
			t.Fatalf("lf=%v: %v", lf, err)
		}
		if err := br.Close(); err != nil {
			t.Fatal(err)
		}
		next, _ := c.r.ReadString('\n')
		if !bytes.HasPrefix([]byte(next), []byte("223 next")) {
			t.Fatalf("next %q", next)
		}
	}
}

func TestRapidWireRejectsSplitEscapeBeforeDecode(t *testing.T) {
	if rapidyenc.DecodeKernel() == "generic" {
		t.Skip("portable decoder retains split-escape compatibility")
	}
	// RapidYenc treats CR following '=' as escaped payload, unlike the portable
	// line API. Reject this noncanonical input before forwarding it to keep the
	// guard's output count a hard bound.
	wire := []byte("=ybegin line=128 size=2 name=x\r\n=\r\n=\r\n=\r\n=\r\n=yend size=2\r\n.\r\n")
	c := &Conn{r: bufio.NewReader(bytes.NewReader(wire))}
	br := &BodyReader{c: c, end: func(err error) error { return err }}
	_, err := decodeBody(br, &yenc.DecodeOptions{MaxSize: 2})
	if !errors.Is(err, yenc.ErrMalformed) {
		t.Fatalf("got %v want malformed", err)
	}
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRapidWireRejectsAmbiguousAllocationHint(t *testing.T) {
	if rapidyenc.DecodeKernel() == "generic" {
		t.Skip("portable metadata parser")
	}
	wire := []byte("=ybegin line=128 size=10485760 size=1 name=x\r\n.\r\n")
	c := &Conn{r: bufio.NewReader(bytes.NewReader(wire))}
	br := &BodyReader{c: c, end: func(err error) error { return err }}
	_, err := decodeBody(br, &yenc.DecodeOptions{MaxSize: 1})
	if !errors.Is(err, yenc.ErrMalformed) {
		t.Fatalf("got %v want malformed", err)
	}
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRapidWireRejectsUnexpectedControlHeader(t *testing.T) {
	if rapidyenc.DecodeKernel() == "generic" {
		t.Skip("portable decoder")
	}
	wire := []byte("=ybegin line=128 size=100 part=1 name=x\r\n=ybegin line=128 size=1048576 name=x\r\nA\r\n.\r\n")
	c := &Conn{r: bufio.NewReader(bytes.NewReader(wire))}
	br := &BodyReader{c: c, end: func(err error) error { return err }}
	_, err := decodeBody(br, &yenc.DecodeOptions{MaxSize: 100})
	if !errors.Is(err, yenc.ErrMalformed) {
		t.Fatalf("got %v want malformed", err)
	}
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
}
