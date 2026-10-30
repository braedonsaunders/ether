package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/store"
)

type consumerHLSPlayback struct {
	*fakePlayback
	source *revisionSource
}

func (p *consumerHLSPlayback) PrepareSurface(ctx context.Context, v *core.User, k core.RatingKey, c core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if exact && release != p.source.id {
		return nil, core.ErrSourceChanged
	}
	ps, err := p.fakePlayback.Prepare(ctx, v, k, c)
	if err == nil {
		ps.Source = p.source
		ps.Resolution = &core.Resolution{Release: core.Release{ID: p.source.id}}
		ps.Info = core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Type: "video", Codec: "h264"}, {Type: "audio", Index: 1, Codec: "aac"}}}
	}
	return ps, err
}
func (p *consumerHLSPlayback) PinSourceRevision(_ context.Context, id, revision string) error {
	ps, ok := p.Session(id)
	if !ok || core.SourceRevision(ps) != revision {
		return core.ErrSourceChanged
	}
	return nil
}
func (*consumerHLSPlayback) UnpinSourceRevision(string) {}

type consumerHLSGrants struct {
	protocol string
	revoked  atomic.Bool
}

func (g *consumerHLSGrants) GrantReference(_ context.Context, surface, credential string) (string, *core.Viewer, error) {
	if surface != g.protocol || credential != "account-secret" || g.revoked.Load() {
		return "", nil, core.ErrUnauthorized
	}
	return "grant-reference", &core.Viewer{ID: 1}, nil
}
func (g *consumerHLSGrants) ValidateGrantReference(_ context.Context, surface, ref string, viewer int64) error {
	if surface != g.protocol || ref != "grant-reference" || viewer != 1 || g.revoked.Load() {
		return core.ErrUnauthorized
	}
	return nil
}

// Record the output sent to the encoder, not merely the advertised plan.
type consumerHLSTranscoder struct {
	*fakeMediaTranscoder
	container string
	started   core.HLSRequest
}

func (t *consumerHLSTranscoder) Decide(_ core.MediaInfo, _ core.ClientCaps, opts core.DecideOptions) core.Decision {
	if t.container == "mpegts" && opts.DirectPlay {
		return core.Decision{Kind: core.DecisionDirectPlay, Container: "mp4", VideoCopy: true, AudioCopy: true}
	}
	return core.Decision{Kind: core.DecisionRemux, Container: t.container, VideoCopy: true, AudioCopy: true, VideoCodec: "h264", AudioCodec: "aac", AudioStream: 1}
}
func (t *consumerHLSTranscoder) StartHLS(ctx context.Context, req core.HLSRequest) (core.HLSSession, error) {
	t.started = req
	return t.fakeMediaTranscoder.StartHLS(ctx, req)
}

func TestMediaBrowserResolvedLazyHLSBroker(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby"} {
		for _, container := range []string{"mpegts", "fmp4"} {
			t.Run(protocol+"/"+container, func(t *testing.T) {
				ctx := context.Background()
				db := store.OpenTest(t)
				pb := &consumerHLSPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, source: &revisionSource{Reader: bytes.NewReader(nil), id: "private-release", name: "movie.mp4", size: 100}}
				tc := &consumerHLSTranscoder{fakeMediaTranscoder: &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}, container: container}
				active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
				lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, k core.RatingKey) (*core.Title, error) {
					return &core.Title{Key: k, Type: "movie"}, nil
				}
				bus := NewEventBus(nil, active)
				defer bus.Close()
				b, err := NewBroker(BrokerDeps{DB: db, Events: bus, Playback: pb, Transcoder: tc, Lookup: lookup, MediaLookup: lookup, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, Progress: &fakeProgress{}, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}})
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close(ctx)
				grants := &consumerHLSGrants{protocol: protocol}
				lazy, err := NewLazyStreams(db, b, frontend.Protocol(protocol), grants, time.Hour, func(*http.Request) bool { return false })
				if err != nil {
					t.Fatal(err)
				}
				host := frontend.Host{Sessions: BindSurface(b, frontend.Protocol(protocol)), LazyStreams: lazy, Capabilities: frontendtest.Capabilities{}, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "https://ether.example", nil }, Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*core.Viewer, error) { return &core.Viewer{ID: 1}, nil }}, Experience: frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
					return frontend.ItemDetail{Title: core.Title{Key: 5, Type: "movie"}}, nil
				}}}
				var front frontend.Frontend = mediabrowser.Jellyfin{MachineID: "test"}
				if protocol == "emby" {
					front = mediabrowser.Emby{MachineID: "test"}
				}
				inst, err := front.Mount(ctx, host)
				if err != nil {
					t.Fatal(err)
				}
				call := func(method, path string) *httptest.ResponseRecorder {
					r := httptest.NewRequest(method, path, nil)
					r.Header.Set("Authorization", `MediaBrowser Token="account-secret", DeviceId="device"`)
					w := httptest.NewRecorder()
					inst.Handler().ServeHTTP(w, r)
					return w
				}
				detail := call("GET", "/Items/"+ids.RatingKeyGUID(5))
				var dto struct {
					MediaSources []struct{ Id, TranscodingUrl, TranscodingContainer string }
				}
				if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &dto) != nil || len(dto.MediaSources) != 1 || dto.MediaSources[0].TranscodingUrl == "" {
					t.Fatal(detail.Code, detail.Body.String())
				}
				if strings.Contains(detail.Body.String(), "account-secret") || strings.Contains(detail.Body.String(), "private-release") || len(b.sessions) != 0 || len(tc.sessions) != 0 {
					t.Fatal("detail opened encoder or leaked authority", detail.Body.String())
				}
				wantContainer := "ts"
				if container == "fmp4" {
					wantContainer = "mp4"
				}
				if dto.MediaSources[0].TranscodingContainer != wantContainer {
					t.Fatal("container metadata", dto)
				}
				path := strings.TrimPrefix(dto.MediaSources[0].TranscodingUrl, "https://ether.example")
				if w := call("HEAD", path); w.Code != 200 || len(b.sessions) != 0 {
					t.Fatal("HEAD opened", w.Code)
				}
				base := strings.TrimSuffix(path, "master.m3u8")
				if w := call("GET", base+"base/00000.ts"); w.Code != 404 {
					t.Fatal("orphan", w.Code)
				}
				if w := call("GET", path); w.Code != 200 || !strings.Contains(w.Body.String(), "base/index.m3u8") || strings.Contains(w.Body.String(), "/sessions/media/") {
					t.Fatal("master", w.Code, w.Body.String())
				}
				if tc.started.Decision.Container != container {
					t.Fatal("advertised container differs from encoder", tc.started.Decision)
				}
				for _, s := range b.sessions {
					if err := b.MatchDetailSession(ctx, frontend.VersionScope{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "device"}, Protocol: frontend.Protocol(protocol)}, dto.MediaSources[0].Id, s.public.ID); err != nil {
						t.Fatal("source binding", err)
					}
				}
				if w := call("GET", base+"base/00000.ts"); w.Code != 200 || w.Body.String() != "segment-0" {
					t.Fatal("segment", w.Code, w.Body.String())
				}
				grants.revoked.Store(true)
				if w := call("GET", base+"base/00000.ts"); w.Code != 404 {
					t.Fatal("revoked", w.Code)
				}
				grants.revoked.Store(false)
				pb.source.size++
				if w := call("GET", base+"base/00000.ts"); w.Code != 404 {
					t.Fatal("changed source", w.Code, w.Body.String())
				}
			})
		}
	}
}
