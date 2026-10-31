package livetv

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

var dashURL = regexp.MustCompile(`(initialization|media)="([^"]+)"`)

func (s *Service) ServeDASH(w http.ResponseWriter, r *http.Request, id string, u *core.User) error {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return nil
	}
	ds, err := s.ensureDASH(r.Context(), ss, id)
	if err != nil {
		var dashErr *VirtualDASHError
		if errors.As(err, &dashErr) {
			return err
		}
		writeStreamError(w, err)
		return nil
	}
	rec := httptest.NewRecorder()
	ds.ServeManifest(rec, r)
	if rec.Code != 200 {
		if _, virtual := ds.(*virtualDASH); virtual {
			switch rec.Code {
			case http.StatusNotFound:
				return core.ErrNotFound
			case http.StatusForbidden:
				return &VirtualDASHError{Err: core.ErrUnauthorized}
			default:
				return &VirtualDASHError{Err: core.ErrNotAvailable}
			}
		}
		http.Error(w, "Live stream unavailable", rec.Code)
		return nil
	}
	prefix := "session/livetv-dash-" + id + "/"
	body := dashURL.ReplaceAllStringFunc(rec.Body.String(), func(attr string) string {
		parts := dashURL.FindStringSubmatch(attr)
		target := strings.Replace(parts[2], prefix, "/livetv/sessions/"+id+"/0/dash/", 1)
		return parts[1] + `="` + html.EscapeString(withAuth(target, ss.streamGrant)) + `"`
	})
	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "HEAD" {
		fmt.Fprint(w, body)
	}
	return nil
}

// ensureDASH returns the session's DASH transcoder, starting it on first
// use. A session closed since it was looked up gets none: close has already
// run and would never stop a transcoder started now.
func (s *Service) ensureDASH(ctx context.Context, ss *session, id string) (core.DASHSession, error) {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, core.ErrNotFound
	}
	ss.consumed = true
	if ss.dash != nil {
		ds := ss.dash
		ss.mu.Unlock()
		return ds, nil
	}
	if ss.info.Channel.Kind == "virtual" {
		if s.o.Transcoder == nil {
			ss.mu.Unlock()
			return nil, &VirtualDASHError{Err: core.ErrNotAvailable}
		}
		gen := ss.generation
		ss.mu.Unlock()
		return s.publishVirtualDASH(ctx, ss, id, gen)
	}
	defer ss.mu.Unlock()
	if ss.info.Channel.Kind != "iptv" || s.o.Transcoder == nil {
		return nil, core.ErrNotAvailable
	}
	info := ss.info.MediaInfo
	if info == nil {
		var err error
		if info, err = s.probeSessionInput(ctx, ss); err != nil {
			return nil, err
		}
		if info == nil {
			return nil, core.ErrNotAvailable
		}
	}
	copy := *info
	info = &copy
	info.Duration = 0
	ss.info.MediaInfo = info
	dec := s.o.Transcoder.Decide(*info, ss.caps, core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1})
	if dec.Kind == core.DecisionDirectPlay {
		dec.Kind = core.DecisionRemux
		dec.VideoCopy = true
		dec.AudioCopy = true
	}
	input, err := s.relayInput(ss)
	if err != nil {
		return nil, err
	}
	hs, err := s.o.Transcoder.StartHLS(s.ctx, core.HLSRequest{SessionID: "livetv-dash-" + id, InputURL: input, LiveInput: true, Info: *info, Decision: dec, Protocol: core.ProtocolDASH, SegmentDuration: segmentDuration})
	if err != nil {
		return nil, err
	}
	ds, ok := hs.(core.DASHSession)
	if !ok {
		hs.Close()
		return nil, core.ErrNotAvailable
	}
	ss.dash = ds
	return ds, nil
}

func (s *Service) ServeDASHPart(w http.ResponseWriter, r *http.Request, id string, u *core.User, rep, n int, init bool) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	ds := ss.dash
	ss.mu.Unlock()
	if ds == nil {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	if !init && r.Method != http.MethodHead {
		s.touchMedia(ss)
	}
	if init {
		ds.ServeInit(w, r, rep)
	} else {
		ds.ServeMedia(w, r, rep, n)
	}
}
