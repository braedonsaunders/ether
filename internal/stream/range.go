package stream

import (
	"context"
	"fmt"
	"io"

	"github.com/braedonsaunders/ether/internal/core"
)

// MetadataRange reads [off, off+n) as container metadata: no speculative
// read-ahead when src implements core.MetadataReaderAt, and a cancelled
// ctx stops an in-flight segment read when src implements
// core.ContextReaderAt. n is capped at 4 MiB so a hot path cannot pull
// a whole release.
func MetadataRange(ctx context.Context, src io.ReaderAt, off int64, n int) ([]byte, error) {
	if src == nil {
		return nil, fmt.Errorf("stream: no source")
	}
	if off < 0 || n < 0 {
		return nil, fmt.Errorf("stream: bad range [%d+%d)", off, n)
	}
	if n == 0 {
		return []byte{}, nil
	}
	if n > 4<<20 {
		n = 4 << 20
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	var (
		got int
		err error
	)
	if mr, ok := src.(core.MetadataReaderAt); ok {
		got, err = mr.ReadMetadataAtContext(ctx, buf, off)
	} else if cr, ok := src.(core.ContextReaderAt); ok {
		got, err = cr.ReadAtContext(ctx, buf, off)
	} else {
		got, err = src.ReadAt(buf, off)
	}
	if got > 0 {
		return buf[:got], nil
	}
	if err == nil {
		err = io.EOF
	}
	return nil, err
}
