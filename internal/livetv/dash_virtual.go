package livetv

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// VirtualDASHError is a virtual channel that could not produce DASH.
// Plex turns it into a decision document. A bare 503 is what Plex Web
// reports as s1001 (Network), which hides the reason and blocks fallback.
type VirtualDASHError struct {
	Err error
	// Message is the client-facing reason when it is more specific than Err.
	Message string
}

func (e *VirtualDASHError) Error() string {
	if e == nil {
		return "DASH is not available for this channel"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "DASH is not available for this channel"
}

func (e *VirtualDASHError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// virtualProgram is one catalogue item encoded as DASH for a virtual channel.
// Segment numbers continue across programmes the way the HLS playlist's
// media sequence does: the next item starts when this one ends.
type virtualProgram struct {
	tr   *track
	info core.MediaInfo
}

// virtualDASH is the DASH view of a virtual-channel session. It uses the
// same per-programme playback source, join offset and rollover as HLS, and
// publishes one dynamic MPD in the shape Plex Media Server uses for live TV.
type virtualDASH struct {
	s      *Service
	ss     *session
	id     string
	origin time.Time // wall time of global segment 0
	mu     sync.Mutex
	items  []*virtualProgram
	next   int64
	closed bool
}

func (s *Service) publishVirtualDASH(ctx context.Context, ss *session, id string, gen int64) (core.DASHSession, error) {
	v, err := s.openVirtualDASH(ctx, ss, id)
	if err != nil {
		return nil, err
	}
	ss.mu.Lock()
	stale := ss.closed || ss.generation != gen
	var existing core.DASHSession
	if !stale && ss.dash != nil {
		existing = ss.dash
	}
	if !stale && existing == nil {
		ss.dash = v
	}
	ss.mu.Unlock()
	if stale {
		v.Close()
		return nil, core.ErrNotFound
	}
	if existing != nil {
		v.Close()
		return existing, nil
	}
	return v, nil
}

func (s *Service) openVirtualDASH(ctx context.Context, ss *session, id string) (*virtualDASH, error) {
	v := &virtualDASH{s: s, ss: ss, id: id}
	if err := v.sync(ctx); err != nil {
		v.Close()
		return nil, err
	}
	v.mu.Lock()
	n := len(v.items)
	v.mu.Unlock()
	if n == 0 {
		v.Close()
		return nil, &VirtualDASHError{Err: core.ErrNotAvailable}
	}
	return v, nil
}

func (v *virtualDASH) ID() string { return "livetv-dash-" + v.id }

func (v *virtualDASH) LastAccess() time.Time { return v.s.o.Now() }

func (v *virtualDASH) Close() error {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil
	}
	v.closed = true
	items := v.items
	v.items = nil
	v.mu.Unlock()
	for _, it := range items {
		v.s.stopTrack(it.tr)
	}
	return nil
}

func (v *virtualDASH) SwapInput(context.Context, string, core.MediaInfo, time.Duration) error {
	return core.ErrNotAvailable
}

func (v *virtualDASH) ServePlaylist(w http.ResponseWriter, r *http.Request) {
	v.ServeManifest(w, r)
}

func (v *virtualDASH) ServeSegment(w http.ResponseWriter, r *http.Request, n int) {
	v.ServeMedia(w, r, 0, n)
}

func (v *virtualDASH) ServeManifest(w http.ResponseWriter, r *http.Request) {
	if err := v.sync(r.Context()); err != nil && (errors.Is(err, core.ErrNotFound) || len(v.snapshot()) == 0) {
		status := http.StatusServiceUnavailable
		if errors.Is(err, core.ErrNotFound) {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		return
	}
	now := v.s.o.Now()
	segs, reps := v.timeline(now)
	if len(segs) == 0 || len(reps) == 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	initURL := "session/livetv-dash-" + v.id + "/$RepresentationID$/header"
	mediaURL := "session/livetv-dash-" + v.id + "/$RepresentationID$/$Number$.m4s"
	body := renderVirtualLiveMPD(v.origin, now, int(segs[0].seq), segs, initURL, mediaURL, reps)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

func (v *virtualDASH) ServeInit(w http.ResponseWriter, r *http.Request, rep int) {
	if err := v.sync(r.Context()); err != nil {
		http.NotFound(w, r)
		return
	}
	it := v.liveItem(v.s.o.Now())
	if it == nil {
		http.NotFound(w, r)
		return
	}
	ds, ok := it.tr.hls.(core.DASHSession)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ds.ServeInit(w, r, rep)
}

func (v *virtualDASH) ServeMedia(w http.ResponseWriter, r *http.Request, rep, n int) {
	if n < 0 {
		http.NotFound(w, r)
		return
	}
	if err := v.sync(r.Context()); err != nil {
		http.NotFound(w, r)
		return
	}
	it, local, ok := v.locate(int64(n))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ds, isDASH := it.tr.hls.(core.DASHSession)
	if !isDASH {
		http.NotFound(w, r)
		return
	}
	ds.ServeMedia(w, r, rep, local)
}

// sync prepares the current programme and the next ones, and drops items
// that have fallen out of the live window. Offsets match HLS: the programme
// on now is joined on a segment boundary, and the following items start at
// the beginning.
func (v *virtualDASH) sync(ctx context.Context) error {
	v.ss.prepareMu.Lock()
	defer v.ss.prepareMu.Unlock()
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return core.ErrNotFound
	}
	v.mu.Unlock()
	v.ss.mu.Lock()
	if v.ss.closed {
		v.ss.mu.Unlock()
		return core.ErrNotFound
	}
	channel := v.ss.info.Channel
	v.ss.mu.Unlock()

	now := v.s.o.Now()
	ps, err := v.s.programmes(ctx, channel, now.Add(-24*time.Second), now.Add(24*time.Second))
	if err != nil {
		return &VirtualDASHError{Err: err}
	}
	var want []Programme
	for _, p := range ps {
		if now.Before(p.End) {
			want = append(want, p)
		}
	}
	if len(want) > 3 {
		want = want[:3]
	}
	desired := map[string]bool{}
	for _, p := range want {
		desired[p.ID] = true
	}

	v.mu.Lock()
	var removed []*virtualProgram
	kept := make([]*virtualProgram, 0, len(v.items))
	for _, it := range v.items {
		if !desired[it.tr.programme.ID] && it.tr.programme.End.Before(now.Add(-24*time.Second)) {
			removed = append(removed, it)
			continue
		}
		kept = append(kept, it)
	}
	v.items = kept
	v.mu.Unlock()
	for _, it := range removed {
		v.s.stopTrack(it.tr)
	}

	for _, p := range want {
		if v.has(p.ID) {
			continue
		}
		if len(v.snapshot())+1 > 3 {
			break
		}
		offset := time.Duration(0)
		if !now.Before(p.Start) && now.Before(p.End) && len(v.snapshot()) == 0 {
			offset = alignedJoin(p, now)
		}
		it, err := v.s.startProgrammeDASH(ctx, v.ss, p, offset)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return err
			}
			if errors.Is(err, core.ErrUnauthorized) && p.Start.After(now) {
				break
			}
			if !now.Before(p.Start) && now.Before(p.End) {
				return err
			}
			break
		}
		v.mu.Lock()
		if v.closed || v.hasLocked(p.ID) {
			closed := v.closed
			v.mu.Unlock()
			v.s.stopTrack(it.tr)
			if closed {
				return core.ErrNotFound
			}
			continue
		}
		it.tr.sequence = v.next
		v.next += int64(it.tr.count)
		if v.origin.IsZero() {
			v.origin = p.Start.Add(offset)
		}
		v.items = append(v.items, it)
		v.mu.Unlock()
	}
	return nil
}

func (v *virtualDASH) has(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hasLocked(id)
}

func (v *virtualDASH) hasLocked(id string) bool {
	for _, it := range v.items {
		if it.tr.programme.ID == id {
			return true
		}
	}
	return false
}

func (v *virtualDASH) snapshot() []*virtualProgram {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]*virtualProgram(nil), v.items...)
}

func alignedJoin(p Programme, now time.Time) time.Duration {
	offset := (p.Offset(now) / segmentDuration) * segmentDuration
	if offset < 0 {
		offset = 0
	}
	if span := p.End.Sub(p.Start); offset >= span && span > 0 {
		offset = 0
	}
	return offset
}

func (s *Service) startProgrammeDASH(ctx context.Context, ss *session, p Programme, offset time.Duration) (*virtualProgram, error) {
	if s.o.Playback == nil || s.o.Transcoder == nil {
		return nil, &VirtualDASHError{Err: core.ErrNotAvailable}
	}
	ss.mu.Lock()
	user := ss.user
	caps := ss.caps
	channel := ss.info.Channel
	closed := ss.closed
	ss.mu.Unlock()
	if closed {
		return nil, core.ErrNotFound
	}
	if p.Item.Slate == offAirSlate {
		return nil, &VirtualDASHError{Err: core.ErrNotAvailable, Message: "This channel is off air."}
	}
	if !s.allowed(ctx, &user, channel, &p) {
		return nil, &VirtualDASHError{Err: core.ErrUnauthorized}
	}
	caps.ClientID = "livetv-dash-" + ss.info.ID + "-" + strconv.FormatInt(p.Start.UnixMilli(), 10)
	var ps *core.PlaySession
	var err error
	if grouped, ok := s.o.Playback.(core.ChannelPreparer); ok {
		ps, err = grouped.PrepareChannel(ctx, &user, p.Item.Key, caps, ss.info.ID)
	} else {
		ps, err = s.o.Playback.Prepare(ctx, &user, p.Item.Key, caps)
	}
	if err != nil {
		return nil, &VirtualDASHError{Err: err}
	}
	info := ps.Info
	info.Duration = p.End.Sub(p.Start)
	if info.Duration < 0 {
		info.Duration = 0
	}
	decision := s.o.Transcoder.Decide(info, caps, core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1})
	if decision.Kind == core.DecisionDirectPlay {
		decision.Kind = core.DecisionRemux
		decision.VideoCopy = true
		decision.AudioCopy = true
	}
	sid := "livetv-dash-" + ss.info.ID + "-" + strconv.FormatInt(p.Start.UnixMilli(), 10)
	hs, lifetime, err := s.startOwnedHLS(ctx, ss, core.HLSRequest{
		SessionID: sid, InputURL: s.o.Playback.InputURL(ps.ID), Info: info, Decision: decision,
		StartOffset: offset, SegmentDuration: segmentDuration, Protocol: core.ProtocolDASH,
	})
	if err != nil {
		s.o.Playback.Stop(ps.ID)
		return nil, &VirtualDASHError{Err: err}
	}
	if _, ok := hs.(core.DASHSession); !ok {
		if lifetime != nil {
			lifetime()
		}
		hs.Close()
		s.o.Playback.Stop(ps.ID)
		return nil, &VirtualDASHError{Err: core.ErrNotAvailable, Message: "This live channel cannot be played as DASH."}
	}
	tr := &track{
		programme: p, hls: hs, cancel: lifetime, playbackID: ps.ID, offset: offset,
		count: trackCount(info.Duration - offset),
	}
	if tr.count < 1 {
		tr.count = 1
	}
	s.transMu.Lock()
	s.transcodes[ps.ID] = hs
	s.transMu.Unlock()
	return &virtualProgram{tr: tr, info: info}, nil
}

type dashPiece struct {
	seq   int64
	at    time.Time
	dur   time.Duration
	local int
	item  *virtualProgram
}

func (v *virtualDASH) timeline(now time.Time) ([]dashPiece, []virtualRep) {
	items := v.snapshot()
	var segs []dashPiece
	var live *virtualProgram
	for _, it := range items {
		if !now.Before(it.tr.programme.Start) && now.Before(it.tr.programme.End) {
			live = it
		}
		base := int(it.tr.offset / segmentDuration)
		for n := 0; n < it.tr.count; n++ {
			at := it.tr.programme.Start.Add(it.tr.offset + time.Duration(n)*segmentDuration)
			dur := segmentDuration
			if n == it.tr.count-1 {
				dur = it.tr.programme.End.Sub(at)
			}
			if dur <= 0 {
				dur = segmentDuration
			}
			if at.Before(now.Add(-24*time.Second)) || at.After(now.Add(20*time.Second)) {
				continue
			}
			segs = append(segs, dashPiece{seq: it.tr.sequence + int64(n), at: at, dur: dur, local: base + n, item: it})
		}
	}
	if live == nil && len(items) > 0 {
		live = items[0]
	}
	var info core.MediaInfo
	if live != nil {
		info = live.info
	}
	return segs, virtualReps(info)
}

func (v *virtualDASH) locate(seq int64) (*virtualProgram, int, bool) {
	for _, it := range v.snapshot() {
		if seq < it.tr.sequence || seq >= it.tr.sequence+int64(it.tr.count) {
			continue
		}
		n := int(seq - it.tr.sequence)
		base := int(it.tr.offset / segmentDuration)
		return it, base + n, true
	}
	return nil, 0, false
}

func (v *virtualDASH) liveItem(now time.Time) *virtualProgram {
	items := v.snapshot()
	for _, it := range items {
		if !now.Before(it.tr.programme.Start) && now.Before(it.tr.programme.End) {
			return it
		}
	}
	if len(items) == 0 {
		return nil
	}
	return items[len(items)-1]
}

type virtualRep struct {
	id        int
	kind      string
	codecs    string
	bandwidth int
	width     int
	height    int
}

func (r virtualRep) mime() string {
	if r.kind == "audio" {
		return "audio/mp4"
	}
	return "video/mp4"
}

// virtualReps describes the fMP4 representations Ether's DASH transcoder
// actually emits (H.264 and AAC). Plex Web matches these against MSE.
func virtualReps(info core.MediaInfo) []virtualRep {
	var reps []virtualRep
	if v := info.Video(); v != nil {
		rep := virtualRep{id: 0, kind: "video", codecs: "avc1.640028", bandwidth: 4000000, width: v.Width, height: v.Height}
		if v.BitrateKbps > 0 {
			rep.bandwidth = v.BitrateKbps * 1000
		}
		reps = append(reps, rep)
	}
	for _, st := range info.Streams {
		if st.Type != "audio" {
			continue
		}
		bw := 128000
		if st.BitrateKbps > 0 {
			bw = st.BitrateKbps * 1000
		}
		reps = append(reps, virtualRep{id: len(reps), kind: "audio", codecs: "mp4a.40.2", bandwidth: bw})
		break
	}
	if len(reps) == 0 {
		reps = append(reps, virtualRep{id: 0, kind: "video", codecs: "avc1.640028", bandwidth: 4000000})
	}
	return reps
}

// renderVirtualLiveMPD writes a dynamic live MPD. The attributes match
// transcode.renderLiveMPD, which follows a recorded PMS live DASH manifest:
// one period, a segment timeline, and no invented end. Durations follow the
// programme segments, so a boundary can be shorter than the grid the way the
// HLS playlist's last segment is.
func renderVirtualLiveMPD(origin, published time.Time, startNumber int, segs []dashPiece, initURL, mediaURL string, reps []virtualRep) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011" availabilityStartTime="%s" publishTime="%s" minimumUpdatePeriod="PT2S" timeShiftBufferDepth="PT120S" suggestedPresentationDelay="PT8S" minBufferTime="PT4S"><Period id="0" start="PT0S">`, origin.UTC().Format(time.RFC3339Nano), published.UTC().Format(time.RFC3339Nano))
	for _, rep := range reps {
		fmt.Fprintf(&b, `<AdaptationSet segmentAlignment="true" contentType="%s"><SegmentTemplate timescale="1000" initialization="%s" media="%s" startNumber="%d"><SegmentTimeline>`, rep.kind, html.EscapeString(initURL), html.EscapeString(mediaURL), startNumber)
		writeTimeline(&b, origin, segs)
		fmt.Fprintf(&b, `</SegmentTimeline></SegmentTemplate><Representation id="%d" mimeType="%s" codecs="%s" bandwidth="%d"`, rep.id, rep.mime(), html.EscapeString(rep.codecs), rep.bandwidth)
		if rep.width > 0 && rep.height > 0 {
			fmt.Fprintf(&b, ` width="%d" height="%d"`, rep.width, rep.height)
		}
		b.WriteString(`/></AdaptationSet>`)
	}
	b.WriteString(`</Period></MPD>`)
	return b.String()
}

func writeTimeline(b *strings.Builder, origin time.Time, segs []dashPiece) {
	if len(segs) == 0 {
		return
	}
	type run struct {
		t, d int64
		r    int
	}
	var runs []run
	for _, seg := range segs {
		t := seg.at.Sub(origin).Milliseconds()
		d := seg.dur.Milliseconds()
		if d < 1 {
			d = 1
		}
		if n := len(runs); n > 0 && runs[n-1].d == d && runs[n-1].t+int64(runs[n-1].r+1)*d == t {
			runs[n-1].r++
			continue
		}
		runs = append(runs, run{t: t, d: d, r: 0})
	}
	for _, run := range runs {
		if run.r > 0 {
			fmt.Fprintf(b, `<S t="%d" d="%d" r="%d"/>`, run.t, run.d, run.r)
			continue
		}
		fmt.Fprintf(b, `<S t="%d" d="%d"/>`, run.t, run.d)
	}
}
