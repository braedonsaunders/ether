package spike

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// localSource is a core.MediaSource over a local file.
type localSource struct {
	f    *os.File
	size int64
	name string
	id   string
}

func (s *localSource) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }
func (s *localSource) Size() int64                             { return s.size }
func (s *localSource) Name() string                            { return s.name }
func (s *localSource) ReleaseID() string                       { return s.id }
func (s *localSource) LocalFile() *os.File                     { return s.f }
func (s *localSource) Close() error                            { return s.f.Close() }

type session struct {
	ps     *core.PlaySession
	file   *localFile
	src    *localSource
	active time.Time
}

// playback implements core.Playback for local files: direct play from disk.
type playback struct {
	lib *Library

	mu       sync.Mutex
	sessions map[string]*session
	now      func() time.Time
}

func newPlayback(l *Library) *playback {
	return &playback{lib: l, sessions: map[string]*session{}, now: time.Now}
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p *playback) Estimate(_ context.Context, t *core.Title, _ core.ClientCaps) core.MediaInfo {
	if f, ok := p.lib.file(t.Key); ok {
		return f.info
	}
	return core.MediaInfo{Estimated: true, Duration: t.Runtime}
}

func (p *playback) Prepare(_ context.Context, u *core.User, k core.RatingKey, caps core.ClientCaps) (*core.PlaySession, error) {
	if u == nil {
		return nil, core.ErrUnauthorized
	}
	f, ok := p.lib.file(k)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	active := 0
	for _, s := range p.sessions {
		if s.ps.UserID == u.ID && s.ps.ClientID == caps.ClientID && s.ps.Key == k {
			s.active = p.now()
			return s.ps, nil
		}
		if s.ps.UserID == u.ID {
			active++
		}
	}
	limit := u.StreamLimit
	if limit == 0 {
		limit = p.lib.cfg().Server.DefaultStreamLimit
	}
	if limit > 0 && active >= limit {
		return nil, core.ErrStreamLimit
	}
	fh, err := os.Open(f.path)
	if err != nil {
		return nil, core.ErrNotAvailable
	}
	src := &localSource{f: fh, size: f.size, name: filepath.Base(f.path), id: "local:" + k.String()}
	ps := &core.PlaySession{
		ID:       newID(),
		UserID:   u.ID,
		ClientID: caps.ClientID,
		Key:      k,
		Resolution: &core.Resolution{
			Title:      k,
			Release:    core.Release{ID: src.id, Name: src.name, Size: f.size, Indexer: "local"},
			ResolvedAt: p.now(),
		},
		Source:    src,
		Info:      f.info,
		StartedAt: p.now(),
	}
	p.sessions[ps.ID] = &session{ps: ps, file: f, src: src, active: p.now()}
	return ps, nil
}

func (p *playback) Session(id string) (*core.PlaySession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		return nil, false
	}
	return s.ps, true
}

func (p *playback) SessionFor(userID int64, clientID string, k core.RatingKey) (*core.PlaySession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		if s.ps.UserID == userID && s.ps.ClientID == clientID && s.ps.Key == k {
			return s.ps, true
		}
	}
	return nil, false
}

// ServeRange serves the file with full Range support.
func (p *playback) ServeRange(w http.ResponseWriter, r *http.Request, id string) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	if ok {
		s.active = p.now()
	}
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(s.src.name))
	switch filepath.Ext(s.src.name) {
	case ".mkv":
		ct = "video/x-matroska"
	case ".ts", ".m2ts":
		ct = "video/mp2t"
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, s.src.name, s.file.mtime, io.NewSectionReader(s.src, 0, s.src.size))
}

// InputURL is the local path; ffmpeg reads files directly.
func (p *playback) InputURL(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		return s.file.path
	}
	return ""
}

func (p *playback) Failover(context.Context, string, time.Duration) (*core.PlaySession, error) {
	return nil, core.ErrNotAvailable // a local file has no alternates
}

func (p *playback) Stop(id string) {
	p.mu.Lock()
	s, ok := p.sessions[id]
	delete(p.sessions, id)
	p.mu.Unlock()
	if ok {
		_ = s.src.Close()
	}
}

func (p *playback) Sessions() []*core.PlaySession {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*core.PlaySession, 0, len(p.sessions))
	for _, s := range p.sessions {
		out = append(out, s.ps)
	}
	return out
}

// Touch keeps a session alive (plex.SessionToucher).
func (p *playback) Touch(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.active = p.now()
	}
}

func (p *playback) expire(now time.Time, idle time.Duration) {
	p.mu.Lock()
	var stale []*session
	for id, s := range p.sessions {
		if now.Sub(s.active) > idle {
			stale = append(stale, s)
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	for _, s := range stale {
		_ = s.src.Close()
	}
}
