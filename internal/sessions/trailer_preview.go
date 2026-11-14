package sessions

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type PreviewRequest = frontend.TrailerPreviewRequest
type PreviewResult = frontend.TrailerPreviewResult

// PreviewSource is already cached. Opening it never invokes a resolver.
type PreviewSource struct {
	MIMEType, SourceGeneration string
	Open                       func() (*os.File, error)
}

var ErrPreviewPreparing = errors.New("preview preparing")

type previewSession struct {
	lifetime   context.Context
	cancel     context.CancelFunc
	public     frontend.Session
	grant      frontend.TrailerPreviewGrant
	token, key string
	caps       core.ClientCaps
	source     PreviewSource
}

// TrailerPreview has a separate lightweight allowance: one per viewer, at most
// 128 grants and 4 pending requests per broker. It never reserves feature-film
// stream/encoder slots, warms releases, or calls progress/history callbacks.
// Work follows the request context; preparing means retry, not background work.
func (b *Broker) TrailerPreview(ctx context.Context, req PreviewRequest) (PreviewResult, error) {
	unavailable := PreviewResult{State: "unavailable", Reason: "trailer_unavailable"}
	if req.Protocol == "" || req.Device.ID == "" || len(req.Device.ID) > 256 || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 256 || req.Item == 0 {
		return unavailable, core.ErrUnauthorized
	}
	viewer, err := b.viewer(ctx, req.Viewer)
	if err != nil {
		return unavailable, err
	}
	viewer, err = b.confirm(ctx, viewer, req.Caps)
	if err != nil {
		return unavailable, err
	}
	title, err := b.deps.Lookup(ctx, viewer, req.Device, req.Item)
	if err != nil {
		return unavailable, err
	}
	if title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) || b.deps.PreviewSource == nil {
		return unavailable, nil
	}
	b.mu.Lock()
	if b.previews == nil {
		b.previews = make(map[string]previewSession)
		b.previewPending = make(map[int64]bool)
	}
	b.prunePreviewsLocked()
	if b.closed {
		b.mu.Unlock()
		return unavailable, core.ErrNotAvailable
	}
	for _, s := range b.previews {
		if s.public.ViewerID == viewer.ID && s.public.Device.ID == req.Device.ID && s.public.Protocol == req.Protocol && s.public.Item == req.Item && s.key == req.IdempotencyKey {

			// A ready response promises bytes still exist for this generation.
			file, openErr := s.source.Open()
			if openErr == nil {
				file.Close()
				g := s.grant
				b.mu.Unlock()
				return PreviewResult{State: "ready", Grant: &g}, nil
			}
			s.cancel()
			delete(b.previews, s.public.ID)

		}
	}
	if b.previewPending[viewer.ID] || len(b.previewPending) >= 4 {
		b.mu.Unlock()
		return PreviewResult{State: "preparing", RetryAfterSeconds: 1}, nil
	}
	b.previewPending[viewer.ID] = true
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.previewPending, viewer.ID); b.mu.Unlock() }()
	source, err := b.deps.PreviewSource(ctx, viewer, title, req.Language)
	if errors.Is(err, ErrPreviewPreparing) {
		return PreviewResult{State: "preparing", RetryAfterSeconds: 1}, nil
	}
	if err != nil || source.Open == nil || source.SourceGeneration == "" {
		if ctx.Err() != nil {
			return unavailable, ctx.Err()
		}
		return unavailable, nil
	}
	// Reconfirm membership and playback policy after a potentially slow download.
	viewer, err = b.viewer(ctx, viewer)
	if err != nil {
		return unavailable, err
	}
	viewer, err = b.confirm(ctx, viewer, req.Caps)
	if err != nil {
		return unavailable, err
	}
	title, err = b.deps.Lookup(ctx, viewer, req.Device, req.Item)
	if err != nil {
		return unavailable, err
	}
	if title == nil || title.Key != req.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		return unavailable, nil
	}
	if err = ctx.Err(); err != nil {
		return unavailable, err
	}
	id, token := rand.Text(), rand.Text()
	grant := frontend.TrailerPreviewGrant{SessionID: id, URL: "/sessions/media/" + id + "/" + token + "/preview.mp4", MIMEType: source.MIMEType, ExpiresAt: b.deps.Now().Add(2 * time.Minute), Purpose: "preview", SourceGeneration: 1, SeekGeneration: 1}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return unavailable, core.ErrNotAvailable
	}
	b.prunePreviewsLocked()
	for sid, s := range b.previews {
		if s.public.ViewerID == viewer.ID {
			s.cancel()
			delete(b.previews, sid)
		}
	}
	if len(b.previews) >= 128 {
		return PreviewResult{State: "preparing", RetryAfterSeconds: 1}, nil
	}
	lifetime, cancel := context.WithCancel(context.Background())
	b.previews[id] = previewSession{lifetime: lifetime, cancel: cancel, public: frontend.Session{ID: id, MediaPath: grant.URL, Protocol: req.Protocol, ViewerID: viewer.ID, Device: req.Device, Item: req.Item, StartedAt: b.deps.Now(), PlayMethod: "DirectPlay", Purpose: "preview"}, grant: grant, token: token, key: req.IdempotencyKey, caps: req.Caps, source: source}
	return PreviewResult{State: "ready", Grant: &grant}, nil
}
func (b *Broker) prunePreviewsLocked() {
	for id, s := range b.previews {
		if !b.deps.Now().Before(s.grant.ExpiresAt) {
			s.cancel()
			delete(b.previews, id)
		}
	}
}

func (b *Broker) previewControl(ctx context.Context, a frontend.SessionAccess, id string, stop bool) (bool, error) {
	b.mu.Lock()
	s, ok := b.previews[id]
	b.mu.Unlock()
	if !ok {
		return false, nil
	}
	v, err := b.viewer(ctx, a.Viewer)
	if err != nil {
		return true, err
	}
	owner := a.Owner && v.IsOwner && !v.Delegated
	if !owner && (a.Owner || v.ID != s.public.ViewerID || a.Protocol != s.public.Protocol || a.Device.ID == "" || a.Device.ID != s.public.Device.ID) {
		return true, core.ErrUnauthorized
	}
	if stop {
		b.mu.Lock()
		s.cancel()
		delete(b.previews, id)
		b.mu.Unlock()
	}
	return true, nil
}
func (b *Broker) servePreview(protocol frontend.Protocol, access *frontend.SessionAccess, w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/sessions/media/"), "/")
	if len(parts) != 3 {
		return false
	}
	b.mu.Lock()
	s, ok := b.previews[parts[0]]
	b.mu.Unlock()
	if !ok {
		return false
	}
	deny := func() { http.NotFound(w, r) }
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || parts[2] != "preview.mp4" || !b.deps.Now().Before(s.grant.ExpiresAt) || subtle.ConstantTimeCompare([]byte(parts[1]), []byte(s.token)) != 1 || (protocol != "" && protocol != s.public.Protocol) {
		deny()
		return true
	}
	if access != nil && (access.Owner || access.Viewer == nil || access.Viewer.ID != s.public.ViewerID || access.Protocol != s.public.Protocol || access.Device.ID != s.public.Device.ID) {
		deny()
		return true
	}
	viewer, err := b.viewer(r.Context(), &core.Viewer{ID: s.public.ViewerID})
	if err != nil {
		deny()
		return true
	}
	if viewer, err = b.confirm(r.Context(), viewer, s.caps); err != nil {
		deny()
		return true
	}
	lookup := b.deps.MediaLookup
	if lookup == nil {
		lookup = b.deps.Lookup
	}
	title, err := lookup(r.Context(), viewer, s.public.Device, s.public.Item)
	if err != nil || title == nil || title.Key != s.public.Item || (!title.Released.IsZero() && title.Released.After(b.deps.Now())) {
		deny()
		return true
	}
	file, err := s.source.Open()
	if err != nil {
		deny()
		return true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		deny()
		return true
	}
	b.mu.Lock()
	_, current := b.previews[parts[0]]
	b.mu.Unlock()
	if !current {
		deny()
		return true
	}
	ctx, cancel := context.WithDeadline(r.Context(), s.grant.ExpiresAt)
	defer cancel()
	done := context.AfterFunc(s.lifetime, cancel)
	defer done()
	w.Header().Set("Content-Type", s.grant.MIMEType)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r.WithContext(ctx), "preview.mp4", info.ModTime(), previewReadSeeker{File: file, ctx: ctx})
	return true
}

func (s surface) TrailerPreview(ctx context.Context, r frontend.TrailerPreviewRequest) (frontend.TrailerPreviewResult, error) {
	if r.Protocol != s.protocol {
		return frontend.TrailerPreviewResult{}, core.ErrUnauthorized
	}
	if p, ok := s.broker.(frontend.TrailerPreviews); ok {
		return p.TrailerPreview(ctx, r)
	}
	return frontend.TrailerPreviewResult{State: "unavailable", Reason: "unsupported"}, nil
}

type previewReadSeeker struct {
	*os.File
	ctx context.Context
}

func (f previewReadSeeker) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}
