package sessions

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrailerPreviewPurposeIsolation(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 0}
	progress := &fakeProgress{}
	timeline := 0
	fills := 0
	cold := make(chan struct{})
	path := filepath.Join(t.TempDir(), "trailer.mp4")
	if err := os.WriteFile(path, []byte("preview bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := NewBroker(BrokerDeps{SurfacePolicy: testSessionPolicy, Playback: pb, Progress: progress, ActiveViewer: active, Events: events, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Now: func() time.Time { return now }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) { timeline++ }, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k}, nil
	}, PreviewSource: func(ctx context.Context, _ *core.Viewer, title *core.Title, _ string) (PreviewSource, error) {
		if title.Key == core.MovieKey(99) {
			select {
			case <-cold:
			case <-ctx.Done():
				return PreviewSource{}, ctx.Err()
			}
		}
		fills++
		return PreviewSource{MIMEType: "video/mp4", SourceGeneration: "internal-hash", Open: func() (*os.File, error) { return os.Open(path) }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	req := PreviewRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "device"}, Protocol: "jellyfin", Item: core.MovieKey(1), IdempotencyKey: "focus-1"}
	// Exercise the wrapper used by real adapters too.
	previews := BindSurface(b, "jellyfin").(frontend.TrailerPreviews)
	result, err := previews.TrailerPreview(ctx, req)
	if err != nil || result.State != "ready" {
		t.Fatal(result, err)
	}
	grant := result.Grant
	if grant.Purpose != "preview" || grant.SourceGeneration != 1 || grant.SeekGeneration != 1 {
		t.Fatal("wrong grant purpose/generations", grant)
	}
	retry, err := previews.TrailerPreview(ctx, req)
	if err != nil || retry.Grant.SessionID != grant.SessionID || fills != 1 {
		t.Fatal("idempotent retry changed grant", retry, err)
	}
	access := frontend.SessionAccess{Viewer: req.Viewer, Device: req.Device, Protocol: req.Protocol}
	if err = b.Report(ctx, access, grant.SessionID, frontend.Timeline{Position: time.Minute, Duration: time.Minute, State: "playing"}); err != nil {
		t.Fatal(err)
	}
	if timeline != 0 || progress.calls != 0 || pb.prepares != 0 || pb.estimates != 0 {
		t.Fatal("preview reached library playback/progress", timeline, progress.calls, pb.prepares, pb.estimates)
	}
	serve := func(g *frontend.TrailerPreviewGrant, a *frontend.SessionAccess) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, g.URL, nil)
		if a == nil {
			b.ServeMedia(w, r)
		} else {
			b.ServeMediaAccess(w, r, *a)
		}
		return w.Code
	}
	if code := serve(grant, &access); code != 200 {
		t.Fatal("preview not playable", code)
	}
	wrong := access
	wrong.Viewer = &core.Viewer{ID: 2}
	if code := serve(grant, &wrong); code != 404 {
		t.Fatal("cross-viewer grant accepted", code)
	}
	if err = b.Stop(ctx, wrong, grant.SessionID); err != core.ErrUnauthorized {
		t.Fatal("cross-viewer stop accepted", err)
	}
	req.IdempotencyKey = "focus-2"
	req.Item = core.MovieKey(2)
	replacement, err := previews.TrailerPreview(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if code := serve(grant, nil); code != 404 {
		t.Fatal("replaced grant survived", code)
	}
	list, err := b.Sessions(ctx, access)
	if err != nil || len(list) != 1 || list[0].Purpose != "preview" {
		t.Fatal("preview missing from stop-command scope", list, err)
	}
	if err = b.Command(ctx, req.Viewer, replacement.Grant.SessionID, frontend.Command{Kind: frontend.CommandStop}); err != nil {
		t.Fatal(err)
	}
	if code := serve(replacement.Grant, nil); code != 404 {
		t.Fatal("stopped grant survived", code)
	}
	req.IdempotencyKey = "focus-3"
	expired, err := previews.TrailerPreview(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if code := serve(expired.Grant, nil); code != 404 {
		t.Fatal("expired grant survived", code)
	}
	// Cold focus must return promptly without promising a grant. A fresh poll
	// can issue a grant only after preparation completes and policy is rechecked.
	req.Item, req.IdempotencyKey = core.MovieKey(99), "cold-focus"
	started := time.Now()
	preparing, err := previews.TrailerPreview(ctx, req)
	if err != nil || preparing.State != "preparing" || preparing.Grant != nil || time.Since(started) > time.Second {
		t.Fatal("cold focus blocked or fabricated a grant", preparing, err)
	}
	close(cold)
	req.IdempotencyKey = "cold-poll"
	ready, err := previews.TrailerPreview(ctx, req)
	if err != nil || ready.State != "ready" || ready.Grant == nil {
		t.Fatal("prepared focus did not become ready", ready, err)
	}

}
