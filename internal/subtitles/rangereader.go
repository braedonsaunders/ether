package subtitles

import (
	"context"
	"errors"
	"io"

	"github.com/braedonsaunders/ether/internal/stream"
)

const rangeMaxSegments = 256

var errRangeBudget = errors.New("subtitle index read budget exhausted")

type rangeSeg struct{ off, n int64 }

// rangeReader is a segment-budgeted ReaderAt over a release, using
// stream.MetadataRange so index reads do not pull the file or fight
// playback read-ahead.
type rangeReader struct {
	ctx  context.Context
	src  io.ReaderAt
	size int64
	seen map[rangeSeg]bool
	segs int
}

func newRangeReader(ctx context.Context, src io.ReaderAt, size int64) *rangeReader {
	if ctx == nil {
		ctx = context.Background()
	}
	return &rangeReader{ctx: ctx, src: src, size: size, seen: map[rangeSeg]bool{}}
}

func (r *rangeReader) read(off int64, n int) ([]byte, error) {
	if r.ctx.Err() != nil {
		return nil, r.ctx.Err()
	}
	if off < 0 || n < 0 || (r.size > 0 && off > r.size) {
		return nil, io.ErrUnexpectedEOF
	}
	if n == 0 {
		return []byte{}, nil
	}
	if r.size > 0 && off+int64(n) > r.size {
		n = int(r.size - off)
	}
	if n <= 0 {
		return nil, io.EOF
	}
	key := rangeSeg{off, int64(n)}
	if !r.seen[key] {
		if r.segs >= rangeMaxSegments {
			return nil, errRangeBudget
		}
		r.seen[key] = true
		r.segs++
	}
	b, err := stream.IndexRange(r.ctx, r.src, off, n)
	if err != nil && len(b) == 0 {
		return nil, err
	}
	return b, nil
}
