package sessions

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func (b *Broker) StartStream(ctx context.Context, a frontend.SessionAccess, id string, req frontend.StreamRequest) (string, error) {
	if req.Position < 0 || req.SegmentDuration < 0 {
		return "", core.ErrNotAvailable
	}
	s, _, err := b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return "", err
	}
	defer unlock()
	s, viewer, err := b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err != nil || !b.currentMediaPolicy(ctx, viewer, s) {
		return "", core.ErrUnauthorized
	}
	if req.Format != frontend.StreamDirect && req.Format != frontend.StreamHLS && req.Format != frontend.StreamDASH {
		return "", core.ErrNotAvailable
	}
	path := "/sessions/media/" + s.public.ID + "/" + s.grant + "/"
	if s.live {
		if req.Format != frontend.StreamHLS {
			return "", core.ErrNotAvailable
		}
		return path + "live/index.m3u8", nil
	}
	if req.Format == frontend.StreamDirect {
		if s.decision.Kind != core.DecisionDirectPlay || !s.opts.DirectPlay {
			return "", core.ErrNotAvailable
		}
		if b.deps.Transcoder != nil {
			b.deps.Transcoder.Stop(id)
		}
		if entry, ok := b.bindings.Get(id); ok {
			entry.Started = false
			b.bindings.Put(entry)
		}
		b.mu.Lock()
		if live := b.sessions[id]; live != nil {
			live.format = req.Format
		}
		b.mu.Unlock()
		return path + "file", nil
	}
	if b.deps.Transcoder == nil {
		return "", core.ErrNotAvailable
	}
	if s.format == req.Format && s.streamPosition == req.Position && s.segmentDuration == req.SegmentDuration {
		if hs, ok := b.deps.Transcoder.Session(id); ok && hs != nil {
			if req.Format == frontend.StreamDASH {
				if _, ok := hs.(core.DASHSession); ok {
					return path + "manifest.mpd", nil
				}
			} else {
				return path + "master.m3u8", nil
			}
		}
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		return "", core.ErrNotAvailable
	}
	dec := s.decision
	proto := core.ProtocolHLS
	if req.Format == frontend.StreamDASH {
		proto = core.ProtocolDASH
		if dec.Kind == core.DecisionDirectPlay {
			dec.Kind, dec.VideoCopy, dec.AudioCopy = core.DecisionRemux, true, true
		}
	} else {
		dec = HLSDecision(dec, s.opts.DirectPlay, func() core.Decision { return b.deps.Transcoder.Decide(ps.Info, s.caps, s.opts) })
	}
	if s.allowTranscoding != nil && !*s.allowTranscoding && decisionEncodes(dec, ps.Info) {
		return "", core.ErrNotAvailable
	}
	if !profileDeliveryAllowed(s.caps.Profiles, dec, ps.Info, string(req.Format)) {
		return "", core.ErrNotAvailable
	}
	if dec.Kind == core.DecisionRemux && !s.opts.DirectStream {
		return "", core.ErrNotAvailable
	}
	b.deps.Transcoder.Stop(id)
	hs, err := b.deps.Transcoder.StartHLS(context.WithoutCancel(ctx), core.HLSRequest{SessionID: id, InputURL: b.deps.Playback.InputURL(s.playID), Info: ps.Info, Decision: dec, StartOffset: req.Position, SegmentDuration: req.SegmentDuration, Protocol: proto})
	if err != nil {
		return "", capacityError(err)
	}
	if hs == nil {
		b.deps.Transcoder.Stop(id)
		return "", core.ErrNotAvailable
	}
	// StartHLS owns a detached lifetime, but a request canceled during startup
	// must not leave an encoder and permit behind. Close also prevents a
	// concurrently starting encoder from surviving shutdown.
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if err := ctx.Err(); err != nil || closed {
		b.deps.Transcoder.Stop(id)
		if err != nil {
			return "", err
		}
		return "", core.ErrNotAvailable
	}
	if req.Format == frontend.StreamDASH {
		if _, ok := hs.(core.DASHSession); !ok {
			b.deps.Transcoder.Stop(id)
			return "", core.ErrNotAvailable
		}
	}
	entry, ok := b.bindings.Get(id)
	if ok {
		entry.Started = true
		entry.Decision = dec
		entry.Offset = req.Position
		entry.UpdatedAt = b.deps.Now()
		b.bindings.Put(entry)
	}
	b.mu.Lock()
	if live := b.sessions[id]; live != nil {
		live.format = req.Format
		live.streamPosition = req.Position
		live.segmentDuration = req.SegmentDuration
	}
	b.mu.Unlock()
	if req.Format == frontend.StreamDASH {
		return path + "manifest.mpd", nil
	}
	return path + "master.m3u8", nil
}

// ServeMedia validates a session-scoped capability on every request.
func (b *Broker) ServeMedia(w http.ResponseWriter, r *http.Request) {
	b.serveMedia("", nil, w, r)
}

// ServeMediaFor binds a mounted handler to its protocol surface.
func (b *Broker) ServeMediaFor(protocol frontend.Protocol, w http.ResponseWriter, r *http.Request) {
	b.serveMedia(protocol, nil, w, r)
}

// ServeMediaAccess additionally binds the grant to the authenticated device.
func (b *Broker) ServeMediaAccess(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess) {
	b.serveMedia(a.Protocol, &a, w, r)
}

func (b *Broker) currentMediaPolicy(ctx context.Context, viewer *core.Viewer, s *brokerSession) bool {
	if viewer == nil || viewer.ID != s.public.ViewerID || !b.permitted(viewer, s.caps) {
		return false
	}
	if s.live {
		return b.deps.Live != nil && b.deps.Live.Allowed(ctx, viewer, s.public.Channel) == nil
	}
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	title, err := lookup(ctx, viewer, s.public.Device, s.public.Item)
	return err == nil && title != nil && title.Key == s.public.Item && (title.Released.IsZero() || !title.Released.After(b.deps.Now()))
}

func (b *Broker) serveMedia(protocol frontend.Protocol, access *frontend.SessionAccess, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/sessions/media/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	b.mu.Lock()
	s, ok := b.sessions[parts[0]]
	if ok {
		copy := *s
		s = &copy
	}
	b.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if protocol != "" && s.public.Protocol != protocol {
		http.NotFound(w, r)
		return
	}
	if access != nil && (access.Owner || access.Viewer == nil || access.Viewer.ID != s.public.ViewerID || access.Protocol == "" || access.Device.ID == "" || access.Device.ID != s.public.Device.ID) {
		http.NotFound(w, r)
		return
	}
	viewer, ok := AuthenticateGrant(r.Context(), r.Method, parts[1], GrantBinding{Token: s.grant, ViewerID: s.public.ViewerID, Started: true}, func(ctx context.Context, id int64) *core.Viewer {
		v, e := b.deps.ActiveViewer(ctx, id)
		if e != nil {
			return nil
		}
		return v
	})
	if !ok || viewer.ID != s.public.ViewerID || !b.permitted(viewer, s.caps) {
		http.NotFound(w, r)
		return
	}
	// Grants do not freeze library, rating, release, or channel permissions.
	// Check the current policy before handing any bytes to a backend.
	if !b.currentMediaPolicy(r.Context(), viewer, s) {
		http.NotFound(w, r)
		return
	}
	tail := parts[2:]
	if s.live && len(tail) >= 2 && tail[0] == "live" && b.deps.Live != nil {
		b.deps.Live.Serve(w, r, viewer, s.playID, strings.Join(tail[1:], "/"), "/sessions/media/"+s.public.ID+"/"+s.grant+"/live/")
		return
	}
	if len(tail) == 2 && tail[0] == "subtitles" {
		b.serveSubtitle(w, r, s, tail[1])
		return
	}
	if len(tail) == 1 && tail[0] == "file" && s.format == frontend.StreamDirect {
		b.deps.Playback.ServeRange(w, r, s.playID)
		return
	}
	if b.deps.Transcoder == nil {
		http.NotFound(w, r)
		return
	}
	hs, ok := b.deps.Transcoder.Session(s.public.ID)
	if !ok || hs == nil {
		http.NotFound(w, r)
		return
	}
	if s.format == frontend.StreamHLS {
		if len(tail) == 1 && tail[0] == "master.m3u8" {
			b.serveMaster(w, r, s, hs)
			return
		}
		// Keep the initial single-variant URLs working for already-open clients.
		if len(tail) == 1 {
			if tail[0] == "index.m3u8" {
				hs.ServePlaylist(w, r)
				return
			}
			if n, ok := transcode.ParseSegmentName(tail[0]); ok {
				hs.ServeSegment(w, r, n)
				return
			}
		}
		if len(tail) == 2 {
			rung, name := tail[0], tail[1]
			if ls, ok := hs.(core.HLSLadderSession); ok {
				for _, candidate := range ls.Rungs() {
					if candidate.ID == rung && validRung(rung) {
						if name == "index.m3u8" {
							ls.ServeRungPlaylist(w, r, rung)
							return
						}
						if n, ok := transcode.ParseSegmentName(name); ok {
							ls.ServeRungSegment(w, r, rung, n)
							return
						}
					}
				}
			} else if rung == "base" {
				if name == "index.m3u8" {
					hs.ServePlaylist(w, r)
					return
				}
				if n, ok := transcode.ParseSegmentName(name); ok {
					hs.ServeSegment(w, r, n)
					return
				}
			}
		}
	}
	if s.format == frontend.StreamDASH {
		ds, ok := hs.(core.DASHSession)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(tail) == 1 && tail[0] == "manifest.mpd" {
			ds.ServeManifest(w, r)
			return
		}
		if len(tail) == 4 && tail[0] == "session" && tail[1] == s.public.ID {
			rep, err := strconv.Atoi(tail[2])
			if err == nil && rep >= 0 {
				if tail[3] == "header" {
					ds.ServeInit(w, r, rep)
					return
				}
				if n, ok := transcode.ParseSegmentName(tail[3]); ok && n >= 0 {
					ds.ServeMedia(w, r, rep, n)
					return
				}
			}
		}
	}
	http.NotFound(w, r)
}

func validRung(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (b *Broker) serveMaster(w http.ResponseWriter, r *http.Request, s *brokerSession, hs core.HLSSession) {
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		http.NotFound(w, r)
		return
	}
	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	if ls, ok := hs.(core.HLSLadderSession); ok && len(ls.Rungs()) > 1 {
		for _, rung := range ls.Rungs() {
			if !validRung(rung.ID) || rung.Bandwidth <= 0 {
				continue
			}
			fmt.Fprintf(&body, "#EXT-X-STREAM-INF:BANDWIDTH=%d", rung.Bandwidth)
			if rung.AverageBandwidth > 0 {
				fmt.Fprintf(&body, ",AVERAGE-BANDWIDTH=%d", rung.AverageBandwidth)
			}
			if rung.Width > 0 && rung.Height > 0 {
				fmt.Fprintf(&body, ",RESOLUTION=%dx%d", rung.Width, rung.Height)
			}
			if rung.Codecs != "" && !strings.ContainsAny(rung.Codecs, "\"\r\n") {
				fmt.Fprintf(&body, ",CODECS=%q", rung.Codecs)
			}
			if rung.FrameRate > 0 {
				fmt.Fprintf(&body, ",FRAME-RATE=%.3f", rung.FrameRate)
			}
			fmt.Fprintf(&body, "\n%s/index.m3u8\n", rung.ID)
		}
	} else {
		entry, ok := b.bindings.Get(s.public.ID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		dec := entry.Decision
		bw := ps.Info.BitrateKbps
		if !dec.VideoCopy && dec.BitrateKbps > 0 {
			bw = dec.BitrateKbps
		}
		if bw <= 0 {
			bw = 8000
		}
		fmt.Fprintf(&body, "#EXT-X-STREAM-INF:BANDWIDTH=%d%s\nbase/index.m3u8\n", bw*1000, transcode.HLSVariantAttributes(ps.Info, dec))
	}
	if body.String() == "#EXTM3U\n" {
		http.Error(w, "no playable rendition", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body.String())
	}
}

var _ frontend.Sessions = (*Broker)(nil)
