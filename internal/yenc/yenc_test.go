package yenc

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"strings"
	"testing"
	"testing/iotest"
)

func randBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestRoundTripSinglePart(t *testing.T) {
	for _, n := range []int{0, 1, 2, 127, 128, 129, 1000, 65536, 750_000} {
		for _, ll := range []int{0, 1, 2, 64, 128, 997} {
			data := randBytes(int64(n*31+ll), n)
			enc, err := EncodeBytes(data, EncodeOptions{Name: "file.bin", LineLength: ll})
			if err != nil {
				t.Fatal(err)
			}
			p, err := Decode(enc)
			if err != nil {
				t.Fatalf("n=%d ll=%d: %v", n, ll, err)
			}
			if !bytes.Equal(p.Data, data) {
				t.Fatalf("n=%d ll=%d: data mismatch", n, ll)
			}
			if p.Header.Name != "file.bin" || p.Header.Size != int64(n) || p.Header.Multipart {
				t.Fatalf("header %+v", p.Header)
			}
			if p.Header.Begin != 1 || p.Header.End != int64(n) {
				t.Fatalf("begin/end %d %d", p.Header.Begin, p.Header.End)
			}
			if !p.Trailer.HasCRC || p.Trailer.CRC32 != crc32.ChecksumIEEE(data) {
				t.Fatalf("trailer %+v", p.Trailer)
			}
		}
	}
}

func TestEveryByteValue(t *testing.T) {
	data := make([]byte, 0, 256*3)
	for rep := 0; rep < 3; rep++ {
		for i := 0; i < 256; i++ {
			data = append(data, byte(i))
		}
	}
	enc, err := EncodeBytes(data, EncodeOptions{Name: "all", LineLength: 7})
	if err != nil {
		t.Fatal(err)
	}
	// No critical characters may appear unescaped in data lines.
	lines := strings.Split(strings.TrimSuffix(string(enc), "\r\n"), "\r\n")
	for _, l := range lines[1 : len(lines)-1] {
		if strings.ContainsAny(l, "\x00\r\n") {
			t.Fatalf("raw critical char in %q", l)
		}
		if strings.HasPrefix(l, ".") || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
			t.Fatalf("unescaped leading char in %q", l)
		}
		if strings.HasSuffix(l, " ") || strings.HasSuffix(l, "\t") {
			t.Fatalf("unescaped trailing whitespace in %q", l)
		}
		if strings.HasPrefix(l, "=y") {
			t.Fatalf("data line looks like a keyword: %q", l)
		}
	}
	p, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data, data) {
		t.Fatal("mismatch")
	}
}

func TestGolden(t *testing.T) {
	// 214+42 = 0 (escaped NUL), 19+42 = '=' (escaped), 4+42 = '.' at line start (escaped),
	// 'A'-42 decodes to 'A'.
	data := []byte{4, 214, 19, 'A' - 42, 4}
	enc, err := EncodeBytes(data, EncodeOptions{Name: "g.bin", LineLength: 128})
	if err != nil {
		t.Fatal(err)
	}
	want := "=ybegin line=128 size=5 name=g.bin\r\n" +
		"=n=@=}A.\r\n" +
		"=yend size=5 crc32=" + hex32(crc32.ChecksumIEEE(data)) + "\r\n"
	if string(enc) != want {
		t.Fatalf("got\n%q\nwant\n%q", enc, want)
	}
}

func hex32(v uint32) string { return string(appendHex32(nil, v)) }

func TestMultipart(t *testing.T) {
	file := randBytes(7, 2_000_000)
	fileCRC := crc32.ChecksumIEEE(file)
	const seg = 768_000
	total := (len(file) + seg - 1) / seg
	var got []byte
	for i := 0; i < total; i++ {
		begin := i * seg
		end := min(begin+seg, len(file))
		enc, err := EncodeBytes(file[begin:end], EncodeOptions{
			Name: "movie name with spaces.mkv", Part: i + 1, Total: total,
			Begin: int64(begin + 1), FileSize: int64(len(file)), FileCRC: fileCRC, HasFileCRC: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		p, err := Decode(enc)
		if err != nil {
			t.Fatalf("part %d: %v", i+1, err)
		}
		h := p.Header
		if !h.Multipart || h.Part != i+1 || h.Total != total || h.Begin != int64(begin+1) || h.End != int64(end) || h.Size != int64(len(file)) {
			t.Fatalf("part %d header %+v", i+1, h)
		}
		if h.Name != "movie name with spaces.mkv" {
			t.Fatalf("name %q", h.Name)
		}
		if p.Trailer.Part != i+1 || !p.Trailer.HasPartCRC || !p.Trailer.HasCRC || p.Trailer.CRC32 != fileCRC {
			t.Fatalf("trailer %+v", p.Trailer)
		}
		got = append(got, p.Data...)
	}
	if !bytes.Equal(got, file) {
		t.Fatal("reassembled file mismatch")
	}
}

func TestCRCMismatch(t *testing.T) {
	data := randBytes(3, 5000)
	enc, _ := EncodeBytes(data, EncodeOptions{Name: "x", Part: 1, Total: 1, FileSize: 5000})
	// Flip a data character on the first data line (not an escape).
	lines := bytes.SplitN(enc, []byte("\r\n"), 4)
	dl := lines[2]
	for i, c := range dl {
		if c != '=' && (i == 0 || dl[i-1] != '=') && c != 'a' {
			dl[i] = 'a'
			break
		}
	}
	p, err := Decode(enc)
	if !errors.Is(err, ErrCRCMismatch) {
		t.Fatalf("err = %v, want ErrCRCMismatch", err)
	}
	if p == nil || len(p.Data) != 5000 {
		t.Fatal("expected data alongside CRC error")
	}
	if _, err := DecodeLines(&bytesLines{b: enc}, &DecodeOptions{SkipCRC: true}); err != nil {
		t.Fatalf("SkipCRC: %v", err)
	}
}

func TestSizeMismatch(t *testing.T) {
	enc := []byte("=ybegin line=128 size=4 name=a\r\n" + string([]byte{'a' + 42, 'b' + 42, 'c' + 42}) + "\r\n=yend size=4\r\n")
	if _, err := Decode(enc); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoHeaderAndTruncated(t *testing.T) {
	if _, err := Decode([]byte("hello\r\nworld\r\n")); !errors.Is(err, ErrNoHeader) {
		t.Fatalf("err = %v", err)
	}
	enc, _ := EncodeBytes(randBytes(1, 1000), EncodeOptions{Name: "x"})
	cut := enc[:bytes.LastIndex(enc, []byte("=yend"))]
	if _, err := Decode(cut); !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Decode([]byte("=ybegin line=128 name=a\r\n")); !errors.Is(err, ErrMalformed) {
		t.Fatalf("missing size: %v", err)
	}
	if _, err := Decode([]byte("=ybegin part=1 size=10 name=a\r\n=ypart begin=5 end=2\r\n=yend size=0\r\n")); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad ypart: %v", err)
	}
}

func TestTooLarge(t *testing.T) {
	enc := []byte("=ybegin line=128 size=999999999999 name=a\r\nxx\r\n=yend size=2\r\n")
	if _, err := Decode(enc); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	enc2, _ := EncodeBytes(randBytes(2, 4096), EncodeOptions{Name: "x"})
	if _, err := DecodeLines(&bytesLines{b: enc2}, &DecodeOptions{MaxSize: 1024}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestPreambleAndLFOnlyAndDanglingEscape(t *testing.T) {
	// Text before =ybegin, LF line endings, an escape split across lines,
	// trailing text after =yend.
	data := []byte{19, 'x' - 42, 'y' - 42} // 19 -> escaped '='
	body := "some preamble\n\n=ybegin line=128 size=3 name=f\n=\n}xy\n=yend size=3 crc32=" + hex32(crc32.ChecksumIEEE(data)) + "\nsignature\n"
	p, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data, data) {
		t.Fatalf("got %v", p.Data)
	}
}

func TestDecodeReaderLongLinesAndOneByteReads(t *testing.T) {
	data := randBytes(9, 200_000)
	enc, _ := EncodeBytes(data, EncodeOptions{Name: "long", LineLength: 100_000})
	p, err := DecodeReader(iotest.OneByteReader(bytes.NewReader(enc)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data, data) {
		t.Fatal("mismatch")
	}
}

func TestBufferReuse(t *testing.T) {
	data := randBytes(4, 10_000)
	enc, _ := EncodeBytes(data, EncodeOptions{Name: "r"})
	buf := make([]byte, 0, 20_000)
	p, err := DecodeLines(&bytesLines{b: enc}, &DecodeOptions{Buffer: buf})
	if err != nil {
		t.Fatal(err)
	}
	if &p.Data[0] != &buf[:1][0] {
		t.Fatal("buffer not reused")
	}
}

func TestEncodeErrors(t *testing.T) {
	if _, err := EncodeBytes([]byte("x"), EncodeOptions{}); err == nil {
		t.Fatal("expected error for empty name")
	}
	if _, err := EncodeBytes([]byte("xyz"), EncodeOptions{Name: "a", Part: 1, Begin: 2, FileSize: 3}); err == nil {
		t.Fatal("expected error for part past file size")
	}
	var w bytes.Buffer
	if err := Encode(&w, []byte("abc"), EncodeOptions{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if p, err := DecodeReader(&w, nil); err != nil || string(p.Data) != "abc" {
		t.Fatalf("%v %q", err, p.Data)
	}
}

func TestReaderError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("=ybegin line=1 size=1 name=a\r\n"), iotest.ErrReader(boom))
	if _, err := DecodeReader(r, nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	enc, _ := EncodeBytes([]byte("hello world"), EncodeOptions{Name: "f", Part: 1, Total: 1})
	f.Add(enc)
	f.Add([]byte("=ybegin line=1 size=1 name=a\r\n=\r\n=yend size=1\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = DecodeLines(&bytesLines{b: b}, &DecodeOptions{MaxSize: 1 << 20})
	})
}

func BenchmarkDecode(b *testing.B) {
	data := randBytes(42, 750_000)
	enc, _ := EncodeBytes(data, EncodeOptions{Name: "bench.bin", Part: 1, Total: 1, FileSize: int64(len(data))})
	buf := make([]byte, 0, len(data))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecodeLines(&bytesLines{b: enc}, &DecodeOptions{Buffer: buf}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLine(b *testing.B) {
	data := randBytes(43, 750_000)
	enc, _ := EncodeBytes(data, EncodeOptions{Name: "bench.bin"})
	var lines [][]byte
	for _, l := range bytes.Split(enc, []byte("\r\n")) {
		if len(l) > 0 && !bytes.HasPrefix(l, []byte("=y")) {
			lines = append(lines, l)
		}
	}
	dst := make([]byte, 1024)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		esc := false
		for _, l := range lines {
			_, esc = DecodeLine(dst, l, esc)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	data := randBytes(44, 750_000)
	buf := make([]byte, 0, 2*len(data))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var err error
		if buf, err = AppendEncode(buf[:0], data, EncodeOptions{Name: "bench.bin"}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDecodeLineLengthsMatchScalar(t *testing.T) {
	src := randBytes(11, 1031)
	for n := 0; n <= len(src); n++ {
		want, wantEsc := decodeLineScalar(src[:n], false)
		dst := make([]byte, n)
		got, gotEsc := DecodeLine(dst, src[:n], false)
		if !bytes.Equal(dst[:got], want) || gotEsc != wantEsc {
			t.Fatalf("length %d", n)
		}
	}
}

func TestUnderstatedSize(t *testing.T) {
	// Header claims 2 bytes but 300 follow: decoder must grow, then report.
	data := randBytes(12, 300)
	enc, _ := EncodeBytes(data, EncodeOptions{Name: "u"})
	enc = bytes.Replace(enc, []byte("size=300 name"), []byte("size=2 name"), 1)
	p, err := Decode(enc)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(p.Data, data) {
		t.Fatal("data mismatch")
	}
}

// decodeLineScalar is the byte-at-a-time reference DecodeLine must match.
func decodeLineScalar(src []byte, escaped bool) ([]byte, bool) {
	var out []byte
	for _, c := range src {
		switch {
		case escaped:
			out = append(out, c-106)
			escaped = false
		case c == '=':
			escaped = true
		default:
			out = append(out, c-42)
		}
	}
	return out, escaped
}

func TestDecodeLineEveryByteAtEveryLane(t *testing.T) {
	for lane := 0; lane < 9; lane++ {
		for v := 0; v < 256; v++ {
			src := bytes.Repeat([]byte{'a'}, 17)
			src[lane] = byte(v)
			for _, esc := range []bool{false, true} {
				want, wantEsc := decodeLineScalar(src, esc)
				dst := make([]byte, len(src))
				n, gotEsc := DecodeLine(dst, src, esc)
				if !bytes.Equal(dst[:n], want) || gotEsc != wantEsc {
					t.Fatalf("lane %d byte %#x escaped %v: got %x/%v want %x/%v", lane, v, esc, dst[:n], gotEsc, want, wantEsc)
				}
			}
		}
	}
}

func FuzzDecodeLine(f *testing.F) {
	f.Add([]byte("abc=}def"), false)
	f.Add([]byte("=\x00\xff=="), true)
	f.Fuzz(func(t *testing.T, src []byte, escaped bool) {
		want, wantEsc := decodeLineScalar(src, escaped)
		dst := make([]byte, len(src))
		n, gotEsc := DecodeLine(dst, src, escaped)
		if !bytes.Equal(dst[:n], want) || gotEsc != wantEsc {
			t.Fatalf("got %x/%v want %x/%v", dst[:n], gotEsc, want, wantEsc)
		}
	})
}

func TestDecodeLineInPlace(t *testing.T) {
	for _, length := range []int{0, 1, 7, 8, 9, 15, 16, 127, 128, 256, 1031} {
		original := randBytes(23, length)
		for offset := 0; offset < 9; offset++ {
			for _, escaped := range []bool{false, true} {
				want, wantEsc := decodeLineScalar(original, escaped)
				backing := make([]byte, len(original)+offset)
				copy(backing[offset:], original)
				n, gotEsc := DecodeLine(backing[:len(original)], backing[offset:], escaped)
				if !bytes.Equal(backing[:n], want) || gotEsc != wantEsc {
					t.Fatalf("length %d offset %d escaped %v", length, offset, escaped)
				}
			}
		}
	}
}

// Some uploaders randomise =ybegin size, name and total on every article of
// a post while =ypart begin/end stay truthful. When those extents run past the
// announced size the size is demonstrably the wrong field, so the extents must
// win instead of the article being rejected as malformed. (A size that is
// merely too small to be believable is caught later, against the article's
// decoded length, where that length is known.)
func TestParsePartKeepsExtentsWhenSizeContradicts(t *testing.T) {
	h, err := ParseBegin([]byte("=ybegin part=1 total=829 size=10888513 line=128 name=Q0bFusc4ted"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ParsePart([]byte("=ypart begin=1 end=200000000"), &h); err != nil {
		t.Fatalf("contradicting size rejected a good article: %v", err)
	}
	if !h.Multipart || h.Begin != 1 || h.End != 200000000 {
		t.Fatalf("extents not preserved: %+v", h)
	}
	if h.PartSize() != 200000000 {
		t.Fatalf("part size %d, want 200000000", h.PartSize())
	}
	if h.Size != 0 || h.Total != 0 || h.Name != "" {
		t.Fatalf("contradicting metadata survived: %+v", h)
	}

	// An extent beyond any plausible file is still refused, so a hostile
	// header cannot announce an absurd length.
	big, err := ParseBegin([]byte("=ybegin size=1000 name=x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ParsePart([]byte("=ypart begin=1 end=1099511627777"), &big); !errors.Is(err, ErrMalformed) {
		t.Fatalf("absurd extent accepted: %v", err)
	}
}

// A consistent multipart header keeps the metadata the extents agree with.
func TestParsePartKeepsConsistentMetadata(t *testing.T) {
	h, err := ParseBegin([]byte("=ybegin part=1 total=4 size=3000000 line=128 name=real.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ParsePart([]byte("=ypart begin=1 end=768000"), &h); err != nil {
		t.Fatal(err)
	}
	if h.Size != 3000000 || h.Total != 4 || h.Name != "real.mkv" {
		t.Fatalf("consistent metadata dropped: %+v", h)
	}
}
