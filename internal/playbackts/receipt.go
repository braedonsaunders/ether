// Package playbackts observes bounded fixed-program transport streams. Transport
// receipts and counter normalization are not decoder or seamless splice proof.
package playbackts

import (
	"crypto/sha256"
	"errors"
	"io"

	"github.com/braedonsaunders/ether/internal/playbackbuffer"
)

var ErrUnsupported = errors.New("unsupported or unproven transport stream")

// Limits bound each allocation and the number of observations. Zero is invalid.
// Hard ceilings also prevent an accidentally unlimited caller configuration.
type Limits struct {
	MaxBytes                    int64
	MaxPESBytes, MaxAccessUnits int
}

func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxBytes <= 32<<20 && l.MaxPESBytes > 0 && l.MaxPESBytes <= 4<<20 && l.MaxAccessUnits > 0 && l.MaxAccessUnits <= 65536
}

// Receipt is immutable evidence from actual published Store bytes. Fields are
// private so callers cannot assert parsed facts or attach arbitrary identities.
type Receipt struct {
	identity playbackbuffer.RetainedIdentity
	hash     [32]byte
	facts    transport
}

func (r *Receipt) Identity() playbackbuffer.RetainedIdentity { return r.identity }
func (r *Receipt) SHA256() [32]byte                          { return r.hash }

func Observe(lease *playbackbuffer.Lease, limits Limits) (*Receipt, error) {
	id, err := lease.Identity()
	if err != nil {
		return nil, err
	}
	if !limits.valid() || id.Format != playbackbuffer.MPEGTS || id.Key.Kind != playbackbuffer.Segment || id.Size <= 0 || id.Size > limits.MaxBytes {
		return nil, ErrUnsupported
	}
	b := make([]byte, int(id.Size))
	if n, err := lease.ReadAt(b, 0); err != nil || n != len(b) {
		return nil, ErrUnsupported
	}
	facts, err := parse(b, limits)
	if err != nil {
		return nil, err
	}
	after, err := lease.Identity()
	if err != nil || after != id {
		return nil, ErrUnsupported
	}
	return &Receipt{identity: id, hash: sha256.Sum256(b), facts: facts}, nil
}

// Candidate owns uncommitted bytes. Target is an instruction, not evidence that
// a Store admitted this identity, a generation was frozen, or an encoder fenced.
type Candidate struct {
	target playbackbuffer.Key
	bytes  []byte
	facts  transport
	hash   [32]byte
}

// NewCandidate reads exactly size bytes and immediate EOF. Reader must bound its
// own blocking Read; no background goroutine or service is started.
func NewCandidate(target playbackbuffer.Key, size int64, reader io.Reader, limits Limits) (*Candidate, error) {
	if !limits.valid() || reader == nil || size <= 0 || size > limits.MaxBytes || target.Session == "" || target.Generation == 0 || target.Sequence < 0 || target.Kind != playbackbuffer.Segment {
		return nil, ErrUnsupported
	}
	b := make([]byte, int(size))
	for off := 0; off < len(b); {
		n, err := reader.Read(b[off:])
		if n < 0 || n > len(b)-off {
			return nil, ErrUnsupported
		}
		off += n
		if err != nil {
			if err == io.EOF && off == len(b) {
				break
			}
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, ErrUnsupported
	}
	facts, err := parse(b, limits)
	if err != nil {
		return nil, err
	}
	return &Candidate{target: target, bytes: b, facts: facts, hash: sha256.Sum256(b)}, nil
}
func (c *Candidate) Key() playbackbuffer.Key { return c.target }
func (c *Candidate) SHA256() [32]byte        { return c.hash }
func (c *Candidate) Size() int64             { return int64(len(c.bytes)) }
func (c *Candidate) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrUnsupported
	}
	if off >= int64(len(c.bytes)) {
		return 0, io.EOF
	}
	n := copy(p, c.bytes[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
