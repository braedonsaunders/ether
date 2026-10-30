package sessions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLazyHLSAuthorizesEveryResourceAndDefersOpen(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	playback := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	transcoder := &fakeMediaTranscoder{sessions: map[string]*fakeMediaSession{}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	var hidden atomic.Bool
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		if hidden.Load() {
			return nil, core.ErrNotFound
		}
		return &core.Title{Key: key}, nil
	}
	networkLookups := 0
	planLookup := func(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (*core.Title, error) {
		networkLookups++
		return lookup(ctx, viewer, device, key)
	}
	bus := NewEventBus(nil, active)
	defer bus.Close()
	broker, err := NewBroker(BrokerDeps{Lookup: planLookup, MediaLookup: lookup, Playback: playback, Transcoder: transcoder, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	grants := &fakeLazyGrants{}
	lazy, err := NewLazyStreams(db, broker, "stremio", grants, time.Hour, func(*http.Request) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	issuer, ok := lazy.(frontend.LazyHLSStreams)
	if !ok {
		t.Fatal("missing HLS issuer")
	}
	plan := frontend.PlanRequest{Viewer: &core.Viewer{ID: 1}, Device: core.Device{ID: "device"}, Protocol: "stremio", Item: core.MovieKey(1), Intent: frontend.PlayIntent{Options: core.DecideOptions{DirectPlay: true, DirectStream: true}}}
	path, err := issuer.IssueHLS(ctx, "account-credential-secret", plan, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil || strings.Contains(path, "account-credential-secret") || playback.prepares != 0 {
		t.Fatalf("issue %q, prepares %d, error %v", path, playback.prepares, err)
	}
	base := strings.TrimSuffix(path, "master.m3u8")
	request := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		lazy.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	if got := request(http.MethodHead, path); got.Code != http.StatusOK || playback.prepares != 0 {
		t.Fatalf("HEAD status %d, prepares %d", got.Code, playback.prepares)
	}
	if got := request(http.MethodGet, base+"base/00000.ts"); got.Code != http.StatusNotFound || playback.prepares != 0 {
		t.Fatalf("orphan status %d, prepares %d", got.Code, playback.prepares)
	}
	master := request(http.MethodGet, path)
	if master.Code != http.StatusOK || !strings.Contains(master.Body.String(), "base/index.m3u8") || strings.Contains(master.Body.String(), "/sessions/media/") {
		t.Fatalf("master %d %q", master.Code, master.Body.String())
	}
	lookupsAfterMaster := networkLookups
	playlist := request(http.MethodGet, base+"base/index.m3u8")
	if playlist.Code != http.StatusOK || !strings.Contains(playlist.Body.String(), "00000.ts") {
		t.Fatalf("playlist %d %q", playlist.Code, playlist.Body.String())
	}
	segment := request(http.MethodGet, base+"base/00000.ts")
	if segment.Code != http.StatusOK || segment.Body.String() != "segment-0" || playback.prepares != 1 || networkLookups != lookupsAfterMaster {
		t.Fatalf("segment %d %q, prepares %d, network lookups %d -> %d", segment.Code, segment.Body.String(), playback.prepares, lookupsAfterMaster, networkLookups)
	}
	hidden.Store(true)
	if got := request(http.MethodGet, base+"base/index.m3u8"); got.Code != http.StatusNotFound {
		t.Fatalf("restricted playlist status %d", got.Code)
	}
	hidden.Store(false)
	grants.mu.Lock()
	grants.revoked = true
	grants.mu.Unlock()
	if got := request(http.MethodGet, base+"base/00000.ts"); got.Code != http.StatusNotFound {
		t.Fatalf("revoked segment status %d", got.Code)
	}
}

func TestHLSEligibilityUsesDeclaredDeliveryAndPermissions(t *testing.T) {
	b := &Broker{deps: BrokerDeps{Transcoder: fakeBrokerTranscoder{}}}
	ps := &core.PlaySession{Info: core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Codec: "hevc"}, {Type: "audio", Index: 1, Codec: "aac"}}}}
	if !knownHLSVideo(ps.Info) || knownHLSVideo(core.MediaInfo{Estimated: true, Streams: ps.Info.Streams}) || knownHLSVideo(core.MediaInfo{Streams: []core.StreamInfo{{Type: "video"}}}) {
		t.Fatal("HLS eligibility did not distinguish known from estimated or missing codec facts")
	}
	profiles := &core.ClientCapabilityProfiles{Transcoding: []core.ClientTranscodeProfile{{MediaType: "video", Delivery: "dash", Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}}}
	caps := core.ClientCaps{Profiles: profiles}
	dec := core.Decision{Kind: core.DecisionTranscode, Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", AudioStream: 1}
	opts := core.DecideOptions{DirectStream: true}
	if _, ok := b.hlsOutputDecision(ps, caps, opts, nil, dec); ok {
		t.Fatal("DASH-only profile advertised HLS")
	}
	profiles.Transcoding[0].Delivery = "hls"
	if _, ok := b.hlsOutputDecision(ps, caps, opts, nil, dec); !ok {
		t.Fatal("declared HLS output rejected")
	}
	denied := false
	if _, ok := b.hlsOutputDecision(ps, caps, opts, &denied, dec); ok {
		t.Fatal("forbidden transcoding advertised HLS")
	}
}
