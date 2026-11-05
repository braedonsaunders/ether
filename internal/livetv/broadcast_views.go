package livetv

import (
	"context"
	"errors"
	"fmt"
	"html"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func (s *Service) broadcastSegments(ctx context.Context, ss *session) (transcode.SegmentIndex, error) {
	ss.mu.Lock()
	lease := ss.broadcast
	ss.mu.Unlock()
	if lease == nil {
		return nil, core.ErrNotAvailable
	}
	return lease.output()
}
func (s *Service) broadcastWindow(ctx context.Context, ss *session, r *http.Request) (transcode.SegmentIndex, []transcode.Segment, error) {
	index, err := s.broadcastSegments(ctx, ss)
	if err != nil {
		return nil, nil, err
	}
	latest, ok := index.Latest()
	if !ok {
		wait, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		latest, err = index.Wait(wait, -1)
		if err != nil {
			// No playlist exists yet, including when a cold encoder exits before
			// publishing. Tell clients to retry rather than report a bad gateway.
			return nil, nil, core.ErrNotAvailable
		}
	}
	end := latest.Start.Add(latest.Duration)
	from := end.Add(-24 * time.Second)
	if raw := r.URL.Query().Get("offset"); raw != "" {
		seconds, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > broadcastDVR.Seconds() {
			return nil, nil, fmt.Errorf("invalid DVR offset")
		}
		end = end.Add(-time.Duration(seconds * float64(time.Second)))
		from = end.Add(-24 * time.Second)
	}
	ss.mu.Lock()
	user := ss.user
	ss.mu.Unlock()
	ctx, err = s.broadcastPolicyContext(ctx, ss, &user)
	if err != nil {
		return nil, nil, err
	}
	segments := index.Window(from, end)
	if len(segments) == 0 {
		return nil, nil, core.ErrNotAvailable
	}

	for _, seg := range segments {
		if !s.broadcastSegmentAllowed(ctx, ss, seg) {
			return nil, nil, core.ErrUnauthorized
		}
	}
	return index, segments, nil
}
func (s *Service) serveBroadcastPlaylist(w http.ResponseWriter, r *http.Request, ss *session) {
	_, segments, err := s.broadcastWindow(r.Context(), ss, r)
	if err != nil {
		if errors.Is(err, core.ErrNotAvailable) {
			w.Header().Set("Retry-After", "1")
		}
		writeStreamError(w, err)
		return
	}
	// TARGETDURATION is a property of the rendition, including its short
	// startup segments, and must not change as the window advances.
	target := 4
	for _, seg := range segments {
		target = max(target, int(math.Ceil(seg.Duration.Seconds())))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", target, segments[0].Seq)
	for _, seg := range segments {
		uri := fmt.Sprintf("relay/%d.ts", seg.Seq)
		if !strings.Contains(r.URL.Path, "/livetv/sessions/") {
			uri = fmt.Sprintf("/livetv/sessions/%s/0/relay/%d.ts", ss.info.ID, seg.Seq)
		}
		uri = withAuth(uri, ss.streamGrant)
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%.6f,\n%s\n", seg.Start.UTC().Format(time.RFC3339Nano), seg.Duration.Seconds(), uri)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		fmt.Fprint(w, b.String())
	}
}
func (s *Service) serveBroadcastTS(w http.ResponseWriter, r *http.Request, ss *session, seq int64) {
	index, err := s.broadcastSegments(r.Context(), ss)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	segments := index.Since(seq-1, 1)
	if len(segments) == 0 || segments[0].Seq != seq {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	if !s.broadcastSegmentAllowed(r.Context(), ss, segments[0]) {
		writeStreamError(w, core.ErrUnauthorized)
		return
	}
	s.touchMedia(ss)
	observed := s.observeMedia(w, ss)
	defer observed.done()
	index.ServeTS(observed, r, seq)
}
func (s *Service) serveBroadcastDASH(w http.ResponseWriter, r *http.Request, ss *session) error {
	index, segments, err := s.broadcastWindow(r.Context(), ss, r)
	if err != nil {
		return err
	}
	codecs, width, height, bandwidth := index.Format()
	first := segments[0]
	ss.mu.Lock()
	lease := ss.broadcast
	ss.mu.Unlock()
	if lease == nil {
		return core.ErrNotAvailable
	}
	origin, err := lease.timelineOrigin(r.Context())
	if err != nil {
		return err
	}
	init := withAuth(fmt.Sprintf("/livetv/sessions/%s/0/dash/0/header", ss.info.ID), ss.streamGrant)
	media := withAuth(fmt.Sprintf("/livetv/sessions/%s/0/dash/0/$Number$.m4s", ss.info.ID), ss.streamGrant)
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011" availabilityStartTime="%s" publishTime="%s" minimumUpdatePeriod="PT2S" minBufferTime="PT2S" timeShiftBufferDepth="PT1800S"><Period id="0" start="PT0S"><AdaptationSet mimeType="video/mp4" segmentAlignment="true"><Representation id="0" codecs="%s" width="%d" height="%d" bandwidth="%d"><SegmentTemplate timescale="1000000000" startNumber="%d" initialization="%s" media="%s"><SegmentTimeline>`, origin.UTC().Format(time.RFC3339Nano), s.o.Now().UTC().Format(time.RFC3339Nano), html.EscapeString(codecs), width, height, bandwidth, first.Seq, html.EscapeString(init), html.EscapeString(media))
	for _, seg := range segments {
		fmt.Fprintf(&b, `<S t="%d" d="%d"/>`, seg.Start.Sub(origin).Nanoseconds(), seg.Duration.Nanoseconds())
	}
	b.WriteString(`</SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`)
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		fmt.Fprint(w, b.String())
	}
	return nil
}
func (s *Service) serveBroadcastDASHPart(w http.ResponseWriter, r *http.Request, ss *session, rep int, seq int64, init bool) {
	if rep != 0 {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	index, err := s.broadcastSegments(r.Context(), ss)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	if init {
		index.ServeInit(w, r)
		return
	}
	segments := index.Since(seq-1, 1)
	if len(segments) == 0 || segments[0].Seq != seq {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	if !s.broadcastSegmentAllowed(r.Context(), ss, segments[0]) {
		writeStreamError(w, core.ErrUnauthorized)
		return
	}
	s.touchMedia(ss)
	observed := s.observeMedia(w, ss)
	defer observed.done()
	index.ServeFMP4(observed, r, seq)
}
