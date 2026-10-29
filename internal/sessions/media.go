package sessions

import (
	"context"
	"fmt"
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
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return "", err
	}
	defer unlock()
	s, _, err = b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	if req.Format != frontend.StreamDirect && req.Format != frontend.StreamHLS && req.Format != frontend.StreamDASH {
		return "", core.ErrNotAvailable
	}
	path := "/sessions/media/" + s.public.ID + "/" + s.grant + "/"
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
				return path + "index.m3u8", nil
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
	if s.allowTranscoding != nil && !*s.allowTranscoding && dec.Kind == core.DecisionTranscode {
		return "", core.ErrNotAvailable
	}
	if dec.Kind == core.DecisionRemux && !s.opts.DirectStream {
		return "", core.ErrNotAvailable
	}
	b.deps.Transcoder.Stop(id)
	hs, err := b.deps.Transcoder.StartHLS(context.WithoutCancel(ctx), core.HLSRequest{SessionID: id, InputURL: b.deps.Playback.InputURL(s.playID), Info: ps.Info, Decision: dec, StartOffset: req.Position, SegmentDuration: req.SegmentDuration, Protocol: proto})
	if err != nil {
		return "", err
	}
	if hs == nil {
		b.deps.Transcoder.Stop(id)
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
	return path + "index.m3u8", nil
}

// ServeMedia validates a session-scoped capability on every request.
func (b *Broker) ServeMedia(w http.ResponseWriter, r *http.Request) {
	b.serveMedia("", w, r)
}

// ServeMediaFor binds a mounted handler to its protocol surface.
func (b *Broker) ServeMediaFor(protocol frontend.Protocol, w http.ResponseWriter, r *http.Request) {
	b.serveMedia(protocol, w, r)
}

func (b *Broker) serveMedia(protocol frontend.Protocol, w http.ResponseWriter, r *http.Request) {
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
	tail := parts[2:]
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
	if s.format == frontend.StreamHLS && len(tail) == 1 {
		if tail[0] == "index.m3u8" {
			hs.ServePlaylist(w, r)
			return
		}
		if n, ok := transcode.ParseSegmentName(tail[0]); ok {
			hs.ServeSegment(w, r, n)
			return
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

var _ frontend.Sessions = (*Broker)(nil)
