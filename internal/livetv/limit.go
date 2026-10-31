package livetv

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

const (
	// maxPlaylistBytes is the M3U cap, compressed or decompressed.
	maxPlaylistBytes = 64 << 20
	// maxGuideBytes is the decompressed XMLTV cap. The document is streamed
	// and never buffered whole.
	maxGuideBytes = 2 << 30
	// maxGzipRatio rejects a gzip bomb once output passes gzipRatioFloor.
	// Real XMLTV compresses far below this; a bomb does not.
	maxGzipRatio   = 100
	gzipRatioFloor = 8 << 20
)

type limitError struct{ msg string }

func (e *limitError) Error() string { return e.msg }

func limitCause(err error) error {
	var lim *limitError
	if errors.As(err, &lim) {
		return lim
	}
	return nil
}

func sizeLabel(n int64) string {
	switch n {
	case 64 << 20:
		return "64 MB"
	case 2 << 30:
		return "2 GB"
	default:
		if n > 0 && n%(1<<20) == 0 {
			return fmt.Sprintf("%d MB", n>>20)
		}
		return fmt.Sprintf("%d bytes", n)
	}
}

func limitf(kind string, n int64, decompressed bool) error {
	if decompressed {
		return &limitError{msg: fmt.Sprintf("%s exceeds the %s limit after decompression", kind, sizeLabel(n))}
	}
	return &limitError{msg: fmt.Sprintf("%s exceeds the %s limit", kind, sizeLabel(n))}
}

func bombf() error {
	return &limitError{msg: fmt.Sprintf("gzip decompression exceeded the %d:1 limit", maxGzipRatio)}
}

// hardLimit stops a stream once it passes max bytes.
type hardLimit struct {
	r       io.Reader
	n, max  int64
	kind    string
	tripped bool
}

func (h *hardLimit) err() error { return limitf(h.kind, h.max, false) }

func (h *hardLimit) Read(p []byte) (int, error) {
	if h.tripped {
		return 0, h.err()
	}
	if h.n > h.max {
		h.tripped = true
		return 0, h.err()
	}
	if h.n == h.max {
		var one [1]byte
		n, err := h.r.Read(one[:])
		if n > 0 {
			h.n += int64(n)
			h.tripped = true
			return 0, h.err()
		}
		return 0, err
	}
	if int64(len(p)) > h.max-h.n {
		p = p[:h.max-h.n]
	}
	n, err := h.r.Read(p)
	h.n += int64(n)
	return n, err
}

// overflow reports a stream that continued past max after the parser stopped.
func (h *hardLimit) overflow() error {
	if h.tripped || h.n > h.max {
		return h.err()
	}
	if h.n < h.max {
		return nil
	}
	var one [1]byte
	n, err := h.r.Read(one[:])
	if n > 0 {
		h.tripped = true
		return h.err()
	}
	if err == nil || err == io.EOF {
		return nil
	}
	return err
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// gzipGuard streams a gzip body, capping decompressed bytes and the
// expansion ratio so a bomb cannot run to the size cap.
type gzipGuard struct {
	r    *gzip.Reader
	comp *countReader
	out  int64
	max  int64
	kind string
}

func (g *gzipGuard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	g.out += int64(n)
	if g.out > g.max {
		return n, limitf(g.kind, g.max, true)
	}
	if g.comp.n > 0 && g.out > gzipRatioFloor && g.out > g.comp.n*int64(maxGzipRatio) {
		return n, bombf()
	}
	return n, err
}

func (g *gzipGuard) Close() error { return g.r.Close() }

// maybeGunzip wraps r so callers can stream either plain text or gzip.
// Detection uses the gzip magic bytes, not a Content-Encoding header.
func maybeGunzip(r io.Reader, max int64, kind string) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return &hardLimit{r: br, max: max, kind: kind}, nil
	}
	comp := &countReader{r: br}
	zr, err := gzip.NewReader(comp)
	if err != nil {
		return nil, fmt.Errorf("%s is not valid gzip: %w", kind, err)
	}
	return &gzipGuard{r: zr, comp: comp, max: max, kind: kind}, nil
}

func closeReader(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		c.Close()
	}
}

func readLimited(r io.Reader, max int64, kind string) ([]byte, error) {
	rd, err := maybeGunzip(r, max, kind)
	if err != nil {
		return nil, err
	}
	defer closeReader(rd)
	return io.ReadAll(rd)
}

// gunzipBounded transparently decompresses gzip bodies (detected by magic
// bytes), refusing output larger than max or an expansion past maxGzipRatio.
func gunzipBounded(b []byte, max int64) (string, error) {
	out, err := readLimited(bytes.NewReader(b), max, "source")
	return string(out), err
}
