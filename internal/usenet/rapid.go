//go:build (!cgo && goexperiment.simd && (amd64 || arm64)) || (cgo && !android && !ios && (((darwin || linux) && (amd64 || arm64)) || (windows && amd64)))

package usenet

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/braedonsaunders/ether/internal/yenc"
	"github.com/mnightingale/rapidyenc"
)

// The public decoder owns framing, payload decoding and CRC. This guard only
// checks Ether's limits and records the metadata not exposed by upstream.
// Existing caller buffers retain the portable decoder's exact reuse contract.
func decodeBody(br *BodyReader, opt *yenc.DecodeOptions) (*yenc.Part, error) {
	if rapidyenc.DecodeKernel() == "generic" || (opt != nil && cap(opt.Buffer) > 0) {
		return yenc.DecodeLines(br, opt)
	}
	maxSize := int64(yenc.DefaultMaxSize)
	if opt != nil && opt.MaxSize > 0 {
		maxSize = opt.MaxSize
	}
	maxSize = min(maxSize, int64(int(^uint(0)>>1))-1024)
	state := wireDecoderPool.Get().(*wireDecoder)
	state.input = guardedBody{body: br, maxSize: maxSize, tail: append(state.tail[:0], '\n'), last: '\n'}
	if maxSize <= (int64(^uint64(0)>>1)-yenc.MaxPreambleSize)/6 {
		state.input.budget = maxSize*6 + yenc.MaxPreambleSize
	} else {
		state.input.budget = int64(^uint64(0) >> 1)
	}
	response, err := nextGuarded(state.decoder)
	g := &state.input
	p := &g.part
	if response != nil {
		p.Data = response.Data
	}
	// Copy the result before clearing pooled state. Never keep media buffers in
	// the pool, and discard upstream's private read buffer on incomplete input.
	result := *p
	guardErr := g.err
	complete := g.terminated && guardErr == nil && len(g.pending) == 0 && len(g.suffix) == 0 &&
		(err == nil || errors.Is(err, rapidyenc.ErrCrcMismatch) || errors.Is(err, rapidyenc.ErrDataCorruption))
	began, ended := g.began, g.ended
	state.tail = g.tail[:0]
	state.input = guardedBody{}
	if complete {
		wireDecoderPool.Put(state)
	}
	if guardErr != nil {
		return &result, guardErr
	}
	if !began {
		return nil, yenc.ErrNoHeader
	}
	if !ended {
		return &result, yenc.ErrTruncated
	}
	if err != nil && !errors.Is(err, rapidyenc.ErrCrcMismatch) && !errors.Is(err, rapidyenc.ErrDataCorruption) {
		return &result, err
	}
	if response == nil {
		return &result, yenc.ErrTruncated
	}
	// Upstream parsed the same headers independently; any disagreement means the
	// stream was ambiguous (embedded control lines), so refuse it.
	m := &response.Metadata
	h := &result.Header
	if m.FileSize != h.Size || m.PartSize != h.PartSize() || m.BytesProduced != int64(len(result.Data)) ||
		(h.Multipart && (m.Offset+1 != h.Begin || m.PartNumber != int64(h.Part))) || m.EndSize != result.Trailer.Size {
		return &result, yenc.ErrMalformed
	}
	return &result, yenc.VerifyChecksum(&result, opt != nil && opt.SkipCRC, response.Metadata.CRC)
}

type wireDecoder struct {
	input   guardedBody
	decoder *rapidyenc.Decoder
	tail    []byte
}

var wireDecoderPool = sync.Pool{New: func() any {
	s := &wireDecoder{}
	s.decoder = rapidyenc.NewDecoder(&s.input, rapidyenc.WithStatusLineAlreadyRead(), rapidyenc.WithDataFunc(func() []byte {
		// The guard validated this hint before forwarding its first data line.
		return make([]byte, 0, max(1024, min(s.input.maxSize, s.input.part.Header.PartSize())+64))
	}))
	return s
}}

type guardedBody struct {
	body                      *BodyReader
	part                      yenc.Part
	maxSize, budget, wireData int64
	preamble                  int
	began, first, ended       bool
	terminated, lfSeen        bool
	last                      byte
	tail                      []byte
	pending                   []byte
	suffix                    string
	err                       error
}

func (g *guardedBody) Read(dst []byte) (int, error) {
	n := 0
	for n < len(dst) {
		if len(g.pending) > 0 {
			copied := copy(dst[n:], g.pending)
			n += copied
			g.pending = g.pending[copied:]
			continue
		}
		if len(g.suffix) > 0 {
			copied := copy(dst[n:], g.suffix)
			n += copied
			g.suffix = g.suffix[copied:]
			continue
		}
		if g.err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, g.err
		}
		if copied := g.readBulk(dst[n:]); copied > 0 {
			n += copied
			continue
		}
		if g.err != nil {
			continue
		}
		line, err := g.body.readBodyLine(true)
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if g.body.done {
			g.terminated = true
			g.queueLine(line)
			continue
		}
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		// Dot unstuff only the view used for validation. Forward original bytes.
		if len(content) > 0 && content[0] == '.' {
			content = content[1:]
		}
		if g.ended {
			continue
		} // Drain signatures without feeding them to upstream.
		if len(content) > yenc.MaxLineSize {
			g.err = yenc.ErrTooLarge
			continue
		}
		if !g.began {
			if !bytes.HasPrefix(content, []byte("=ybegin ")) {
				g.preamble += len(content) + 2
				if g.preamble > yenc.MaxPreambleSize {
					g.err = yenc.ErrTooLarge
				}
				continue // Do not let upstream detect UU in a yEnc preamble.
			}
			g.part.Header, g.err = yenc.ParseBegin(content)
			if g.err != nil {
				continue
			}
			if !wireIntMatches(content, "size", g.part.Header.Size, false) || !wireIntMatches(content, "part", int64(g.part.Header.Part), true) {
				g.err = yenc.ErrMalformed
				continue
			}
			// A nonmultipart header can trigger upstream allocation even if the
			// next line is the terminator. Reject its hint before forwarding it.
			if !bytes.Contains(content, []byte(" part=")) && g.part.Header.PartSize() > g.maxSize {
				g.err = yenc.ErrTooLarge
				continue
			}
			g.began, g.first = true, true
		} else {
			g.budget -= int64(len(content)) + 2
			if g.budget < 0 {
				g.err = yenc.ErrTooLarge
				continue
			}
			switch {
			case g.first && bytes.HasPrefix(content, []byte("=ypart ")):
				g.err = yenc.ParsePart(content, &g.part.Header)
				if g.err == nil && (!wireIntMatches(content, "begin", g.part.Header.Begin, false) || !wireIntMatches(content, "end", g.part.Header.End, false)) {
					g.err = yenc.ErrMalformed
				}
				g.first = false
			case bytes.HasPrefix(content, []byte("=yend")):
				g.first = false
				g.part.Trailer, g.err = yenc.ParseEnd(content)
				g.ended = true
			default:
				if bytes.HasPrefix(content, []byte("=y")) {
					g.err = yenc.ErrMalformed
					continue
				}
				g.first = false
				// Avoid a second scan of the payload. Decoded bytes never exceed wire
				// bytes, and valid escaping at most doubles them, so bounding wire
				// bytes bounds upstream's output. Exact size is checked after decode.
				g.wireData += int64(len(content))
				if g.wireData > 2*g.part.Header.PartSize()+64 {
					g.err = yenc.ErrSizeMismatch
					continue
				}
				// An odd trailing run of '=' leaves a dangling escape.
				trail := len(content)
				for trail > 0 && content[trail-1] == '=' {
					trail--
				}
				if (len(content)-trail)&1 == 1 {
					g.err = yenc.ErrMalformed
				}
			}
		}
		if !g.first && g.part.Header.PartSize() > g.maxSize {
			g.err = yenc.ErrTooLarge
		}
		if g.err == nil {
			g.queueLine(line)
		}
	}
	return n, nil
}

// NNTP uses CRLF; retain BodyReader's tolerance for LF-only servers without
// changing headers, dot-stuffing, payload bytes or the actual terminator.
func (g *guardedBody) queueLine(line []byte) {
	g.pending = line
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		g.lfSeen = true
		g.pending = bytes.TrimSuffix(line, []byte("\n"))
		g.suffix = "\r\n"
	}
}

// Upstream uses the first literal lowercase field; Ether's metadata parser
// accepts case variants and uses the last duplicate. Reject disagreements
// before forwarding allocation hints, instead of rewriting the header.
func wireIntMatches(line []byte, key string, want int64, optional bool) bool {
	_, value, found := bytes.Cut(line, []byte(" "+key+"="))
	if !found {
		return optional && want == 0
	}
	if end := bytes.IndexAny(value, "\x00 \r\n"); end >= 0 {
		value = value[:end]
	}
	got, err := strconv.ParseInt(string(value), 10, 64)
	return err == nil && got == want
}

const bulkTailSize = 8 << 10

// readBulk hands upstream whole buffered lines in one copy once the header
// lines are validated. Upstream does the framing and decoding; this only keeps
// the connection aligned: it never forwards past the NNTP terminator, always
// stops on a line boundary, keeps a bounded wire budget, and remembers the
// tail so the =yend trailer can be parsed when the terminator is reached.
func (g *guardedBody) readBulk(dst []byte) int {
	if !g.began || g.first || g.ended || g.lfSeen || g.err != nil || g.terminated || g.body.done || len(dst) < 512 {
		return 0
	}
	r := g.body.c.r
	if r.Buffered() == 0 {
		if _, err := r.Peek(1); err != nil {
			return 0 // the line path reports it with its usual handling
		}
	}
	buf, _ := r.Peek(min(r.Buffered(), len(dst)))
	end := bytes.LastIndexByte(buf, '\n') + 1
	if end == 0 {
		return 0
	}
	chunk := buf[:end]
	prev := g.last
	for off := 0; ; {
		i := bytes.IndexByte(chunk[off:], '.')
		if i < 0 {
			break
		}
		p := off + i
		if p > 0 {
			prev = chunk[p-1]
		}
		off = p + 1
		if prev != '\n' {
			continue
		}
		if len(chunk)-p >= 3 && chunk[p+1] == '\r' && chunk[p+2] == '\n' {
			chunk = chunk[:p+3]
			g.terminated = true
			break
		}
		if chunk[p+1] == '\n' { // LF-only terminator: not the CRLF framing upstream needs
			if p == 0 {
				g.err = yenc.ErrMalformed
				return 0
			}
			chunk = chunk[:p]
			break
		}
	}
	if int64(len(chunk)) > g.budget || g.wireData+int64(len(chunk)) > 2*g.part.Header.PartSize()+64+yenc.MaxPreambleSize {
		g.err = yenc.ErrSizeMismatch
		return 0
	}
	g.budget -= int64(len(chunk))
	g.wireData += int64(len(chunk))
	g.last = chunk[len(chunk)-1]
	if len(chunk) >= bulkTailSize {
		g.tail = append(g.tail[:0], chunk[len(chunk)-bulkTailSize:]...)
	} else {
		if over := len(g.tail) + len(chunk) - bulkTailSize; over > 0 {
			g.tail = g.tail[:copy(g.tail, g.tail[over:])]
		}
		g.tail = append(g.tail, chunk...)
	}
	copy(dst, chunk)
	r.Discard(len(chunk))
	if g.terminated {
		g.body.finish(nil)
		i := bytes.Index(g.tail, []byte("\n=yend"))
		if i < 0 {
			return len(chunk) // no trailer: reported as truncated
		}
		line := g.tail[i+1:]
		if j := bytes.IndexByte(line, '\r'); j >= 0 {
			line = line[:j]
		}
		g.part.Trailer, g.err = yenc.ParseEnd(line)
		g.ended = g.err == nil
	}
	return len(chunk)
}

// nextGuarded keeps a parser panic on hostile input from taking down the
// process. The state is not returned to the pool because the decoder's
// internal buffer is undefined after a panic.
func nextGuarded(d *rapidyenc.Decoder) (resp *rapidyenc.Response, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp, err = nil, fmt.Errorf("%w: rapidyenc parser panic: %v", yenc.ErrMalformed, r)
		}
	}()
	return d.Next()
}
