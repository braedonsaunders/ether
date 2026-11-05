package transcode

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// The tee is always drained. Slow readers lose only their own attachment.
type continuousFanout struct {
	mu      sync.Mutex
	readers map[chan []byte]struct{}
	closed  bool
}

func newContinuousFanout() *continuousFanout {
	return &continuousFanout{readers: make(map[chan []byte]struct{})}
}
func (f *continuousFanout) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.readers) == 0 {
		return len(b), nil
	}
	p := append([]byte(nil), b...)
	for ch := range f.readers {
		select {
		case ch <- p:
		default:
			close(ch)
			delete(f.readers, ch)
		}
	}
	return len(b), nil
}
func (f *continuousFanout) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for ch := range f.readers {
		close(ch)
		delete(f.readers, ch)
	}
}

type continuousTSReader struct {
	ctx     context.Context
	fan     *continuousFanout
	ch      chan []byte
	done    chan struct{}
	once    sync.Once
	pending []byte
	cancel  context.CancelFunc
}

func (r *continuousTSReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		select {
		case <-r.done:
			return 0, io.ErrClosedPipe
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case p, ok := <-r.ch:
			if !ok {
				return 0, io.EOF
			}
			r.pending = p
		}
	}
	n := copy(b, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func (r *continuousTSReader) Close() error {
	r.once.Do(func() {
		r.fan.mu.Lock()
		delete(r.fan.readers, r.ch)
		r.fan.mu.Unlock()
		close(r.done)
		if r.cancel != nil {
			r.cancel()
		}
	})
	return nil
}

type continuousSegmentIndex struct {
	policySeq                int64
	policyTags               map[int64][]any
	enc                      *continuousEncoder
	mu                       sync.Mutex
	segments                 []Segment
	next                     int64
	start                    time.Time
	duration                 time.Duration
	changed                  chan struct{}
	codecs                   string
	width, height, bandwidth int
}

var _ SegmentIndex = (*continuousSegmentIndex)(nil)

func newContinuousSegmentIndex(e *continuousEncoder) *continuousSegmentIndex {
	f := e.req.Source
	g := (4*f.FPSNum + f.FPSDen/2) / f.FPSDen
	w, h := outputSize(f.Width, f.Height, e.req.Decision.MaxHeight)
	d := e.req.Decision
	if d.VideoCodec == "" {
		d.VideoCodec = "h264"
	}
	bandwidth := d.BitrateKbps
	if bandwidth <= 0 {
		bandwidth = videoBitrate(core.MediaInfo{}, d) + max(d.AudioBitrateKbps, 128)
	}
	return &continuousSegmentIndex{enc: e, duration: time.Second * time.Duration(g*f.FPSDen) / time.Duration(f.FPSNum), changed: make(chan struct{}), width: w, height: h, bandwidth: bandwidth * 1000}
}
func (s *continuousSegmentIndex) setStart(at time.Time) { s.mu.Lock(); s.start = at; s.mu.Unlock() }
func (s *continuousSegmentIndex) ts(seq int64) string {
	return filepath.Join(s.enc.dir, fmt.Sprintf("seg-%09d.ts", seq))
}
func (s *continuousSegmentIndex) mp4(seq int64) string {
	return filepath.Join(s.enc.dir, fmt.Sprintf("fragment-%09d.m4s", seq))
}
func (s *continuousSegmentIndex) watch() {
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-s.enc.ctx.Done():
			return
		case <-timer.C:
			s.refresh()
		}
	}
}
func (s *continuousSegmentIndex) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.start.IsZero() {
		return
	}
	if s.codecs == "" {
		if b, e := os.ReadFile(filepath.Join(s.enc.dir, "init.mp4")); e == nil {
			if tracks, e := parseInitTracks(b); e == nil {
				var codecs []string
				for _, t := range tracks {
					codecs = append(codecs, t.Codecs)
					if t.Handler == "vide" {
						s.width = t.Width
						s.height = t.Height
					}
				}
				s.codecs = strings.Join(codecs, ",")
			}
		}
	}
	// A machine suspend can outlast the live window. Recover the retained
	// prefix from the muxer playlist rather than waiting forever for an evicted file.
	if b, e := os.ReadFile(filepath.Join(s.enc.dir, "index.m3u8")); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
				first, e := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
				if e == nil && first > s.next {
					s.next = first
					for seq := range s.policyTags {
						if seq < first {
							delete(s.policyTags, seq)
						}
					}
					s.segments = nil
				}
				break
			}
		}
	}
	for {
		if _, e := os.Stat(s.ts(s.next)); e != nil {
			break
		}
		if _, e := os.Stat(s.mp4(s.next)); e != nil {
			break
		}
		// Both muxers use atomic temp-file publication. Only their shared finished
		// prefix is visible, so a viewer can change delivery format at any boundary.
		segment := s.segment(s.next)
		segment.PolicyTags = s.policyTags[s.next]
		delete(s.policyTags, s.next)
		s.segments = append(s.segments, segment)
		s.next++
		for len(s.segments) > 1 {
			evict := len(s.segments) > 6
			if s.enc.req.DVRWindow > 0 {
				latest := s.segments[len(s.segments)-1]
				first := s.segments[0]
				evict = latest.Start.Add(latest.Duration).Sub(first.Start.Add(first.Duration)) >= s.enc.req.DVRWindow
			}
			if !evict {
				break
			}
			old := s.segments[0]
			s.segments = s.segments[1:]
			_ = os.Remove(s.ts(old.Seq))
			_ = os.Remove(s.mp4(old.Seq))
		}
		close(s.changed)
		s.changed = make(chan struct{})
	}
}
func (s *continuousSegmentIndex) Latest() (Segment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segments) == 0 {
		return Segment{}, false
	}
	return s.segments[len(s.segments)-1], true
}
func (s *continuousSegmentIndex) Window(from, until time.Time) []Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Segment
	for _, seg := range s.segments {
		if seg.Start.Before(until) && seg.Start.Add(seg.Duration).After(from) {
			out = append(out, seg)
		}
	}
	return out
}
func (s *continuousSegmentIndex) Since(seq int64, max int) []Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max <= 0 {
		return nil
	}
	i := sort.Search(len(s.segments), func(i int) bool { return s.segments[i].Seq > seq })
	end := i + min(max, len(s.segments)-i)
	return append([]Segment(nil), s.segments[i:end]...)
}
func (s *continuousSegmentIndex) Wait(ctx context.Context, after int64) (Segment, error) {
	if after < -1 || after == math.MaxInt64 {
		return Segment{}, core.ErrNotFound
	}
	for {
		s.mu.Lock()
		if len(s.segments) > 0 {
			first := s.segments[0].Seq
			if after+1 < first {
				s.mu.Unlock()
				return Segment{}, core.ErrNotFound
			}
			if after+1 < s.next {
				out := s.segments[after+1-first]
				s.mu.Unlock()
				return out, nil
			}
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Segment{}, ctx.Err()
		case <-s.enc.ctx.Done():
			return Segment{}, io.EOF
		case <-changed:
		}
	}
}
func (s *continuousSegmentIndex) open(seq int64, mp4 bool) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.segments) == 0 || seq < s.segments[0].Seq || seq >= s.next {
		return nil, core.ErrNotFound
	}
	path := s.ts(seq)
	if mp4 {
		path = s.mp4(seq)
	}
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return nil, core.ErrNotFound
	}
	return f, e
}
func (s *continuousSegmentIndex) serve(w http.ResponseWriter, r *http.Request, seq int64, mp4 bool) {
	f, e := s.open(seq, mp4)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	s.serveOpen(w, r, f, mp4)
}
func (s *continuousSegmentIndex) serveOpen(w http.ResponseWriter, r *http.Request, f *os.File, mp4 bool) {
	info, e := f.Stat()
	if e != nil {
		http.NotFound(w, r)
		return
	}
	ctype := "video/mp2t"
	if mp4 {
		ctype = "video/mp4"
	}
	w.Header().Set("Content-Type", ctype)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
func (s *continuousSegmentIndex) ServeTS(w http.ResponseWriter, r *http.Request, seq int64) {
	s.serve(w, r, seq, false)
}
func (s *continuousSegmentIndex) ServeFMP4(w http.ResponseWriter, r *http.Request, seq int64) {
	s.serve(w, r, seq, true)
}
func (s *continuousSegmentIndex) ServeInit(w http.ResponseWriter, r *http.Request) {
	f, e := os.Open(filepath.Join(s.enc.dir, "init.mp4"))
	if e != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	s.serveOpen(w, r, f, true)
}
func (s *continuousSegmentIndex) Format() (string, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codecs, s.width, s.height, s.bandwidth
}
func (s *continuousSegmentIndex) AttachTS(ctx context.Context) (io.ReadCloser, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	f := s.enc.stream
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, io.EOF
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &continuousTSReader{cancel: cancel, ctx: ctx, fan: f, ch: make(chan []byte, 32), done: make(chan struct{})}
	f.readers[r.ch] = struct{}{}
	context.AfterFunc(ctx, func() { r.Close() })
	return r, nil
}

func continuousWindow(window, duration time.Duration) int {
	if window == 0 {
		return 6
	}
	return max(1, int((window+duration-1)/duration))
}

// recordPolicy follows the encoder tick clock, including short startup GOPs.
// Tags are accumulated before media can publish and released with DVR eviction.
func (s *continuousSegmentIndex) recordPolicy(tick int64, tags []any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start, frames := continuousSegmentFrames(s.enc.req.Source, s.policySeq)
	if tick >= start+frames {
		s.policySeq++
	}
	if s.policyTags == nil {
		s.policyTags = make(map[int64][]any)
	}
	for _, tag := range tags {
		found := false
		for _, old := range s.policyTags[s.policySeq] {
			if old == tag {
				found = true
				break
			}
		}
		if !found {
			s.policyTags[s.policySeq] = append(s.policyTags[s.policySeq], tag)
		}
	}
}
