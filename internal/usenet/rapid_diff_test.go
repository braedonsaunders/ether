//go:build !cgo && goexperiment.simd && (amd64 || arm64) && !(windows && arm64)

package usenet

import (
	"bufio"
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/braedonsaunders/ether/internal/yenc"
)

func diffMutate(rng *rand.Rand, in []byte) []byte {
	b := append([]byte(nil), in...)
	for k := rng.Intn(4); k >= 0; k-- {
		if len(b) == 0 {
			break
		}
		i := rng.Intn(len(b))
		switch rng.Intn(9) {
		case 0:
			b[i] = byte(rng.Intn(256))
		case 1:
			b = append(b[:i], b[i+1:]...)
		case 2:
			b = append(b[:i], append([]byte{byte(rng.Intn(256))}, b[i:]...)...)
		case 3:
			b = b[:i]
		case 4:
			ins := [][]byte{[]byte("\r\n=yend size=1\r\n"), []byte("\r\n.\r\n"), []byte("\r\n..x\r\n"), []byte("\r\n=ybegin size=9 line=128 name=z\r\n"), []byte("=\r\n"), []byte("\r\n.\n"), []byte("\n"), []byte("\r\n=ypart begin=1 end=2\r\n")}
			x := ins[rng.Intn(len(ins))]
			b = append(b[:i], append(append([]byte(nil), x...), b[i:]...)...)
		case 5:
			if j := bytes.IndexByte(b[i:], '\r'); j >= 0 {
				b = append(b[:i+j], b[i+j+1:]...)
			}
		case 6:
			b = bytes.Replace(b, []byte("size="), []byte("size=99"), 1)
		case 7:
			b = bytes.Replace(b, []byte("crc32="), []byte("crc32=0"), 1)
		case 8:
			b[i] = '='
		}
	}
	return b
}

func diffDecode(wire []byte, rapid bool, opt *yenc.DecodeOptions) (*yenc.Part, error, string) {
	c := &Conn{r: bufio.NewReaderSize(bytes.NewReader(wire), 4096)}
	br := &BodyReader{c: c, end: func(err error) error { return err }}
	var p *yenc.Part
	var err error
	if rapid {
		p, err = decodeBody(br, opt)
	} else {
		p, err = yenc.DecodeLines(br, opt)
	}
	br.Close()
	next, _ := c.r.ReadString('\n')
	return p, err, next
}

// Differential: the RapidYenc path must never accept CRC-protected bytes that
// differ from the original, must not panic, and must leave the connection at
// the same place as the portable decoder.
func TestRapidDifferentialMutations(t *testing.T) {
	for _, seed := range []int64{7, 107, 207, 307, 407, 507} {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			testRapidDifferentialMutations(t, seed)
		})
	}
}

func testRapidDifferentialMutations(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	var accepted, onlyRapid, rapidRejects int
	for iter := range 30000 {
		size := []int{300, 2000, 20000}[rng.Intn(3)]
		data := randData(int64(iter), size)
		opts := yenc.EncodeOptions{Name: "m.bin", LineLength: []int{64, 128, 256}[rng.Intn(3)]}
		if rng.Intn(2) == 0 {
			opts.Part, opts.Total, opts.Begin, opts.FileSize = 2, 3, 1001, int64(size)+2000
		}
		article, err := yenc.EncodeBytes(data, opts)
		if err != nil {
			t.Fatal(err)
		}
		wire := append(diffMutate(rng, article), []byte("\r\n.\r\n223 next\r\n")...)
		opt := &yenc.DecodeOptions{MaxSize: 1 << 20}
		rp, rerr, rnext := diffDecode(wire, true, opt)
		sp, serr, snext := diffDecode(wire, false, opt)
		if rnext != snext {
			t.Fatalf("iter %d: connection misaligned rapid=%q portable=%q (rerr=%v serr=%v) wire tail=%q", iter, rnext, snext, rerr, serr, wire[max(0, len(wire)-120):])
		}
		if rerr == nil {
			accepted++
			switch {
			case serr == nil && (!bytes.Equal(rp.Data, sp.Data) || rp.Header != sp.Header):
				t.Fatalf("iter %d: different result", iter)
			case serr != nil && !bytes.Equal(rp.Data, data) && (rp.Trailer.HasCRC || rp.Trailer.HasPartCRC):
				t.Fatalf("iter %d: rapid accepted wrong bytes where portable rejected (%v) len=%d trailer=%+v\nhead=%q\ntail=%q", iter, serr, len(rp.Data), rp.Trailer, wire[:min(len(wire), 80)], wire[max(0, len(wire)-160):])
			case serr != nil:
				onlyRapid++ // includes corrupt payload whose CRC field was also mutated away
			}
		} else if serr == nil {
			rapidRejects++
		}
	}
	t.Logf("accepted=%d acceptedOnlyByRapid=%d rapidRejectedPortableAccepted=%d", accepted, onlyRapid, rapidRejects)
}

// Exercise arbitrary wire mutations in addition to the reproducible seed matrix.
// Inputs without a usable CRC can be accepted differently by the two parsers;
// compare output only when both accept, as in the deterministic test above.
func FuzzRapidDifferential(f *testing.F) {
	for _, line := range []int{64, 128, 256} {
		for _, multipart := range []bool{false, true} {
			opts := yenc.EncodeOptions{Name: "fuzz.bin", LineLength: line}
			if multipart {
				opts.Part, opts.Total, opts.Begin, opts.FileSize = 2, 3, 1001, 4000
			}
			article, err := yenc.EncodeBytes(randData(95, 2000), opts)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(article)
		}
	}
	f.Add([]byte("=ybegin line=128 size=1 name=x\r\n=\r\n=yend size=1\r\n"))
	f.Fuzz(func(t *testing.T, article []byte) {
		if len(article) > 64<<10 {
			t.Skip()
		}
		wire := append(append([]byte(nil), article...), []byte("\r\n.\r\n223 next\r\n")...)
		opt := &yenc.DecodeOptions{MaxSize: 64 << 10}
		rp, rerr, rnext := diffDecode(wire, true, opt)
		sp, serr, snext := diffDecode(wire, false, opt)
		if rnext != snext {
			t.Fatalf("connection misaligned rapid=%q portable=%q (rerr=%v serr=%v)", rnext, snext, rerr, serr)
		}
		if rerr == nil && serr == nil && (!bytes.Equal(rp.Data, sp.Data) || rp.Header != sp.Header) {
			t.Fatal("different result when both decoders accept")
		}
	})
}
