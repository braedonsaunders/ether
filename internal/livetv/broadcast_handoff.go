package livetv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// SteadyFeed is a channel's Steady output handed to a direct session that
// keeps its own owned timeline: one more producer the direct side splices
// in (with a discontinuity: its codec parameters differ from a copy relay).
type SteadyFeed interface {
	// Playlist is a live media playlist: TARGETDURATION of the segment
	// length, URIs "N.ts" with N the feed's media sequence, PDT per segment.
	Playlist(ctx context.Context) ([]byte, error)
	// Segment is segment N's MPEG-TS bytes.
	Segment(ctx context.Context, seq int64) ([]byte, error)
	// Ready returns once two segments (the client cushion) are published.
	Ready(ctx context.Context) error
	// Close releases the lease, the input grant and the capacity.
	Close()
}

// steadyHandoff starts or joins the channel's Steady output for a direct
// session already playing, adopting that session's input: the same resolved
// URL and headers on a Steady grant, so a transport stream joins the
// session's tap (one provider connection) and the provider reservation counts
// both as one. It never pre-empts a running viewer: it fails with an
// ErrStreamLimit error when capacity or allocation refuses, and with
// core.ErrNotAvailable when the session may not use Steady at all.
func (s *Service) steadyHandoff(ctx context.Context, ss *session) (SteadyFeed, error) {
	if !s.steadyAllowed(ss) {
		return nil, core.ErrNotAvailable
	}
	runner, ok := s.transcoder().(transcode.ContinuousRunner)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	ss.mu.Lock()
	c, user, caps := ss.info.Channel, ss.user, ss.caps
	src, input, headers := ss.inputSource, ss.inputURL, ss.inputHeaders.Clone()
	ss.mu.Unlock()
	if input == "" {
		return nil, core.ErrNotAvailable
	}
	key := broadcastKey{channelID: c.ID}
	var admitted *steadyAdmission
	var format transcode.RawFormat
	var full int
	class := ""
	if running := s.broadcasts.running(key); running != nil {
		// Joining a running output needs no capacity.
		format, full = running.format, running.steadyBitrate
	} else {
		link, err := s.linkSessionInput(ss, c, src, input, headers)
		if err != nil {
			return nil, err
		}
		info := s.cachedProbe(link.ss)
		if code, detail := steadyRefusal(info); code != "" {
			link.close(s)
			return nil, fmt.Errorf("%w: %s", core.ErrNotAvailable, detail)
		}
		shape := steadyShapeFor(info)
		var code, detail string
		class, code, detail = s.steadyAllocation(c, src, shape.format)
		if code != "" {
			link.close(s)
			return nil, fmt.Errorf("live TV hand-off refused (%s): %s: %w", code, detail, core.ErrStreamLimit)
		}
		link.format = steadyInputFormat(info)
		admitted = &steadyAdmission{shape: shape, link: link, info: info}
		format, full = shape.format, shape.bitrateKbps
	}
	outputHeight, bitrate := steadyRendition(format, full, caps)
	lease, _, err := s.launchBroadcast(ctx, ss, broadcastLaunch{channel: c, user: user, caps: caps, key: key, runner: runner, format: format, outputHeight: outputHeight, bitrate: bitrate, admitted: admitted, logAttrs: []any{"handoff", true, "allocation", class}})
	if err != nil {
		return nil, err
	}
	s.sessionLog(ss).Info("live input handed off to steady", "input", src.ID, "allocation", class)
	return &steadyFeed{s: s, ss: ss, lease: lease}, nil
}

// linkSessionInput grants a direct session's current input to a new Steady
// link without resolving it again (a plugin input could resolve elsewhere).
func (s *Service) linkSessionInput(ss *session, c Channel, src StreamSource, input string, headers http.Header) (*steadyLink, error) {
	account := src.AuthSourceID
	if account == "" {
		account = c.SourceID
	}
	entry, err := s.sourceEntry(s.serviceContext(), account)
	if err != nil {
		return nil, err
	}
	release, err := s.reserveBroadcastProvider(account, c.ID, "broadcast-"+c.ID, entry.limit)
	if err != nil {
		return nil, err
	}
	inputCtx, cancel := context.WithCancel(s.serviceContext())
	link := &session{ctx: inputCtx, cancel: cancel, info: SessionInfo{ID: "broadcast-" + c.ID, Channel: c}, inputSource: src, inputURL: input, inputHeaders: headers}
	grant := src
	grant.URL = input
	if err := s.grantSteadyInput(link, grant, headers); err != nil {
		cancel()
		release()
		return nil, err
	}
	return &steadyLink{src: src, ss: link, release: release, format: s.cachedInputFormat(link)}, nil
}

// cachedProbe is a granted input's cached probe, without probing.
func (s *Service) cachedProbe(ss *session) *core.MediaInfo {
	key := probeCacheKey(ss)
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if entry, ok := s.probeCache[key]; ok {
		info := entry.info
		return &info
	}
	return nil
}

type steadyFeed struct {
	s     *Service
	ss    *session
	lease *broadcastLease
}

func (f *steadyFeed) Playlist(ctx context.Context) ([]byte, error) {
	index, err := f.lease.output()
	if err != nil {
		return nil, err
	}
	latest, ok := index.Latest()
	if !ok {
		return nil, core.ErrNotAvailable
	}
	end := latest.Start.Add(latest.Duration)
	segments := index.Window(end.Add(-24*time.Second), end)
	if len(segments) == 0 {
		return nil, core.ErrNotAvailable
	}
	for _, seg := range segments {
		if !f.s.broadcastSegmentAllowed(ctx, f.ss, seg) {
			return nil, core.ErrUnauthorized
		}
	}
	target := int(math.Ceil(broadcastSegment(f.lease).Seconds()))
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", target, segments[0].Seq)
	for _, seg := range segments {
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%.6f,\n%d.ts\n", seg.Start.UTC().Format(time.RFC3339Nano), seg.Duration.Seconds(), seg.Seq)
	}
	return []byte(b.String()), nil
}

func (f *steadyFeed) Segment(ctx context.Context, seq int64) ([]byte, error) {
	index, err := f.lease.output()
	if err != nil {
		return nil, err
	}
	known := false
	for _, seg := range index.Since(seq-1, 1) {
		if seg.Seq == seq {
			known = true
			if !f.s.broadcastSegmentAllowed(ctx, f.ss, seg) {
				return nil, core.ErrUnauthorized
			}
		}
	}
	if !known {
		return nil, core.ErrNotFound
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return nil, err
	}
	w := &bufferResponse{header: http.Header{}}
	index.ServeTS(w, req, seq)
	if w.status != 0 && w.status != http.StatusOK {
		return nil, core.ErrNotFound
	}
	return w.body.Bytes(), nil
}

func (f *steadyFeed) Ready(ctx context.Context) error {
	index, err := f.lease.output()
	if err != nil {
		return err
	}
	segment := broadcastSegment(f.lease)
	latest, ok := index.Latest()
	if !ok {
		if latest, err = index.Wait(ctx, -1); err != nil {
			return err
		}
	}
	first := index.Since(-1, 1)
	for len(first) > 0 && latest.Start.Add(latest.Duration).Sub(first[0].Start) < 2*segment {
		if latest, err = index.Wait(ctx, latest.Seq); err != nil {
			return err
		}
	}
	if len(first) == 0 {
		return errors.New("live TV hand-off: no segment published")
	}
	return nil
}

func (f *steadyFeed) Close() { f.lease.close() }

// bufferResponse collects one served segment.
type bufferResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferResponse) Header() http.Header { return w.header }
func (w *bufferResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *bufferResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}
